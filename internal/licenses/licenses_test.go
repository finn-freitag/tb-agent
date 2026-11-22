package licenses

import (
	"testing"

	agentv1 "github.com/finn-freitag/tb-proto/gen/agent/v1"

	"github.com/finn-freitag/tb-agent/internal/config"
)

func TestLeasing(t *testing.T) {
	dir := t.TempDir()
	pools := map[string]config.LicensePool{"p": {Type: "retail", Keys: []string{"K1", "K2"}}}
	m, err := New(pools, dir)
	if err != nil {
		t.Fatal(err)
	}
	pool := &agentv1.LicenseSpec{Mode: agentv1.LicenseMode_LICENSE_MODE_POOL, Pool: "p"}
	k1, err := m.Resolve("a", pool)
	if err != nil || k1 != "K1" {
		t.Fatal(k1, err)
	}
	if again, _ := m.Resolve("a", pool); again != "K1" {
		t.Fatal("lease not sticky")
	}
	if k2, _ := m.Resolve("b", pool); k2 != "K2" {
		t.Fatal(k2)
	}
	if _, err := m.Resolve("c", pool); err == nil {
		t.Fatal("expected exhausted pool")
	}
	// Persistence.
	m2, _ := New(pools, dir)
	if p := m2.Pools(); p[0].InUse != 2 || p[0].Total != 2 {
		t.Fatalf("pools %+v", p)
	}
	m2.Release("a")
	if k, _ := m2.Resolve("c", pool); k != "K1" {
		t.Fatal(k)
	}
	if k, _ := m2.Resolve("d", &agentv1.LicenseSpec{Mode: agentv1.LicenseMode_LICENSE_MODE_USER_KEY, UserKey: "U"}); k != "U" {
		t.Fatal(k)
	}
	if k, _ := m2.Resolve("d", nil); k != "" {
		t.Fatal(k)
	}
}
