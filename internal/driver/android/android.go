// Package android implements the "android" driver with two sub-backends:
//
//   - redroid: Android in a privileged Docker container (redroid/redroid
//     images). adb listens on <container IP>:5555.
//   - cuttlefish: Google's Cuttlefish virtual device (KVM), launched with
//     launch_cvd from a Cuttlefish host package, one instance per device.
//
// The backend is chosen by the device param "backend", else the template
// param "android.backend", else the node config's default_backend.
// adb readiness is checked by the agent core; Cuttlefish's per-instance ports
// are reported through driver.EndpointResolver.
package android

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/finn-freitag/tb-agent/internal/driver"
	agentv1 "github.com/finn-freitag/tb-proto/gen/agent/v1"
	"gopkg.in/yaml.v3"
)

const name = "android"

// Labels / markers for ownership.
const (
	labelDevice  = "tossbox.device"
	labelDriver  = "tossbox.driver"
	labelBackend = "tossbox.android-backend"
)

// Config is the driver's node config section.
//
//	drivers:
//	  android:
//	    default_backend: redroid
//	    redroid:
//	      enabled: true
//	      socket: /var/run/docker.sock
//	      network: bridge
//	      image: redroid/redroid:15.0.0_64only-latest   # used if the template has no docker:// image
//	      width: 720
//	      height: 1280
//	      dpi: 320
//	      gpu_mode: guest
//	    cuttlefish:
//	      enabled: false
//	      host_package_dir: /opt/cuttlefish/host   # contains bin/launch_cvd
//	      images_dir: /opt/cuttlefish/images       # default system images (template param cuttlefish.images_dir overrides)
//	      max_instances: 4
type Config struct {
	DefaultBackend string           `yaml:"default_backend"`
	Redroid        RedroidConfig    `yaml:"redroid"`
	Cuttlefish     CuttlefishConfig `yaml:"cuttlefish"`
}

// backend is implemented by redroid and cuttlefish.
type backend interface {
	name() string
	health(ctx context.Context) error
	ensure(ctx context.Context, spec *agentv1.DeviceSpec, report driver.Reporter) error
	destroy(ctx context.Context, id string) error
	// list returns the ids of devices managed by this backend.
	list(ctx context.Context) ([]string, error)
	// status returns nil, nil if the device is unknown to this backend.
	status(ctx context.Context, id string) (*agentv1.DeviceStatus, error)
	services() []string
	// serviceEndpoint: see driver.EndpointResolver.
	serviceEndpoint(ctx context.Context, id, service string) (string, bool)
}

// Driver is the android driver.
type Driver struct {
	cfg      Config
	env      driver.Env
	backends map[string]backend
}

func init() {
	driver.Register(name, func(env driver.Env, node *yaml.Node) (driver.Driver, error) {
		var cfg Config
		if err := driver.DecodeConfig(node, &cfg); err != nil {
			return nil, fmt.Errorf("android config: %w", err)
		}
		return New(env, cfg)
	})
}

// New creates the driver. If neither backend is explicitly enabled, redroid is enabled.
func New(env driver.Env, cfg Config) (*Driver, error) {
	if !cfg.Redroid.Enabled && !cfg.Cuttlefish.Enabled {
		cfg.Redroid.Enabled = true
	}
	d := &Driver{cfg: cfg, env: env, backends: map[string]backend{}}
	if cfg.Redroid.Enabled {
		d.backends["redroid"] = newRedroid(env, cfg.Redroid)
	}
	if cfg.Cuttlefish.Enabled {
		cf, err := newCuttlefish(env, cfg.Cuttlefish)
		if err != nil {
			return nil, err
		}
		d.backends["cuttlefish"] = cf
	}
	if d.cfg.DefaultBackend == "" {
		if cfg.Redroid.Enabled {
			d.cfg.DefaultBackend = "redroid"
		} else {
			d.cfg.DefaultBackend = "cuttlefish"
		}
	}
	if _, ok := d.backends[d.cfg.DefaultBackend]; !ok {
		return nil, fmt.Errorf("android: default_backend %q is not enabled", d.cfg.DefaultBackend)
	}
	return d, nil
}

// Name implements driver.Driver.
func (d *Driver) Name() string { return name }

func (d *Driver) sortedBackends() []backend {
	out := make([]backend, 0, len(d.backends))
	for _, b := range d.backends {
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name() < out[j].name() })
	return out
}

// Capabilities implements driver.Driver. Only healthy backends are listed.
func (d *Driver) Capabilities(ctx context.Context) *agentv1.DriverCapabilities {
	caps := &agentv1.DriverCapabilities{Name: name, Families: []string{"android"}}
	svc := map[string]bool{}
	var problems []string
	for _, b := range d.sortedBackends() {
		if err := b.health(ctx); err != nil {
			problems = append(problems, b.name()+": "+err.Error())
			continue
		}
		caps.Backends = append(caps.Backends, b.name())
		for _, s := range b.services() {
			svc[s] = true
		}
	}
	for s := range svc {
		caps.Services = append(caps.Services, s)
	}
	sort.Strings(caps.Services)
	caps.Healthy = len(caps.Backends) > 0
	caps.Message = strings.Join(problems, "; ")
	return caps
}

func (d *Driver) backendFor(spec *agentv1.DeviceSpec) (backend, error) {
	n := spec.GetParams()["backend"]
	if n == "" {
		n = spec.GetTemplate().GetParams()["android.backend"]
	}
	if n == "" {
		n = d.cfg.DefaultBackend
	}
	b, ok := d.backends[n]
	if !ok {
		return nil, fmt.Errorf("android: backend %q is not enabled on this node", n)
	}
	return b, nil
}

// Ensure implements driver.Driver.
func (d *Driver) Ensure(ctx context.Context, spec *agentv1.DeviceSpec, report driver.Reporter) error {
	// An existing device stays on the backend it was created with.
	for _, b := range d.sortedBackends() {
		st, err := b.status(ctx, spec.DeviceId)
		if err != nil {
			return err
		}
		if st != nil && st.State != agentv1.DeviceState_DEVICE_STATE_DESTROYED {
			return b.ensure(ctx, spec, report)
		}
	}
	b, err := d.backendFor(spec)
	if err != nil {
		return err
	}
	return b.ensure(ctx, spec, report)
}

// Destroy implements driver.Driver.
func (d *Driver) Destroy(ctx context.Context, id string) error {
	var errs []error
	for _, b := range d.sortedBackends() {
		if err := b.destroy(ctx, id); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// List implements driver.Driver.
func (d *Driver) List(ctx context.Context) ([]*agentv1.DeviceStatus, error) {
	var out []*agentv1.DeviceStatus
	for _, b := range d.sortedBackends() {
		ids, err := b.list(ctx)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", b.name(), err)
		}
		for _, id := range ids {
			st, err := b.status(ctx, id)
			if err != nil {
				return nil, err
			}
			if st != nil {
				out = append(out, st)
			}
		}
	}
	return out, nil
}

// Status implements driver.Driver.
func (d *Driver) Status(ctx context.Context, id string) (*agentv1.DeviceStatus, error) {
	for _, b := range d.sortedBackends() {
		st, err := b.status(ctx, id)
		if err != nil {
			return nil, err
		}
		if st != nil && st.State != agentv1.DeviceState_DEVICE_STATE_DESTROYED {
			return st, nil
		}
	}
	return &agentv1.DeviceStatus{DeviceId: id, State: agentv1.DeviceState_DEVICE_STATE_DESTROYED}, nil
}

// ServiceEndpoint implements driver.EndpointResolver (Cuttlefish serves adb
// and VNC on per-instance host ports; redroid uses <container IP>:<port>).
func (d *Driver) ServiceEndpoint(ctx context.Context, id, service string) (string, bool) {
	for _, b := range d.sortedBackends() {
		if ep, ok := b.serviceEndpoint(ctx, id, service); ok {
			return ep, true
		}
	}
	return "", false
}

func hasService(spec *agentv1.DeviceSpec, svc string) bool {
	for _, s := range spec.GetServices() {
		if s.GetName() == svc {
			return true
		}
	}
	return false
}
