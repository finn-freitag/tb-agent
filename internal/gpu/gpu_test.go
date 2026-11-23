package gpu

import (
	"os"
	"path/filepath"
	"testing"
)

func TestManager(t *testing.T) {
	root := t.TempDir()
	pf := filepath.Join(root, "sys", "bus", "pci", "devices", "0000:00:02.0")
	os.MkdirAll(pf, 0o755)
	for i, a := range []string{"0000:00:02.1", "0000:00:02.2"} {
		os.Symlink("../"+a, filepath.Join(pf, "virtfn"+string(rune('0'+i))))
	}
	os.MkdirAll(filepath.Join(root, "dev", "dri"), 0o755)
	os.WriteFile(filepath.Join(root, "dev", "dri", "renderD128"), nil, 0o644)

	data := t.TempDir()
	m, err := New(Config{SRIOV: SRIOVConfig{PF: "0000:00:02.0"}}, data)
	if err != nil {
		t.Fatal(err)
	}
	m.sysfs, m.dev = filepath.Join(root, "sys"), filepath.Join(root, "dev")
	if got := m.RenderNode(); got != filepath.Join(root, "dev", "dri", "renderD128") {
		t.Fatalf("render node %q", got)
	}
	if v := m.VFs(); len(v) != 2 || v[0] != "0000:00:02.1" {
		t.Fatalf("vfs %v", v)
	}
	a, _ := m.Acquire("d1")
	b, _ := m.Acquire("d2")
	if a2, _ := m.Acquire("d1"); a != "0000:00:02.1" || b != "0000:00:02.2" || a2 != a {
		t.Fatalf("leases %s %s %s", a, b, a2)
	}
	if _, err := m.Acquire("d3"); err == nil {
		t.Fatal("expected no free VF")
	}
	// Leases survive a restart.
	m2, _ := New(Config{SRIOV: SRIOVConfig{PF: "0000:00:02.0"}}, data)
	m2.sysfs = m.sysfs
	if got, _ := m2.Acquire("d2"); got != b {
		t.Fatalf("lease lost: %s", got)
	}
	if got := m2.Prune(func(id string) bool { return id != "d2" }); len(got) != 1 || got[0] != "d2" {
		t.Fatalf("prune: %v", got)
	}
	if got, _ := m2.Acquire("d2"); got != b {
		t.Fatalf("pruned VF not reusable: %s", got)
	}
	m2.Release("d1")
	if got, _ := m2.Acquire("d3"); got != a {
		t.Fatalf("released VF not reused: %s", got)
	}
}
