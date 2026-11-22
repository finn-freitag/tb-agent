// Package config loads and validates the node config (node.yaml).
package config

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/finn-freitag/tb-agent/internal/gpu"
)

// Config is the parsed node config.
type Config struct {
	Name          string    `yaml:"name"`
	Dashboard     Dashboard `yaml:"dashboard"`
	DataDir       string    `yaml:"data_dir"`
	ListenAddress string    `yaml:"listen_address"`
	// AllowedSources limits who may connect to forwarded device ports: IPs,
	// CIDRs, "dashboard" (addresses of dashboard.endpoint), "private" (LAN,
	// ULA and Tailscale ranges) or "any". Default: [dashboard, private].
	// Loopback and the host's own addresses always work.
	AllowedSources []string `yaml:"allowed_sources"`
	// GPU configures GPU sharing (render node, SR-IOV virtual functions).
	GPU          gpu.Config             `yaml:"gpu"`
	Limits       Limits                 `yaml:"limits"`
	Drivers      map[string]yaml.Node   `yaml:"drivers"`
	LicensePools map[string]LicensePool `yaml:"license_pools"`
	LogLevel     string                 `yaml:"log_level"`

	// Parsed limits.
	RAMLimitMB     int64 `yaml:"-"`
	CPULimitPct    int32 `yaml:"-"`
	StorageLimitGB int64 `yaml:"-"`
}

// Dashboard is where the agent connects to.
type Dashboard struct {
	// Endpoint is host:port of the dashboard gRPC listener (default port 7443).
	Endpoint string `yaml:"endpoint"`
	// Token is the one-time enrollment token (tb1.<fp>.<secret>). Only needed
	// until the agent is enrolled; afterwards the certificates in data_dir are used.
	Token string `yaml:"token"`
}

// Limits caps what all devices on this node together may use.
type Limits struct {
	RAM     string `yaml:"ram"`     // e.g. "16G", "512M"; empty = host total
	CPU     string `yaml:"cpu"`     // e.g. "70%"; empty = 100%
	Storage string `yaml:"storage"` // e.g. "300G"; empty = unlimited
}

// LicensePool is a named set of Windows product keys. Keys never leave the node.
type LicensePool struct {
	Type string   `yaml:"type"` // retail | mak | kms
	Keys []string `yaml:"keys"`
}

var nameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,62}$`)

// Load reads and validates a config file.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(b)
}

// Parse parses and validates config YAML.
func Parse(b []byte) (*Config, error) {
	var c Config
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	if err := c.normalize(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) normalize() error {
	var errs []string
	if c.Name == "" {
		if h, err := os.Hostname(); err == nil {
			c.Name = h
		}
	}
	if !nameRe.MatchString(c.Name) {
		errs = append(errs, fmt.Sprintf("name %q is invalid", c.Name))
	}
	if c.Dashboard.Endpoint == "" {
		errs = append(errs, "dashboard.endpoint is required (host:port)")
	} else if _, _, err := net.SplitHostPort(c.Dashboard.Endpoint); err != nil {
		c.Dashboard.Endpoint = net.JoinHostPort(c.Dashboard.Endpoint, "7443")
	}
	if c.DataDir == "" {
		c.DataDir = "/var/lib/tb-agent"
	}
	if c.ListenAddress == "" {
		c.ListenAddress = "0.0.0.0"
	}
	if ip := net.ParseIP(c.ListenAddress); ip == nil {
		errs = append(errs, fmt.Sprintf("listen_address %q is not an IP address", c.ListenAddress))
	}
	if c.GPU.SRIOV.PF != "" && !gpu.ValidPCI(c.GPU.SRIOV.PF) {
		errs = append(errs, fmt.Sprintf("gpu.sriov.pf %q is not a PCI address like 0000:00:02.0", c.GPU.SRIOV.PF))
	}
	for _, v := range c.GPU.SRIOV.VFs {
		if !gpu.ValidPCI(v) {
			errs = append(errs, fmt.Sprintf("gpu.sriov.vfs: %q is not a PCI address", v))
		}
	}
	if len(c.AllowedSources) == 0 {
		c.AllowedSources = []string{"dashboard", "private"}
	}
	for _, e := range c.AllowedSources {
		switch strings.ToLower(strings.TrimSpace(e)) {
		case "any", "dashboard", "private":
			continue
		}
		if !validSource(e) {
			errs = append(errs, fmt.Sprintf("allowed_sources: %q is not an IP, CIDR, \"dashboard\", \"private\" or \"any\"", e))
		}
	}
	var err error
	if c.RAMLimitMB, err = ParseSizeMB(c.Limits.RAM); err != nil {
		errs = append(errs, "limits.ram: "+err.Error())
	}
	if c.CPULimitPct, err = ParsePercent(c.Limits.CPU); err != nil {
		errs = append(errs, "limits.cpu: "+err.Error())
	}
	sMB, err := ParseSizeMB(c.Limits.Storage)
	if err != nil {
		errs = append(errs, "limits.storage: "+err.Error())
	}
	c.StorageLimitGB = sMB / 1024
	if len(c.Drivers) == 0 {
		errs = append(errs, "drivers: at least one driver must be enabled")
	}
	for name, p := range c.LicensePools {
		switch p.Type {
		case "retail", "mak", "kms":
		case "":
			errs = append(errs, fmt.Sprintf("license_pools.%s.type is required (retail|mak|kms)", name))
		default:
			errs = append(errs, fmt.Sprintf("license_pools.%s.type %q must be retail, mak or kms", name, p.Type))
		}
		if len(p.Keys) == 0 {
			errs = append(errs, fmt.Sprintf("license_pools.%s has no keys", name))
		}
	}
	if len(errs) > 0 {
		return errors.New("config: " + strings.Join(errs, "; "))
	}
	return nil
}

// DriverNames returns the enabled driver names, sorted.
func (c *Config) DriverNames() []string {
	out := make([]string, 0, len(c.Drivers))
	for n := range c.Drivers {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// DriverConfig returns the YAML node for a driver (nil if empty).
func (c *Config) DriverConfig(name string) *yaml.Node {
	n, ok := c.Drivers[name]
	if !ok || n.Kind == 0 || (n.Kind == yaml.ScalarNode && (n.Tag == "!!null" || n.Value == "")) {
		return nil
	}
	return &n
}

// ParseSizeMB parses sizes like "16G", "512M", "1.5T", "300GB", "2048" (MB) into MiB.
// Empty means 0 (unlimited).
func ParseSizeMB(s string) (int64, error) {
	s = strings.TrimSpace(strings.ToUpper(s))
	if s == "" {
		return 0, nil
	}
	s = strings.TrimSuffix(strings.TrimSuffix(s, "IB"), "B")
	mult := 1.0
	switch {
	case strings.HasSuffix(s, "T"):
		mult, s = 1024*1024, strings.TrimSuffix(s, "T")
	case strings.HasSuffix(s, "G"):
		mult, s = 1024, strings.TrimSuffix(s, "G")
	case strings.HasSuffix(s, "M"):
		mult, s = 1, strings.TrimSuffix(s, "M")
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || v < 0 {
		return 0, fmt.Errorf("invalid size %q (use e.g. 16G, 512M)", s)
	}
	return int64(v * mult), nil
}

// ParsePercent parses "70%" or "70" into 70. Empty means 0 (= 100%).
func ParsePercent(s string) (int32, error) {
	s = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(s), "%"))
	if s == "" {
		return 0, nil
	}
	v, err := strconv.Atoi(s)
	if err != nil || v < 1 || v > 100 {
		return 0, fmt.Errorf("invalid percentage %q (1-100)", s)
	}
	return int32(v), nil
}

func validSource(e string) bool {
	e = strings.TrimSpace(e)
	if _, err := netip.ParsePrefix(e); err == nil {
		return true
	}
	_, err := netip.ParseAddr(e)
	return err == nil
}
