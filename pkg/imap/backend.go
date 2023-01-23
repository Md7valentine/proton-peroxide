// Copyright (c) 2022 Proton Technologies AG
//
// This file is part of ProtonMail Bridge.
//
// ProtonMail Bridge is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// ProtonMail Bridge is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with ProtonMail Bridge.  If not, see <https://www.gnu.org/licenses/>.

// Package imap provides IMAP server of the Bridge.
//
// Methods are called by the go-imap library in parallel.
// Additional parallelism is achieved while handling each IMAP request.
//
// For example, ListMessages internally uses `fetchWorkers` workers to resolve each requested item.
// When IMAP clients request message literals (or parts thereof), we sometimes need to build RFC822 message literals.
// To do this, we pass build jobs to the message builder, which internally manages its own parallelism.
// Summary:
//   - each IMAP fetch request is handled in parallel,
//   - within each IMAP fetch request, individual items are handled by a pool of `fetchWorkers` workers,
//   - within each worker, build jobs are posted to the message builder,
//   - the message builder handles build jobs using its own, independent worker pool,
//
// The builder will handle jobs in parallel up to its own internal limit. This prevents it from overwhelming API.
package imap

import (
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/emersion/go-imap"
	goIMAPBackend "github.com/emersion/go-imap/backend"
	"github.com/ljanyst/peroxide/pkg/config/settings"
	"github.com/ljanyst/peroxide/pkg/events"
	"github.com/ljanyst/peroxide/pkg/listener"
	"github.com/ljanyst/peroxide/pkg/users"
)

type imapBackend struct {
	usersMgr         *users.Users
	updates          *imapUpdates
	eventListener    listener.Listener
	listWorkers      int
	bccSelf          bool
	isAllMailVisible bool

	users       map[string]*imapUser
	usersLocker sync.Locker

	imapCache     map[string]map[string]string
	imapCachePath string
	imapCacheLock *sync.RWMutex
}

// NewIMAPBackend returns struct implementing go-imap/backend interface.
func NewIMAPBackend(
	eventListener listener.Listener,
	setting *settings.Settings,
	users *users.Users,
	bccSelf bool,
	isAllMailVisible bool,
) *imapBackend { //nolint[golint]

	imapWorkers := setting.GetInt(settings.IMAPWorkers)
	cacheDir := setting.Get(settings.CacheDir)

	backend := &imapBackend{
		usersMgr:      users,
		updates:       newIMAPUpdates(),
		eventListener: eventListener,

		users:       map[string]*imapUser{},
		usersLocker: &sync.Mutex{},

		imapCachePath: filepath.Join(cacheDir, "imap_backend_cache.json"),
		imapCacheLock: &sync.RWMutex{},
		listWorkers:   imapWorkers,

		bccSelf:          bccSelf,
		isAllMailVisible: isAllMailVisible,
	}

	go backend.monitorDisconnectedUsers()

	return backend
}

func (ib *imapBackend) getUser(address, slot, password string) (*imapUser, error) {
	ib.usersLocker.Lock()
	defer ib.usersLocker.Unlock()

	address = strings.ToLower(address)
	imapUser, ok := ib.users[address]
	if ok {
		return imapUser, nil
	}
	return ib.createUser(address, slot, password)
}

// createUser require that address MUST be in lowercase.
func (ib *imapBackend) createUser(address, slot, password string) (*imapUser, error) {
	log.WithField("address", address).Debug("Creating new IMAP user")

	user, err := ib.usersMgr.GetUser(address)
	if err != nil {
		return nil, err
	}

	if err := user.BringOnline(slot, password); err != nil {
		return nil, err
	}

	// Make sure you return the same user for all valid addresses when in combined mode.
	address = strings.ToLower(user.GetPrimaryAddress())
	if combinedUser, ok := ib.users[address]; ok {
		return combinedUser, nil
	}

	// Client can log in only using address so we can properly close all IMAP connections.
	var addressID string
	if addressID, err = user.GetAddressID(address); err != nil {
		return nil, err
	}

	newUser, err := newIMAPUser(ib, user, addressID, address)
	if err != nil {
		return nil, err
	}

	ib.users[address] = newUser

	return newUser, nil
}

// deleteUser removes a user from the users map.
// This is a safe operation even if the user doesn't exist so it is no problem if it is done twice.
func (ib *imapBackend) deleteUser(address string) {
	log.WithField("address", address).Debug("Deleting IMAP user")

	ib.usersLocker.Lock()
	defer ib.usersLocker.Unlock()

	delete(ib.users, strings.ToLower(address))
}

// Login authenticates a user.
func (ib *imapBackend) Login(_ *imap.ConnInfo, username, password string) (goIMAPBackend.User, error) {

	username, slot := users.DecodeLogin(username)

	imapUser, err := ib.getUser(username, slot, password)
	if err != nil {
		log.WithError(err).Warn("Cannot get user")
		return nil, err
	}

	if err := imapUser.user.CheckCredentials(slot, password); err != nil {
		log.WithError(err).Errorf("Could not check bridge password: %s %s", username, slot)
		if err := imapUser.Logout(); err != nil {
			log.WithError(err).Warn("Could not logout user after unsuccessful login check")
		}
		// Apple Mail sometimes generates a lot of requests very quickly.
		// It's therefore good to have a timeout after a bad login so that we can slow
		// those requests down a little bit.
		time.Sleep(10 * time.Second)
		return nil, err
	}

	// The update channel should be nil until we try to login to IMAP for the first time
	// so that it doesn't make bridge slow for users who are only using bridge for SMTP
	// (otherwise the store will be locked for 1 sec per email during synchronization).
	if store := imapUser.user.GetStore(); store != nil {
		store.SetChangeNotifier(ib.updates)
	}

	return imapUser, nil
}

// Updates returns a channel of updates for IMAP IDLE extension.
func (ib *imapBackend) Updates() <-chan goIMAPBackend.Update {
	return ib.updates.chout
}

func (ib *imapBackend) CreateMessageLimit() *uint32 {
	return nil
}

// monitorDisconnectedUsers removes users when it receives a close connection event for them.
func (ib *imapBackend) monitorDisconnectedUsers() {
	ch := make(chan string)
	ib.eventListener.Add(events.CloseConnectionEvent, ch)

	for address := range ch {
		// delete the user to ensure future imap login attempts use the latest bridge user
		// (bridge user might be removed-readded so we want to use the new bridge user object).
		ib.deleteUser(address)
	}
}
