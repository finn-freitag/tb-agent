// Package browser implements the "browser" driver: disposable web browsers,
// one Docker container per device, used through VNC (e.g. Guacamole) or the
// image's own web UI. Nothing is persisted: deleting the device deletes the
// container with its profile, downloads and history.
//
// The template's image must be a docker:// image that serves VNC and/or a
// web UI (e.g. jlesage/firefox, jlesage/chromium). The container
// environment comes from template params "browser.env.<NAME>"; the values
// may contain {password}, {user} and {hostname}.
package browser

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/finn-freitag/tb-agent/internal/dockerapi"
	"github.com/finn-freitag/tb-agent/internal/driver"
	agentv1 "github.com/finn-freitag/tb-proto/gen/agent/v1"
	"gopkg.in/yaml.v3"
)

const name = "browser"

// Labels on every container of this driver.
const (
	labelDevice = "tossbox.device"
	labelDriver = "tossbox.driver"
)

// Config is the driver's node config section.
//
//	drivers:
//	  browser:
//	    socket: /var/run/docker.sock
//	    network: tossbox                 # created with ICC off
//	    offline_network: tossbox-offline # "offline" policy (internal network)
//	    shm_mb: 1024                     # /dev/shm size (Chromium needs a large one)
//	    pids_limit: 4096
type Config struct {
	Socket         string `yaml:"socket"`
	Network        string `yaml:"network"`
	OfflineNetwork string `yaml:"offline_network"`
	ShmMB          int64  `yaml:"shm_mb"`
	PidsLimit      int64  `yaml:"pids_limit"`
}

// Driver is the browser driver.
type Driver struct {
	cfg    Config
	env    driver.Env
	docker *dockerapi.Client
	cpu    driver.CPUMeter
}

func init() {
	driver.Register(name, func(env driver.Env, node *yaml.Node) (driver.Driver, error) {
		var cfg Config
		if err := driver.DecodeConfig(node, &cfg); err != nil {
			return nil, fmt.Errorf("browser config: %w", err)
		}
		return New(env, cfg), nil
	})
}

// New creates the driver.
func New(env driver.Env, cfg Config) *Driver {
	if cfg.Socket == "" {
		cfg.Socket = dockerapi.DefaultSocket
	}
	if cfg.Network == "" {
		cfg.Network = dockerapi.DeviceNetwork
	}
	if cfg.OfflineNetwork == "" {
		cfg.OfflineNetwork = dockerapi.OfflineNetwork
	}
	if cfg.ShmMB == 0 {
		cfg.ShmMB = 1024
	}
	if cfg.PidsLimit == 0 {
		cfg.PidsLimit = 4096
	}
	return &Driver{cfg: cfg, env: env, docker: dockerapi.New(cfg.Socket)}
}

// Name implements driver.Driver.
func (d *Driver) Name() string { return name }

// Capabilities implements driver.Driver.
func (d *Driver) Capabilities(ctx context.Context) *agentv1.DriverCapabilities {
	caps := &agentv1.DriverCapabilities{
		Name: name, Families: []string{"browser"}, Services: []string{"vnc", "http", "https"}, Healthy: true,
		NetworkPolicies: []string{driver.PolicyInternet, driver.PolicyOffline},
	}
	if err := d.docker.Ping(ctx); err != nil {
		caps.Healthy, caps.Message = false, "docker daemon not reachable: "+err.Error()
	}
	return caps
}

func password(spec *agentv1.DeviceSpec) string {
	for _, s := range spec.GetServices() {
		if s.GetPassword() != "" {
			return s.GetPassword()
		}
	}
	return ""
}

// containerConfig maps a spec to the container (pure, unit tested).
func (d *Driver) containerConfig(spec *agentv1.DeviceSpec) (dockerapi.ContainerConfig, string, bool, error) {
	tpl := spec.GetTemplate()
	image, ok := strings.CutPrefix(tpl.GetImage().GetUrl(), "docker://")
	if !ok || image == "" {
		return dockerapi.ContainerConfig{}, "", false, errors.New("browser: template image must be docker://<image>")
	}
	policy, err := driver.NetworkPolicy(spec)
	if err != nil {
		return dockerapi.ContainerConfig{}, "", false, err
	}
	if driver.WantsGPU(spec) {
		return dockerapi.ContainerConfig{}, "", false, errors.New("browser: GPU sharing is not supported for browsers")
	}
	pw := password(spec)
	if pw == "" {
		// The password protects VNC and the web UI; never start without one.
		return dockerapi.ContainerConfig{}, "", false, errors.New("browser: no password in the device spec")
	}
	repl := strings.NewReplacer("{password}", pw, "{user}", tpl.GetDefaultUser(), "{hostname}", spec.GetHostname())
	var env []string
	for k, v := range tpl.GetParams() {
		if n, ok := strings.CutPrefix(k, "browser.env."); ok && n != "" {
			env = append(env, n+"="+repl.Replace(v))
		}
	}
	sort.Strings(env)

	network, offline := d.cfg.Network, false
	if policy == driver.PolicyOffline {
		network, offline = d.cfg.OfflineNetwork, true
	}
	res := spec.GetResources()
	shm := d.cfg.ShmMB
	if v, err := strconv.ParseInt(tpl.GetParams()["browser.shm_mb"], 10, 64); err == nil && v > 0 {
		shm = v
	}
	hc := dockerapi.HostConfig{
		RestartPolicy: dockerapi.RestartPolicy{Name: "unless-stopped"},
		NetworkMode:   network,
		ShmSize:       shm << 20,
		PidsLimit:     d.cfg.PidsLimit,
		SecurityOpt:   []string{"no-new-privileges:true"},
	}
	if m := res.GetRamMb(); m > 0 {
		hc.Memory = m << 20
	}
	switch {
	case res.GetCpuCapPercent() > 0:
		hc.NanoCPUs = int64(runtime.NumCPU()) * int64(res.GetCpuCapPercent()) * 1e7
	case res.GetVcpus() > 0:
		hc.NanoCPUs = int64(res.GetVcpus()) * 1e9
	}
	stop := 10
	return dockerapi.ContainerConfig{
		Image:       image,
		Hostname:    spec.GetHostname(),
		Env:         env,
		StopTimeout: &stop,
		Labels:      map[string]string{labelDevice: spec.DeviceId, labelDriver: name, "tossbox.template": tpl.GetId()},
		HostConfig:  hc,
	}, network, offline, nil
}

// Ensure implements driver.Driver.
func (d *Driver) Ensure(ctx context.Context, spec *agentv1.DeviceSpec, report driver.Reporter) error {
	cname := driver.DomainName(spec.DeviceId)
	ci, err := d.docker.InspectContainer(ctx, cname)
	if errors.Is(err, dockerapi.ErrNotFound) {
		cfg, network, offline, err := d.containerConfig(spec)
		if err != nil {
			return err
		}
		if ok, _ := d.docker.ImageExists(ctx, cfg.Image); !ok {
			report(agentv1.DeviceState_DEVICE_STATE_PROVISIONING, "Pulling "+cfg.Image)
			err := d.docker.PullImage(ctx, cfg.Image, func(done, total int64, _ string) {
				if total > 0 {
					report(agentv1.DeviceState_DEVICE_STATE_PROVISIONING, fmt.Sprintf("Pulling %s (%d%%)", cfg.Image, done*100/total))
				}
			})
			if err != nil {
				return err
			}
		}
		ensure := d.docker.EnsureNetwork
		if offline {
			ensure = d.docker.EnsureOfflineNetwork
		}
		if err := ensure(ctx, network); err != nil {
			return fmt.Errorf("network %s: %w", network, err)
		}
		report(agentv1.DeviceState_DEVICE_STATE_PROVISIONING, "Creating browser container")
		if _, err := d.docker.CreateContainer(ctx, cname, cfg); err != nil {
			return err
		}
		if ci, err = d.docker.InspectContainer(ctx, cname); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	if spec.Power == agentv1.PowerState_POWER_STATE_STOPPED {
		if ci.State.Running {
			return d.docker.StopContainer(ctx, ci.ID, 10*time.Second)
		}
		return nil
	}
	if !ci.State.Running {
		report(agentv1.DeviceState_DEVICE_STATE_PROVISIONING, "Starting browser")
		return d.docker.StartContainer(ctx, ci.ID)
	}
	return nil
}

// Destroy implements driver.Driver.
func (d *Driver) Destroy(ctx context.Context, id string) error {
	return d.docker.RemoveContainer(ctx, driver.DomainName(id))
}

// List implements driver.Driver.
func (d *Driver) List(ctx context.Context) ([]*agentv1.DeviceStatus, error) {
	cs, err := d.docker.ListContainers(ctx, labelDriver+"="+name)
	if err != nil {
		return nil, err
	}
	var out []*agentv1.DeviceStatus
	for _, c := range cs {
		if id := c.Labels[labelDevice]; id != "" {
			if st, err := d.Status(ctx, id); err == nil {
				out = append(out, st)
			}
		}
	}
	return out, nil
}

// Status implements driver.Driver.
func (d *Driver) Status(ctx context.Context, id string) (*agentv1.DeviceStatus, error) {
	st := &agentv1.DeviceStatus{DeviceId: id, UpdatedUnix: time.Now().Unix()}
	ci, err := d.docker.InspectContainer(ctx, driver.DomainName(id))
	if errors.Is(err, dockerapi.ErrNotFound) {
		st.State = agentv1.DeviceState_DEVICE_STATE_DESTROYED
		return st, nil
	}
	if err != nil {
		return nil, err
	}
	switch ci.State.Status {
	case "running", "restarting":
		st.State = agentv1.DeviceState_DEVICE_STATE_RUNNING
		st.InternalIp = ci.IP("")
		if u, err := d.docker.ContainerUsage(ctx, ci.ID); err == nil {
			st.MemoryUsedMb = u.MemoryMB
			st.CpuUsedPercent = d.cpu.Percent(id, float64(u.CPUNs), float64(u.SystemNs))
		}
	case "created":
		st.State = agentv1.DeviceState_DEVICE_STATE_PENDING
	case "exited", "paused":
		st.State = agentv1.DeviceState_DEVICE_STATE_STOPPED
	case "dead":
		st.State, st.Message = agentv1.DeviceState_DEVICE_STATE_FAILED, ci.State.Error
	default:
		st.State = agentv1.DeviceState_DEVICE_STATE_PROVISIONING
	}
	return st, nil
}
