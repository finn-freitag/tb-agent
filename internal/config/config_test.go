package config

import "testing"

func TestParse(t *testing.T) {
	c, err := Parse([]byte(`
name: homelab
dashboard:
  endpoint: dash.example.com
  token: tb1.x.y
limits:
  ram: 16G
  cpu: 70%
  storage: 300G
drivers:
  libvirt:
    network: default
  lxc:
license_pools:
  win:
    type: retail
    keys: [AAAAA-BBBBB]
`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Dashboard.Endpoint != "dash.example.com:7443" {
		t.Errorf("endpoint %q", c.Dashboard.Endpoint)
	}
	if c.RAMLimitMB != 16384 || c.CPULimitPct != 70 || c.StorageLimitGB != 300 {
		t.Errorf("limits %d %d %d", c.RAMLimitMB, c.CPULimitPct, c.StorageLimitGB)
	}
	if got := c.DriverNames(); len(got) != 2 || got[0] != "libvirt" {
		t.Errorf("drivers %v", got)
	}
	if c.DriverConfig("lxc") != nil {
		t.Error("empty driver config should be nil")
	}
	if c.DriverConfig("libvirt") == nil {
		t.Error("libvirt config missing")
	}
}

func TestParseErrors(t *testing.T) {
	for _, y := range []string{
		`name: x`,
		"name: x\ndashboard: {endpoint: a:1}\ndrivers: {libvirt: }\nlimits: {cpu: 150%}",
		"name: x\ndashboard: {endpoint: a:1}\ndrivers: {libvirt: }\nunknown: 1",
		"name: x\ndashboard: {endpoint: a:1}\ndrivers: {libvirt: }\nlicense_pools: {p: {type: foo, keys: [a]}}",
	} {
		if _, err := Parse([]byte(y)); err == nil {
			t.Errorf("expected error for %q", y)
		}
	}
}

func TestSizes(t *testing.T) {
	cases := map[string]int64{"": 0, "512M": 512, "16G": 16384, "1T": 1048576, "2GB": 2048, "1.5G": 1536, "100": 100}
	for in, want := range cases {
		got, err := ParseSizeMB(in)
		if err != nil || got != want {
			t.Errorf("%q: got %d %v want %d", in, got, err, want)
		}
	}
}
