package driver

import "sync"

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
func (m *fakeMounter) Mount(_, t, _ string, _ ...string) error {
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
