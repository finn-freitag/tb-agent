// Package lxc implements the "lxc" driver: Linux system containers (and
// optionally Incus VMs) managed through the Incus REST API on its unix socket.
//
// Guests are provisioned with cloud-init via the instance's
// "cloud-init.user-data" config key, so templates must use cloud-enabled
// images (e.g. "incus:images:debian/13/cloud").
package lxc

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/finn-freitag/tb-agent/internal/cloudinit"
	"github.com/finn-freitag/tb-agent/internal/driver"
	agentv1 "github.com/finn-freitag/tb-proto/gen/agent/v1"
	"gopkg.in/yaml.v3"
)

const name = "lxc"

// Config keys put on every instance created by this driver.
const (
	keyDevice = "user.tossbox.device"
	keyDriver = "user.tossbox.driver"
)

// Config is the driver's node config section.
//
//	drivers:
//	  lxc:
//	    socket: /var/lib/incus/unix.socket
//	    project: default
//	    storage_pool: default
//	    profiles: [default]
//	    remotes:                       # image remotes for incus:<remote>:<alias>
//	      images: https://images.linuxcontainers.org
//	    allow_vms: false               # allow templates with param lxc.type=virtual-machine
type Config struct {
	Socket      string            `yaml:"socket"`
	Project     string            `yaml:"project"`
	StoragePool string            `yaml:"storage_pool"`
	Profiles    []string          `yaml:"profiles"`
	Remotes     map[string]string `yaml:"remotes"`
	AllowVMs    bool              `yaml:"allow_vms"`
	StopTimeout time.Duration     `yaml:"stop_timeout"`
	// ContainerConfig is extra Incus instance config applied to every
	// container (not VMs). Default: security.nesting=true, which current
	// systemd (257+) needs to boot on hosts that restrict unprivileged user
	// namespaces via AppArmor (e.g. Ubuntu 24.04+). Set a key to override.
	ContainerConfig map[string]string `yaml:"container_config"`
	// IsolatePorts copies the profiles' bridged NICs onto each instance with
	// security.port_isolation=true, so devices can't reach each other.
	// Default true.
	IsolatePorts *bool `yaml:"isolate_ports"`
	// OfflineNetwork is the Incus bridge for devices with the "offline"
	// policy (no NAT, no DNS; default "tb-incus-off"), created if missing.
	OfflineNetwork string `yaml:"offline_network"`
}

func (c *Config) defaults() {
	if c.Socket == "" {
		c.Socket = "/var/lib/incus/unix.socket"
	}
	if c.Project == "" {
		c.Project = "default"
	}
	if c.StoragePool == "" {
		c.StoragePool = "default"
	}
	if len(c.Profiles) == 0 {
		c.Profiles = []string{"default"}
	}
	if c.Remotes == nil {
		c.Remotes = map[string]string{}
	}
	if _, ok := c.Remotes["images"]; !ok {
		c.Remotes["images"] = "https://images.linuxcontainers.org"
	}
	if c.ContainerConfig == nil {
		c.ContainerConfig = map[string]string{}
	}
	if _, ok := c.ContainerConfig["security.nesting"]; !ok {
		c.ContainerConfig["security.nesting"] = "true"
	}
	if c.StopTimeout == 0 {
		c.StopTimeout = 30 * time.Second
	}
	if c.OfflineNetwork == "" {
		c.OfflineNetwork = "tb-incus-off"
	}
}

// Driver is the lxc driver.
type Driver struct {
	cfg   Config
	env   driver.Env
	incus *incusClient
	cores int
}

func init() {
	driver.Register(name, func(env driver.Env, node *yaml.Node) (driver.Driver, error) {
		var cfg Config
		if err := driver.DecodeConfig(node, &cfg); err != nil {
			return nil, fmt.Errorf("lxc config: %w", err)
		}
		return New(env, cfg), nil
	})
}

// New creates the driver.
func New(env driver.Env, cfg Config) *Driver {
	cfg.defaults()
	return &Driver{cfg: cfg, env: env, incus: newIncusClient(cfg.Socket, cfg.Project), cores: runtime.NumCPU()}
}

// Name implements driver.Driver.
func (d *Driver) Name() string { return name }

// Capabilities implements driver.Driver.
func (d *Driver) Capabilities(ctx context.Context) *agentv1.DriverCapabilities {
	caps := &agentv1.DriverCapabilities{
		Name:     name,
		Families: []string{"linux"},
		Services: []string{"ssh", "http", "https"},
		Healthy:  true,

		NetworkPolicies: []string{driver.PolicyInternet, driver.PolicyOffline},
	}
	if d.env.GPU != nil && d.env.GPU.RenderNode() != "" {
		caps.GpuMode, caps.GpuSlots = "render", -1 // Incus gpu device (host render node)
	}
	if d.cfg.AllowVMs {
		caps.Services = append(caps.Services, "vnc")
	}
	if err := d.incus.ping(ctx); err != nil {
		caps.Healthy = false
		caps.Message = "incus not reachable: " + err.Error()
	}
	return caps
}

// parseImage parses "incus:<remote>:<alias>".
func parseImage(u string, remotes map[string]string) (server, alias string, err error) {
	rest, ok := strings.CutPrefix(u, "incus:")
	if !ok {
		return "", "", fmt.Errorf("lxc: unsupported image url %q (want incus:<remote>:<alias>)", u)
	}
	remote, alias, ok := strings.Cut(rest, ":")
	if !ok || remote == "" || alias == "" {
		return "", "", fmt.Errorf("lxc: malformed image url %q", u)
	}
	server, ok = remotes[remote]
	if !ok {
		return "", "", fmt.Errorf("lxc: unknown image remote %q", remote)
	}
	return server, alias, nil
}

// buildCreate maps a spec to the Incus create request (pure, unit tested).
func (d *Driver) buildCreate(spec *agentv1.DeviceSpec) (instancesPost, error) {
	tpl := spec.GetTemplate()
	server, alias, err := parseImage(tpl.GetImage().GetUrl(), d.cfg.Remotes)
	if err != nil {
		return instancesPost{}, err
	}
	typ := "container"
	params := tpl.GetParams()
	if params["lxc.type"] == "virtual-machine" {
		if !d.cfg.AllowVMs {
			return instancesPost{}, errors.New("lxc: virtual machines are not enabled on this node (allow_vms)")
		}
		typ = "virtual-machine"
	}
	user, password := credentials(spec)
	seed, err := cloudinit.Linux(cloudinit.Params{
		InstanceID: spec.DeviceId, Hostname: spec.GetHostname(), User: user, Password: password,
		SSHKeys: spec.GetSshAuthorizedKeys(),
		// images: cloud variants have cloud-init but no SSH server.
		Packages:     strings.Fields(strings.ReplaceAll(cmpOr(params["lxc.packages"], "openssh-server"), ",", " ")),
		NoGuestAgent: typ == "container",
	}.WithTemplate(params))
	if err != nil {
		return instancesPost{}, err
	}
	res := spec.GetResources()
	cfg := map[string]string{}
	if typ == "container" {
		for k, v := range d.cfg.ContainerConfig {
			cfg[k] = v
		}
	}
	for k, v := range map[string]string{
		keyDevice:               spec.DeviceId,
		keyDriver:               name,
		"user.tossbox.template": tpl.GetId(),
		"cloud-init.user-data":  string(seed.UserData),
	} {
		cfg[k] = v
	}
	if v := res.GetVcpus(); v > 0 {
		cfg["limits.cpu"] = fmt.Sprint(v)
	}
	if m := res.GetRamMb(); m > 0 {
		cfg["limits.memory"] = fmt.Sprintf("%dMiB", m)
	}
	if p := res.GetCpuCapPercent(); p > 0 && typ == "container" {
		// Hard limit: share of the whole host, expressed as CPU time per 100ms.
		ms := d.cores * int(p)
		cfg["limits.cpu.allowance"] = fmt.Sprintf("%dms/100ms", max(1, ms))
	}
	root := map[string]string{"type": "disk", "path": "/", "pool": d.cfg.StoragePool}
	if s := res.GetStorageGb(); s > 0 {
		root["size"] = fmt.Sprintf("%dGiB", s)
	}
	return instancesPost{
		Name:     driver.DomainName(spec.DeviceId),
		Type:     typ,
		Source:   instanceSource{Type: "image", Protocol: "simplestreams", Server: server, Alias: alias, Mode: "pull"},
		Profiles: d.cfg.Profiles,
		Config:   cfg,
		Devices:  map[string]map[string]string{"root": root},
		Start:    false,
	}, nil
}

// Ensure implements driver.Driver.
func (d *Driver) Ensure(ctx context.Context, spec *agentv1.DeviceSpec, report driver.Reporter) error {
	iname := driver.DomainName(spec.DeviceId)
	inst, err := d.incus.get(ctx, iname)
	if errors.Is(err, errNotFound) {
		body, err := d.buildCreate(spec)
		if err != nil {
			return err
		}
		policy, err := driver.NetworkPolicy(spec)
		if err != nil {
			return err
		}
		isolate := d.cfg.IsolatePorts == nil || *d.cfg.IsolatePorts
		if isolate || policy == driver.PolicyOffline {
			var profiles []map[string]map[string]string
			for _, pn := range d.cfg.Profiles {
				devs, err := d.incus.profileDevices(ctx, pn)
				if err != nil {
					return fmt.Errorf("profile %s: %w", pn, err)
				}
				profiles = append(profiles, devs)
			}
			nics := map[string]map[string]string{}
			if isolate {
				nics = isolatedNICs(profiles)
			}
			if policy == driver.PolicyOffline {
				if err := d.incus.ensureOfflineNetwork(ctx, d.cfg.OfflineNetwork); err != nil {
					return fmt.Errorf("offline network: %w", err)
				}
				nics = offlineNICs(profiles, d.cfg.OfflineNetwork)
			}
			for n, dev := range nics {
				body.Devices[n] = dev
			}
		}
		if driver.WantsGPU(spec) {
			if d.env.GPU == nil || d.env.GPU.RenderNode() == "" {
				return errors.New("lxc: no GPU render node on this node")
			}
			body.Devices["gpu0"] = map[string]string{"type": "gpu", "gputype": "physical"}
		}
		report(agentv1.DeviceState_DEVICE_STATE_PROVISIONING, "Creating container (downloading image if needed)")
		if err := d.incus.create(ctx, body); err != nil {
			return err
		}
		inst, err = d.incus.get(ctx, iname)
		if err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	running := strings.EqualFold(inst.Status, "Running")
	switch spec.Power {
	case agentv1.PowerState_POWER_STATE_STOPPED:
		if running {
			return d.incus.setState(ctx, iname, "stop", int(d.cfg.StopTimeout.Seconds()), false)
		}
	default:
		if !running {
			report(agentv1.DeviceState_DEVICE_STATE_PROVISIONING, "Starting")
			return d.incus.setState(ctx, iname, "start", 0, false)
		}
	}
	return nil
}

// Destroy implements driver.Driver.
func (d *Driver) Destroy(ctx context.Context, deviceID string) error {
	iname := driver.DomainName(deviceID)
	inst, err := d.incus.get(ctx, iname)
	if errors.Is(err, errNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if strings.EqualFold(inst.Status, "Running") {
		if err := d.incus.setState(ctx, iname, "stop", 0, true); err != nil {
			return err
		}
	}
	if err := d.incus.delete(ctx, iname); err != nil && !errors.Is(err, errNotFound) {
		return err
	}
	return nil
}

// List implements driver.Driver.
func (d *Driver) List(ctx context.Context) ([]*agentv1.DeviceStatus, error) {
	all, err := d.incus.list(ctx)
	if err != nil {
		return nil, err
	}
	var out []*agentv1.DeviceStatus
	for _, in := range all {
		if in.Config[keyDriver] != name || in.Config[keyDevice] == "" {
			continue
		}
		st, err := d.Status(ctx, in.Config[keyDevice])
		if err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].DeviceId < out[j].DeviceId })
	return out, nil
}

// Status implements driver.Driver.
func (d *Driver) Status(ctx context.Context, deviceID string) (*agentv1.DeviceStatus, error) {
	out := &agentv1.DeviceStatus{DeviceId: deviceID, UpdatedUnix: time.Now().Unix()}
	st, err := d.incus.state(ctx, driver.DomainName(deviceID))
	if errors.Is(err, errNotFound) {
		out.State = agentv1.DeviceState_DEVICE_STATE_DESTROYED
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	out.State = mapStatus(st.Status)
	out.InternalIp = pickIPv4(st)
	if root, ok := st.Disk["root"]; ok {
		out.StorageUsedMb = root.Usage >> 20
	}
	out.MemoryUsedMb = st.Memory.Usage >> 20
	return out, nil
}

func mapStatus(s string) agentv1.DeviceState {
	switch strings.ToLower(s) {
	case "running":
		return agentv1.DeviceState_DEVICE_STATE_RUNNING
	case "stopped":
		return agentv1.DeviceState_DEVICE_STATE_STOPPED
	case "starting", "thawed":
		return agentv1.DeviceState_DEVICE_STATE_PROVISIONING
	case "stopping", "freezing", "frozen":
		return agentv1.DeviceState_DEVICE_STATE_STOPPING
	case "error":
		return agentv1.DeviceState_DEVICE_STATE_FAILED
	}
	return agentv1.DeviceState_DEVICE_STATE_UNSPECIFIED
}

// pickIPv4 returns the first global IPv4 address of a non-loopback interface,
// preferring eth0 / enp5s0.
func pickIPv4(st *instanceState) string {
	names := make([]string, 0, len(st.Network))
	for n := range st.Network {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool {
		pi, pj := pref(names[i]), pref(names[j])
		if pi != pj {
			return pi < pj
		}
		return names[i] < names[j]
	})
	for _, n := range names {
		if n == "lo" {
			continue
		}
		for _, a := range st.Network[n].Addresses {
			if a.Family == "inet" && a.Scope == "global" {
				return a.Address
			}
		}
	}
	return ""
}

func pref(n string) int {
	switch n {
	case "eth0", "enp5s0":
		return 0
	}
	return 1
}

func credentials(spec *agentv1.DeviceSpec) (user, password string) {
	user = spec.GetTemplate().GetDefaultUser()
	for _, s := range spec.GetServices() {
		if s.GetPassword() != "" {
			password = s.GetPassword()
			if s.GetUsername() != "" {
				user = s.GetUsername()
			}
			break
		}
	}
	if user == "" {
		user = "tossbox"
	}
	return user, password
}

func cmpOr(v, def string) string {
	if v != "" {
		return v
	}
	return def
}

// isolatedNICs returns the effective bridged NICs of the given profiles
// (later profiles override earlier ones) with security.port_isolation set.
// Other NIC types (macvlan, physical, ...) don't support isolation and are
// left to the profiles.
func isolatedNICs(profiles []map[string]map[string]string) map[string]map[string]string {
	eff := map[string]map[string]string{}
	for _, devs := range profiles {
		for n, dev := range devs {
			eff[n] = dev
		}
	}
	out := map[string]map[string]string{}
	for n, dev := range eff {
		if dev["type"] != "nic" {
			continue
		}
		bridged := dev["nictype"] == "bridged" || (dev["nictype"] == "" && dev["network"] != "")
		if !bridged {
			continue
		}
		c := make(map[string]string, len(dev)+1)
		for k, v := range dev {
			c[k] = v
		}
		c["security.port_isolation"] = "true"
		out[n] = c
	}
	return out
}

// offlineNICs replaces every NIC of the profiles (eth0 if there is none)
// with an isolated port on the offline network.
func offlineNICs(profiles []map[string]map[string]string, network string) map[string]map[string]string {
	names := map[string]string{}
	for _, devs := range profiles {
		for n, dev := range devs {
			if dev["type"] == "nic" {
				names[n] = dev["name"]
			}
		}
	}
	if len(names) == 0 {
		names["eth0"] = "eth0"
	}
	out := map[string]map[string]string{}
	for n, ifname := range names {
		if ifname == "" {
			ifname = n
		}
		out[n] = map[string]string{"type": "nic", "network": network, "name": ifname, "security.port_isolation": "true"}
	}
	return out
}
