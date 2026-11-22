package agent

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"

	agentv1 "github.com/finn-freitag/tb-proto/gen/agent/v1"
	"google.golang.org/protobuf/encoding/protojson"
)

// storedDevice is the persisted desired state of one device.
type storedDevice struct {
	Driver   string          `json:"driver"`
	Spec     json.RawMessage `json:"spec,omitempty"`
	Deleting bool            `json:"deleting,omitempty"`
}

// Store persists desired device state to <data_dir>/state.json so devices
// keep running (and get cleaned up) across agent restarts without the dashboard.
type Store struct {
	mu   sync.Mutex
	file string
}

func newStore(dataDir string) *Store { return &Store{file: filepath.Join(dataDir, "state.json")} }

type persisted struct {
	Driver   string
	Spec     *agentv1.DeviceSpec
	Deleting bool
}

func (s *Store) load() (map[string]persisted, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]persisted{}
	b, err := os.ReadFile(s.file)
	if errors.Is(err, os.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	var raw map[string]storedDevice
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, err
	}
	for id, d := range raw {
		p := persisted{Driver: d.Driver, Deleting: d.Deleting}
		if len(d.Spec) > 0 {
			spec := &agentv1.DeviceSpec{}
			if err := protojson.Unmarshal(d.Spec, spec); err != nil {
				return nil, err
			}
			p.Spec = spec
		}
		out[id] = p
	}
	return out, nil
}

func (s *Store) save(devs map[string]persisted) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw := map[string]storedDevice{}
	for id, d := range devs {
		sd := storedDevice{Driver: d.Driver, Deleting: d.Deleting}
		if d.Spec != nil {
			b, err := protojson.Marshal(d.Spec)
			if err != nil {
				return err
			}
			sd.Spec = b
		}
		raw[id] = sd
	}
	b, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.file + ".tmp"
	// Contains device passwords: owner-only.
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.file)
}
