package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// versionStore tracks, per device_uid, the highest desired_version the agent has
// applied. It is the value echoed back on the state topic so the cloud can
// declare convergence (reported_version == desired_version). Persisted to /data
// so a reboot does not re-report version 0 and appear to diverge.
type versionStore struct {
	mu      sync.Mutex
	path    string
	applied map[string]int
}

func newVersionStore(dataDir string) *versionStore {
	s := &versionStore{
		path:    filepath.Join(dataDir, "applied-versions.json"),
		applied: make(map[string]int),
	}

	if raw, err := os.ReadFile(s.path); err == nil {
		_ = json.Unmarshal(raw, &s.applied)
	}

	return s
}

// get returns the applied version for a device and whether one was ever set.
func (s *versionStore) get(deviceUID string) (int, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	v, ok := s.applied[deviceUID]

	return v, ok
}

// set records a newly-applied version (monotonic: never lowers) and persists.
func (s *versionStore) set(deviceUID string, version int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if current, ok := s.applied[deviceUID]; ok && version <= current {
		return
	}

	s.applied[deviceUID] = version
	s.persist()
}

// persist writes the map atomically. Caller holds the lock.
func (s *versionStore) persist() {
	raw, err := json.MarshalIndent(s.applied, "", "  ")
	if err != nil {
		return
	}

	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return
	}

	_ = os.Rename(tmp, s.path)
}
