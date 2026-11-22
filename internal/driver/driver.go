// Package driver defines the interface every device backend implements and
// the registry the agent uses to instantiate enabled drivers from its config.
//
// Ownership: the agent core (internal/agent) owns the control stream, port
// forwarding (public_port -> internal_ip:internal_port), readiness checks and
// license key leasing. Drivers only create, run, stop and destroy devices.
package driver

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"sync"

	agentv1 "github.com/finn-freitag/tb-proto/gen/agent/v1"
	"gopkg.in/yaml.v3"
)

// Driver is a device backend (libvirt, docker-qemu, lxc, android, orchard, usb-phone).
//
// All methods must be safe for concurrent use for different device IDs. The
// agent core never calls Ensure/Destroy concurrently for the same device ID.
type Driver interface {
	// Name returns the driver name as used in configs and the protocol, e.g. "libvirt".
	Name() string

	// Capabilities reports what this driver can do on this host right now.
	Capabilities(ctx context.Context) *agentv1.DriverCapabilities

	// Ensure converges the actual device to the spec: create it if it does not
	// exist (fetch image, create disk, provision guest), then start or stop it
	// according to spec.Power. Must be idempotent: calling it again with the
	// same spec is a cheap no-op. Long running; report progress through report.
	// Resource changes on existing devices may be ignored (devices are throwaway).
	Ensure(ctx context.Context, spec *agentv1.DeviceSpec, report Reporter) error

	// Destroy stops and deletes the device and all its storage. Must succeed
	// (return nil) if the device does not exist.
	Destroy(ctx context.Context, deviceID string) error

	// List returns the status of every device this driver manages on this host
	// (used after agent restarts to rebuild state). Only devices created by
	// Tossbox must be returned (e.g. identified by name prefix or metadata).
	List(ctx context.Context) ([]*agentv1.DeviceStatus, error)

	// Status returns the current status of one device. Must return a status
	// with State DEVICE_STATE_DESTROYED (and nil error) if it does not exist.
	// InternalIp must be filled once the device has an address reachable from
	// the agent (used for port forwarding and readiness checks).
	Status(ctx context.Context, deviceID string) (*agentv1.DeviceStatus, error)
}

// Reporter receives progress updates during Ensure. state is usually
// PROVISIONING; message is human readable ("Downloading image (42%)").
type Reporter func(state agentv1.DeviceState, message string)

// Images fetches and caches base images (implemented by internal/images).
type Images interface {
	// Fetch makes the image available locally and returns its path.
	// Supports https:// (download + cache + optional sha256) and local:<file>.
	// docker:// and oci:// are not handled here (drivers pull them themselves).
	Fetch(ctx context.Context, img *agentv1.ImageSpec, progress func(done, total int64)) (string, error)
}

// Licenses resolves Windows license keys (implemented by internal/licenses).
type Licenses interface {
	// Resolve returns the product key for a device ("" for LICENSE_MODE_NONE).
	// For pool mode it leases a free key from the node config, bound to deviceID.
	Resolve(deviceID string, spec *agentv1.LicenseSpec) (string, error)
	// Release frees a leased pool key (no-op if none).
	Release(deviceID string)
}

// Env is what the agent core hands to a driver factory.
type Env struct {
	NodeName string
	// DataDir is a per-driver persistent directory (e.g. /var/lib/tb-agent/libvirt).
	DataDir  string
	Images   Images
	Licenses Licenses
	Log      *slog.Logger
	// ADBPublicKey is the agent's own adb key (adb_keys format). Drivers
	// that enable adb authentication install it next to the user's keys so
	// readiness checks can authenticate.
	ADBPublicKey string
	// GPU hands out the node's GPU (render node, SR-IOV VFs). May be nil.
	GPU GPU
}

// GPU is implemented by internal/gpu.
type GPU interface {
	// RenderNode is the shared DRM render node ("" if none).
	RenderNode() string
	// VFs lists the SR-IOV virtual functions available to devices.
	VFs() []string
	// Acquire leases a virtual function to the device (idempotent).
	Acquire(deviceID string) (string, error)
	// Release frees the device's virtual function.
	Release(deviceID string)
}

// Network policies (DeviceSpec.network_policy).
const (
	PolicyInternet = "internet"
	PolicyOffline  = "offline"
)

// NetworkPolicy returns the spec's network policy ("" means internet) or an
// error for unknown values.
func NetworkPolicy(spec *agentv1.DeviceSpec) (string, error) {
	switch p := spec.GetNetworkPolicy(); p {
	case "", PolicyInternet:
		return PolicyInternet, nil
	case PolicyOffline:
		return PolicyOffline, nil
	default:
		return "", fmt.Errorf("unknown network policy %q", p)
	}
}

// WantsGPU reports whether the device should get a GPU.
func WantsGPU(spec *agentv1.DeviceSpec) bool { return spec.GetResources().GetGpus() > 0 }

// Factory creates a driver from its YAML config section (may be nil/empty).
type Factory func(env Env, cfg *yaml.Node) (Driver, error)

var (
	mu        sync.Mutex
	factories = map[string]Factory{}
)

// Register makes a driver available under name. Called from driver packages' init().
func Register(name string, f Factory) {
	mu.Lock()
	defer mu.Unlock()
	if _, dup := factories[name]; dup {
		panic("driver: duplicate registration of " + name)
	}
	factories[name] = f
}

// New instantiates a registered driver.
func New(name string, env Env, cfg *yaml.Node) (Driver, error) {
	mu.Lock()
	f, ok := factories[name]
	mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("driver: unknown driver %q (available: %v)", name, Names())
	}
	return f(env, cfg)
}

// Names lists registered drivers.
func Names() []string {
	mu.Lock()
	defer mu.Unlock()
	out := make([]string, 0, len(factories))
	for n := range factories {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// DecodeConfig decodes a driver config section into v (no-op if cfg is nil).
func DecodeConfig(cfg *yaml.Node, v any) error {
	if cfg == nil || cfg.Kind == 0 {
		return nil
	}
	return cfg.Decode(v)
}

// DomainName returns the conventional backend object name for a device
// ("tb-" + device id). Drivers should use it so Tossbox resources are
// recognizable and never collide with unrelated VMs/containers on the host.
func DomainName(deviceID string) string { return "tb-" + deviceID }

// EndpointResolver is an optional driver extension for services that are not
// served at internal_ip:internal_port (e.g. libvirt's VNC console, which QEMU
// serves on the host, or per-service ports of a remote worker). handled=false
// means "use internal_ip:internal_port"; handled=true with endpoint "" means
// "not reachable right now". endpoint is "host:port" as seen from the agent.
type EndpointResolver interface {
	ServiceEndpoint(ctx context.Context, deviceID, service string) (endpoint string, handled bool)
}
