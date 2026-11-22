// Package licenses leases Windows product keys from the node's license pools
// (implements driver.Licenses). Keys never leave the node; leases are
// persisted so a key stays bound to its device across agent restarts.
package licenses

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"

	agentv1 "github.com/finn-freitag/tb-proto/gen/agent/v1"

	"github.com/finn-freitag/tb-agent/internal/config"
)

type lease struct {
	Pool  string `json:"pool"`
	Index int    `json:"index"`
}

// Manager implements driver.Licenses.
type Manager struct {
	mu     sync.Mutex
	pools  map[string]config.LicensePool
	leases map[string]lease // device id -> lease
	file   string
}

// New loads leases from <dataDir>/licenses.json.
func New(pools map[string]config.LicensePool, dataDir string) (*Manager, error) {
	m := &Manager{pools: pools, leases: map[string]lease{}, file: filepath.Join(dataDir, "licenses.json")}
	b, err := os.ReadFile(m.file)
	switch {
	case err == nil:
		if err := json.Unmarshal(b, &m.leases); err != nil {
			return nil, fmt.Errorf("licenses: %s: %w", m.file, err)
		}
	case !errors.Is(err, os.ErrNotExist):
		return nil, err
	}
	// Drop leases whose pool or key disappeared from the config.
	for id, l := range m.leases {
		if p, ok := pools[l.Pool]; !ok || l.Index >= len(p.Keys) {
			delete(m.leases, id)
		}
	}
	return m, nil
}

// Resolve returns the product key for a device ("" for none).
func (m *Manager) Resolve(deviceID string, spec *agentv1.LicenseSpec) (string, error) {
	if spec == nil {
		return "", nil
	}
	switch spec.Mode {
	case agentv1.LicenseMode_LICENSE_MODE_UNSPECIFIED, agentv1.LicenseMode_LICENSE_MODE_NONE:
		return "", nil
	case agentv1.LicenseMode_LICENSE_MODE_USER_KEY:
		if spec.UserKey == "" {
			return "", errors.New("licenses: user key mode without key")
		}
		return spec.UserKey, nil
	case agentv1.LicenseMode_LICENSE_MODE_POOL:
	default:
		return "", fmt.Errorf("licenses: unknown mode %v", spec.Mode)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	pool, ok := m.pools[spec.Pool]
	if !ok {
		return "", fmt.Errorf("licenses: pool %q is not configured on this node", spec.Pool)
	}
	if l, ok := m.leases[deviceID]; ok && l.Pool == spec.Pool {
		return pool.Keys[l.Index], nil
	}
	used := map[int]bool{}
	for _, l := range m.leases {
		if l.Pool == spec.Pool {
			used[l.Index] = true
		}
	}
	for i := range pool.Keys {
		if !used[i] {
			m.leases[deviceID] = lease{Pool: spec.Pool, Index: i}
			if err := m.save(); err != nil {
				delete(m.leases, deviceID)
				return "", err
			}
			return pool.Keys[i], nil
		}
	}
	return "", fmt.Errorf("licenses: no free key in pool %q", spec.Pool)
}

// Release frees a device's leased key.
func (m *Manager) Release(deviceID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.leases[deviceID]; ok {
		delete(m.leases, deviceID)
		_ = m.save()
	}
}

// Pools reports pool names, types and usage (never the keys).
func (m *Manager) Pools() []*agentv1.LicensePoolInfo {
	m.mu.Lock()
	defer m.mu.Unlock()
	inUse := map[string]int32{}
	for _, l := range m.leases {
		inUse[l.Pool]++
	}
	out := make([]*agentv1.LicensePoolInfo, 0, len(m.pools))
	for name, p := range m.pools {
		out = append(out, &agentv1.LicensePoolInfo{Name: name, Type: p.Type, Total: int32(len(p.Keys)), InUse: inUse[name]})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (m *Manager) save() error {
	b, err := json.MarshalIndent(m.leases, "", "  ")
	if err != nil {
		return err
	}
	tmp := m.file + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, m.file)
}
