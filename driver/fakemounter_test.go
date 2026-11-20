/*
Copyright 2026 Nikolay Matrosov

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package driver

import (
	"os"
	"path/filepath"
	"sync"
)

// fakeMounter keeps mounts in memory; it makes sanity and the node unit tests independent of mount, mkfs and /dev.
type fakeMounter struct {
	mu        sync.Mutex
	mounted   map[string]bool
	formatted map[string]bool
}

var _ Mounter = (*fakeMounter)(nil)

func newFakeMounter() *fakeMounter {
	return &fakeMounter{mounted: map[string]bool{}, formatted: map[string]bool{}}
}
func (m *fakeMounter) Format(s, _ string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.formatted[s] = true
	return nil
}
func (m *fakeMounter) Mount(_, t, fsType string, _ ...string) error {
	// like the real mounter: a file for raw block, a directory for a filesystem
	if fsType == "" {
		if err := os.MkdirAll(filepath.Dir(t), 0o750); err != nil {
			return err
		}
		f, err := os.OpenFile(t, os.O_CREATE|os.O_RDONLY, 0o660)
		if err != nil {
			return err
		}
		_ = f.Close()
	} else if err := os.MkdirAll(t, 0o750); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.mounted[t] = true
	return nil
}
func (m *fakeMounter) Unmount(t string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.mounted, t)
	return nil
}
func (m *fakeMounter) IsFormatted(s string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.formatted[s], nil
}
func (m *fakeMounter) IsMounted(t string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.mounted[t], nil
}
func (m *fakeMounter) GetStatistics(string) (volumeStatistics, error) {
	return volumeStatistics{availableBytes: 1, totalBytes: 2, usedBytes: 1, availableInodes: 1, totalInodes: 2, usedInodes: 1}, nil
}
func (m *fakeMounter) IsBlockDevice(string) (bool, error) { return false, nil }

func (m *fakeMounter) ResizeIfNeeded(_, _ string) error     { return nil }
func (m *fakeMounter) DeviceOfMount(string) (string, error) { return "/dev/fake", nil }
func (m *fakeMounter) FindDevice(id string) (string, error) { return "/dev/fake-" + id, nil }
