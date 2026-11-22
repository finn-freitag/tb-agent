// Package gpu hands out the node's GPU to devices: a shared render node
// (containers, VirGL VMs) and SR-IOV virtual functions (one per VM).
package gpu

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Config is the node config section "gpu".
//
//	gpu:
//	  render_node: /dev/dri/renderD128   # default: first /dev/dri/renderD* found
//	  sriov:
//	    pf: "0000:00:02.0"               # virtual functions of this device are handed out
//	    vfs: []                          # or an explicit list of VF PCI addresses
type Config struct {
	RenderNode string      `yaml:"render_node"`
	SRIOV      SRIOVConfig `yaml:"sriov"`
}

// SRIOVConfig selects the virtual functions.
type SRIOVConfig struct {
	PF  string   `yaml:"pf"`
	VFs []string `yaml:"vfs"`
}

var pciRe = regexp.MustCompile(`^[0-9a-f]{4}:[0-9a-f]{2}:[0-9a-f]{2}\.[0-7]$`)

// ValidPCI reports whether s is a PCI address like 0000:00:02.1.
func ValidPCI(s string) bool { return pciRe.MatchString(s) }

// Manager implements driver.GPU.
type Manager struct {
	cfg   Config
	sysfs string // /sys (overridable in tests)
	dev   string // /dev
	path  string // lease file

	mu     sync.Mutex
	leases map[string]string // device ID -> VF address
}

// New creates the manager; leases are persisted in dataDir.
func New(cfg Config, dataDir string) (*Manager, error) {
	m := &Manager{cfg: cfg, sysfs: "/sys", dev: "/dev", path: filepath.Join(dataDir, "gpu-leases.json"), leases: map[string]string{}}
	b, err := os.ReadFile(m.path)
	if err == nil {
		if err := json.Unmarshal(b, &m.leases); err != nil {
			return nil, fmt.Errorf("gpu: %s: %w", m.path, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return m, nil
}

// RenderNode returns the shared render node, "" if the node has none.
func (m *Manager) RenderNode() string {
	if m == nil {
		return ""
	}
	if m.cfg.RenderNode != "" {
		if _, err := os.Stat(m.cfg.RenderNode); err == nil {
			return m.cfg.RenderNode
		}
		return ""
	}
	nodes, _ := filepath.Glob(filepath.Join(m.dev, "dri", "renderD*"))
	sort.Strings(nodes)
	if len(nodes) == 0 {
		return ""
	}
	return nodes[0]
}

// VFs lists the configured virtual functions (explicit list, or the
// virtfn* links of the physical function).
func (m *Manager) VFs() []string {
	if m == nil {
		return nil
	}
	if len(m.cfg.SRIOV.VFs) > 0 {
		return m.cfg.SRIOV.VFs
	}
	if m.cfg.SRIOV.PF == "" {
		return nil
	}
	links, _ := filepath.Glob(filepath.Join(m.sysfs, "bus", "pci", "devices", m.cfg.SRIOV.PF, "virtfn*"))
	type vf struct {
		n    int
		addr string
	}
	var vfs []vf
	for _, l := range links {
		n, err := strconv.Atoi(strings.TrimPrefix(filepath.Base(l), "virtfn"))
		if err != nil {
			continue
		}
		t, err := os.Readlink(l)
		if err != nil {
			continue
		}
		if a := filepath.Base(t); ValidPCI(a) {
			vfs = append(vfs, vf{n, a})
		}
	}
	sort.Slice(vfs, func(i, j int) bool { return vfs[i].n < vfs[j].n })
	out := make([]string, len(vfs))
	for i, v := range vfs {
		out[i] = v.addr
	}
	return out
}

// Acquire leases a virtual function to the device (idempotent).
func (m *Manager) Acquire(deviceID string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if a, ok := m.leases[deviceID]; ok {
		return a, nil
	}
	used := map[string]bool{}
	for _, a := range m.leases {
		used[a] = true
	}
	for _, a := range m.VFs() {
		if !used[a] {
			m.leases[deviceID] = a
			return a, m.save()
		}
	}
	return "", errors.New("gpu: no free SR-IOV virtual function on this node")
}

// Release frees the device's virtual function (no-op without one).
func (m *Manager) Release(deviceID string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.leases[deviceID]; ok {
		delete(m.leases, deviceID)
		_ = m.save()
	}
}

func (m *Manager) save() error {
	b, _ := json.Marshal(m.leases)
	tmp := m.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, m.path)
}
