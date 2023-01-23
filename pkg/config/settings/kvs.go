// Copyright (c) 2022 Proton AG
//
// This file is part of Proton Mail Bridge.
//
// Proton Mail Bridge is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// Proton Mail Bridge is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with Proton Mail Bridge. If not, see <https://www.gnu.org/licenses/>.

package settings

import (
	"io/ioutil"
	"strconv"
	"sync"

	"github.com/ghodss/yaml"
	"github.com/sirupsen/logrus"
)

type keyValueStore struct {
	cache map[string]string
	path  string
	lock  *sync.RWMutex
}

// newKeyValueStore returns loaded preferences.
func newKeyValueStore(path string) *keyValueStore {
	p := &keyValueStore{
		path: path,
		lock: &sync.RWMutex{},
	}
	if err := p.load(); err != nil {
		logrus.WithError(err).Warn("Cannot load preferences file, using defaults")
	}
	return p
}

func (p *keyValueStore) load() error {
	if p.cache != nil {
		return nil
	}

	p.lock.Lock()
	defer p.lock.Unlock()

	p.cache = map[string]string{}

	data, err := ioutil.ReadFile(p.path)
	if err != nil {
		return err
	}

	if len(data) == 0 {
		return nil
	}

	return yaml.Unmarshal(data, &p.cache)
}

func (p *keyValueStore) setDefault(key, value string) {
	if p.Get(key) == "" {
		p.set(key, value)
	}
}

func (p *keyValueStore) Get(key string) string {
	p.lock.RLock()
	defer p.lock.RUnlock()

	return p.cache[key]
}

func (p *keyValueStore) GetBool(key string) bool {
	return p.Get(key) == "true"
}

func (p *keyValueStore) GetInt(key string) int {
	if p.Get(key) == "" {
		return 0
	}

	value, err := strconv.Atoi(p.Get(key))
	if err != nil {
		logrus.WithError(err).Error("Cannot parse int")
	}

	return value
}

func (p *keyValueStore) GetFloat64(key string) float64 {
	if p.Get(key) == "" {
		return 0
	}

	value, err := strconv.ParseFloat(p.Get(key), 64)
	if err != nil {
		logrus.WithError(err).Error("Cannot parse float64")
	}

	return value
}

func (p *keyValueStore) set(key, value string) {
	p.lock.Lock()
	p.cache[key] = value
	p.lock.Unlock()
}
