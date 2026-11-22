// Package orchard implements the "orchard" driver: macOS (and optionally
// Linux) Tart VMs managed through an Orchard controller's REST API
// (github.com/cirruslabs/orchard, api/openapi.yaml).
//
// Network model (two modes):
//   - default: the device's internal IP is the VM's IP on its worker
//     (GET /v1/vms/{name}/ip), only reachable from the worker host itself, so
//     the agent must run on the worker Mac (or route to its VM network).
//   - use_endpoints: every service is declared as an Orchard endpoint (a port
//     on the worker forwarding to the VM); the driver reports
//     <worker address>:<workerPort> via driver.EndpointResolver, so the agent
//     can run anywhere that reaches the workers.
package orchard

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/finn-freitag/tb-agent/internal/driver"
	agentv1 "github.com/finn-freitag/tb-proto/gen/agent/v1"
	"gopkg.in/yaml.v3"
)

const name = "orchard"

// Orchard worker resource names (pkg/resource/v1/resources.go).
const (
	resourceTartVMs = "org.cirruslabs.tart-vms"
)

// Config is the driver's node config section.
//
//	drivers:
//	  orchard:
//	    url: https://127.0.0.1:6120          # controller
//	    service_account_name: tossbox
//	    service_account_token: <token>
//	    ca_file: ""                           # PEM CA for the controller cert
//	    insecure_skip_verify: false
//	    slots: 0                              # override total VM slots (0 = from workers)
//	    image_username: admin                 # credentials baked into the images
//	    image_password: admin
//	    labels: {}                            # worker labels new VMs require
//	    allow_linux: false                    # also offer Linux Tart images
//	    use_endpoints: false                  # expose services as Orchard worker endpoints
//	    worker_addresses: {}                  # worker name -> address as reached from the agent
//	    default_worker_address: 127.0.0.1     # for workers not listed above
type Config struct {
	UseEndpoints         bool              `yaml:"use_endpoints"`
	WorkerAddresses      map[string]string `yaml:"worker_addresses"`
	DefaultWorkerAddress string            `yaml:"default_worker_address"`
	URL                  string            `yaml:"url"`
	ServiceAccountName   string            `yaml:"service_account_name"`
	ServiceAccountToken  string            `yaml:"service_account_token"`
	CAFile               string            `yaml:"ca_file"`
	InsecureSkipVerify   bool              `yaml:"insecure_skip_verify"`
	Slots                int32             `yaml:"slots"`
	ImageUsername        string            `yaml:"image_username"`
	ImagePassword        string            `yaml:"image_password"`
	Labels               map[string]string `yaml:"labels"`
	AllowLinux           bool              `yaml:"allow_linux"`
}

func (c *Config) defaults() {
	if c.URL == "" {
		c.URL = "http://127.0.0.1:6120"
	}
	if c.ImageUsername == "" {
		c.ImageUsername = "admin"
	}
	if c.ImagePassword == "" {
		c.ImagePassword = "admin"
	}
	if c.DefaultWorkerAddress == "" {
		c.DefaultWorkerAddress = "127.0.0.1"
	}
}

// Driver is the orchard driver.
type Driver struct {
	cfg  Config
	env  driver.Env
	hc   *http.Client
	base *url.URL
}

func init() {
	driver.Register(name, func(env driver.Env, node *yaml.Node) (driver.Driver, error) {
		var cfg Config
		if err := driver.DecodeConfig(node, &cfg); err != nil {
			return nil, fmt.Errorf("orchard config: %w", err)
		}
		return New(env, cfg)
	})
}

// New creates the driver.
func New(env driver.Env, cfg Config) (*Driver, error) {
	cfg.defaults()
	u, err := url.Parse(cfg.URL)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("orchard: invalid url %q", cfg.URL)
	}
	tlsCfg := &tls.Config{InsecureSkipVerify: cfg.InsecureSkipVerify} //nolint:gosec // opt-in
	if cfg.CAFile != "" {
		pem, err := os.ReadFile(cfg.CAFile)
		if err != nil {
			return nil, fmt.Errorf("orchard: ca_file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("orchard: ca_file contains no certificates")
		}
		tlsCfg.RootCAs = pool
	}
	hc := &http.Client{Timeout: 60 * time.Second, Transport: &http.Transport{TLSClientConfig: tlsCfg}}
	return &Driver{cfg: cfg, env: env, hc: hc, base: u}, nil
}

// Name implements driver.Driver.
func (d *Driver) Name() string { return name }

var errNotFound = errors.New("orchard: not found")

func (d *Driver) do(ctx context.Context, method, path string, query url.Values, in, out any) error {
	u := d.base.JoinPath("v1", path)
	if query != nil {
		u.RawQuery = query.Encode()
	}
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	if d.cfg.ServiceAccountName != "" && d.cfg.ServiceAccountToken != "" {
		req.SetBasicAuth(d.cfg.ServiceAccountName, d.cfg.ServiceAccountToken)
	}
	resp, err := d.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return errNotFound
	}
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		return fmt.Errorf("orchard: %s %s: HTTP %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// startupScript is the VMSpec.startup_script object.
type startupScript struct {
	ScriptContent string            `json:"script_content"`
	Env           map[string]string `json:"env,omitempty"`
}

// vm is the Orchard VM resource (VMMeta + VMSpec + VMState), subset.
type vm struct {
	Name          string            `json:"name"`
	OS            string            `json:"os,omitempty"`
	Image         string            `json:"image"`
	CPU           int32             `json:"cpu,omitempty"`
	Memory        int64             `json:"memory,omitempty"`
	DiskSize      int64             `json:"diskSize,omitempty"`
	Headless      bool              `json:"headless"`
	Username      string            `json:"username,omitempty"`
	Password      string            `json:"password,omitempty"`
	StartupScript *startupScript    `json:"startup_script,omitempty"`
	RestartPolicy string            `json:"restart_policy,omitempty"`
	Labels        map[string]string `json:"labels,omitempty"`
	PowerState    string            `json:"powerState,omitempty"`
	Endpoints     []endpointSpec    `json:"endpoints,omitempty"`
	Observed      []endpointStatus  `json:"observedEndpoints,omitempty"`
	Status        string            `json:"status,omitempty"`
	StatusMessage string            `json:"status_message,omitempty"`
	Worker        string            `json:"worker,omitempty"`
}

// endpointSpec / endpointStatus: Orchard EndpointSpec / EndpointStatus.
type endpointSpec struct {
	Name     string `json:"name"`
	Protocol string `json:"protocol"`
	Target   struct {
		VM struct {
			Port int32 `json:"port"`
		} `json:"vm"`
	} `json:"target"`
}

type endpointStatus struct {
	Name       string `json:"name"`
	Protocol   string `json:"protocol"`
	WorkerPort int32  `json:"workerPort"`
	State      string `json:"state"`
	Message    string `json:"message"`
}

var defaultPorts = map[string]int32{"ssh": 22, "vnc": 5900, "http": 80, "https": 443}

type worker struct {
	Name      string           `json:"name"`
	Resources map[string]int64 `json:"resources"`
}

// Capabilities implements driver.Driver.
func (d *Driver) Capabilities(ctx context.Context) *agentv1.DriverCapabilities {
	caps := &agentv1.DriverCapabilities{
		Name:     name,
		Families: []string{"macos"},
		Services: []string{"ssh", "vnc", "http", "https"},
		Healthy:  true,
	}
	if d.cfg.AllowLinux {
		caps.Families = append(caps.Families, "linux")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var workers []worker
	if err := d.do(ctx, http.MethodGet, "/workers", nil, nil, &workers); err != nil {
		caps.Healthy = false
		caps.Message = "orchard controller not reachable: " + err.Error()
		caps.FixedSlots = d.cfg.Slots
		return caps
	}
	caps.FixedSlots = slots(d.cfg.Slots, workers)
	if len(workers) == 0 {
		caps.Healthy = false
		caps.Message = "no Orchard workers connected"
	}
	return caps
}

// slots computes the number of macOS VM slots: configured value, else the
// sum of the workers' tart-vms resource, else 2 per worker (Apple's limit).
func slots(configured int32, workers []worker) int32 {
	if configured > 0 {
		return configured
	}
	var n int64
	for _, w := range workers {
		if v, ok := w.Resources[resourceTartVMs]; ok {
			n += v
		} else {
			n += 2
		}
	}
	return int32(n)
}

// buildVM maps a spec to the Orchard create request (pure, unit tested).
func (d *Driver) buildVM(spec *agentv1.DeviceSpec) (vm, error) {
	tpl := spec.GetTemplate()
	img := tpl.GetImage().GetUrl()
	if !strings.HasPrefix(img, "oci://") {
		return vm{}, fmt.Errorf("orchard: unsupported image url %q (want oci://<ref>)", img)
	}
	osName := "darwin"
	if tpl.GetFamily() == "linux" {
		if !d.cfg.AllowLinux {
			return vm{}, errors.New("orchard: Linux VMs are not enabled on this node (allow_linux)")
		}
		osName = "linux"
	}
	res := spec.GetResources()
	v := vm{
		Name:          driver.DomainName(spec.DeviceId),
		OS:            osName,
		Image:         strings.TrimPrefix(img, "oci://"),
		CPU:           res.GetVcpus(),
		Memory:        res.GetRamMb(),
		DiskSize:      res.GetStorageGb(),
		Headless:      true,
		Username:      firstNonEmpty(tpl.GetParams()["orchard.image_username"], d.cfg.ImageUsername),
		Password:      firstNonEmpty(tpl.GetParams()["orchard.image_password"], d.cfg.ImagePassword),
		RestartPolicy: "Never",
		Labels:        d.cfg.Labels,
	}
	if d.cfg.UseEndpoints {
		for _, svc := range spec.GetServices() {
			port := svc.GetInternalPort()
			if port == 0 {
				port = defaultPorts[svc.GetName()]
			}
			if port == 0 {
				continue
			}
			var e endpointSpec
			e.Name, e.Protocol, e.Target.VM.Port = svc.GetName(), "tcp", port
			v.Endpoints = append(v.Endpoints, e)
		}
	}
	user, password := credentials(spec)
	if osName == "darwin" && (password != "" || len(spec.GetSshAuthorizedKeys()) > 0) {
		v.StartupScript = &startupScript{
			ScriptContent: macSetupScript,
			Env: map[string]string{
				"TB_USER":         user,
				"TB_PASSWORD":     password,
				"TB_OLD_PASSWORD": v.Password,
				"TB_HOSTNAME":     spec.GetHostname(),
				"TB_SSH_KEYS":     strings.Join(spec.GetSshAuthorizedKeys(), "\n"),
			},
		}
	}
	return v, nil
}

// macSetupScript runs over SSH (as the image user, with the image password)
// after boot: sets the hostname, creates or updates the device user with the
// generated password, and installs the SSH keys. The image user's password is
// only changed when it is the device user.
const macSetupScript = `#!/bin/zsh
set -e
echo "$TB_OLD_PASSWORD" | sudo -S -v
if [ -n "$TB_HOSTNAME" ]; then
  sudo scutil --set HostName "$TB_HOSTNAME"
  sudo scutil --set LocalHostName "$TB_HOSTNAME"
  sudo scutil --set ComputerName "$TB_HOSTNAME"
fi
if [ -n "$TB_PASSWORD" ]; then
  if id "$TB_USER" >/dev/null 2>&1; then
    sudo dscl . -passwd "/Users/$TB_USER" "$TB_PASSWORD"
  else
    sudo sysadminctl -addUser "$TB_USER" -password "$TB_PASSWORD" -admin
  fi
fi
if [ -n "$TB_SSH_KEYS" ]; then
  H=$(dscl . -read "/Users/$TB_USER" NFSHomeDirectory | awk '{print $2}')
  sudo mkdir -p "$H/.ssh"
  printf '%s\n' "$TB_SSH_KEYS" | sudo tee -a "$H/.ssh/authorized_keys" >/dev/null
  sudo chown -R "$TB_USER" "$H/.ssh"
  sudo chmod 700 "$H/.ssh"
  sudo chmod 600 "$H/.ssh/authorized_keys"
fi
`

// Ensure implements driver.Driver.
func (d *Driver) Ensure(ctx context.Context, spec *agentv1.DeviceSpec, report driver.Reporter) error {
	vname := driver.DomainName(spec.DeviceId)
	var cur vm
	err := d.do(ctx, http.MethodGet, "/vms/"+url.PathEscape(vname), nil, nil, &cur)
	if errors.Is(err, errNotFound) {
		body, err := d.buildVM(spec)
		if err != nil {
			return err
		}
		report(agentv1.DeviceState_DEVICE_STATE_PROVISIONING, "Scheduling macOS VM on Orchard (pulling image if needed)")
		if err := d.do(ctx, http.MethodPost, "/vms", nil, body, &cur); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	stopped := cur.PowerState == "stopped" || cur.PowerState == "suspended"
	switch spec.Power {
	case agentv1.PowerState_POWER_STATE_STOPPED:
		if !stopped {
			// Orchard can transition a VM into "stopped" only once and cannot
			// start it again (see VMSpec.powerState).
			cur.PowerState = "stopped"
			return d.do(ctx, http.MethodPut, "/vms/"+url.PathEscape(vname), nil, specOnly(cur), nil)
		}
	default:
		if stopped {
			return errors.New("orchard: restarting a stopped VM is not supported by Orchard; recreate the device")
		}
	}
	return nil
}

// ServiceEndpoint implements driver.EndpointResolver. Without use_endpoints
// services are served at the VM IP (handled=false).
func (d *Driver) ServiceEndpoint(ctx context.Context, deviceID, service string) (string, bool) {
	if !d.cfg.UseEndpoints {
		return "", false
	}
	var v vm
	if err := d.do(ctx, http.MethodGet, "/vms/"+url.PathEscape(driver.DomainName(deviceID)), nil, nil, &v); err != nil {
		return "", !errors.Is(err, errNotFound)
	}
	for _, e := range v.Observed {
		if e.Name == service && e.State == "listening" && e.WorkerPort > 0 && v.Worker != "" {
			host := d.cfg.WorkerAddresses[v.Worker]
			if host == "" {
				host = d.cfg.DefaultWorkerAddress
			}
			return net.JoinHostPort(host, strconv.Itoa(int(e.WorkerPort))), true
		}
	}
	return "", true
}

// specOnly strips state fields before a PUT (VMSpec only).
func specOnly(v vm) vm {
	v.Status, v.StatusMessage, v.Worker, v.Observed = "", "", "", nil
	return v
}

// Destroy implements driver.Driver.
func (d *Driver) Destroy(ctx context.Context, deviceID string) error {
	err := d.do(ctx, http.MethodDelete, "/vms/"+url.PathEscape(driver.DomainName(deviceID)), nil, nil, nil)
	if errors.Is(err, errNotFound) {
		return nil
	}
	return err
}

// List implements driver.Driver.
func (d *Driver) List(ctx context.Context) ([]*agentv1.DeviceStatus, error) {
	var vms []vm
	if err := d.do(ctx, http.MethodGet, "/vms", nil, nil, &vms); err != nil {
		return nil, err
	}
	var out []*agentv1.DeviceStatus
	for i := range vms {
		id, ok := strings.CutPrefix(vms[i].Name, "tb-")
		if !ok || id == "" {
			continue
		}
		out = append(out, d.statusOf(ctx, id, &vms[i]))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].DeviceId < out[j].DeviceId })
	return out, nil
}

// Status implements driver.Driver.
func (d *Driver) Status(ctx context.Context, deviceID string) (*agentv1.DeviceStatus, error) {
	var v vm
	err := d.do(ctx, http.MethodGet, "/vms/"+url.PathEscape(driver.DomainName(deviceID)), nil, nil, &v)
	if errors.Is(err, errNotFound) {
		return &agentv1.DeviceStatus{DeviceId: deviceID, State: agentv1.DeviceState_DEVICE_STATE_DESTROYED, UpdatedUnix: time.Now().Unix()}, nil
	}
	if err != nil {
		return nil, err
	}
	return d.statusOf(ctx, deviceID, &v), nil
}

func (d *Driver) statusOf(ctx context.Context, id string, v *vm) *agentv1.DeviceStatus {
	st := &agentv1.DeviceStatus{DeviceId: id, Message: v.StatusMessage, UpdatedUnix: time.Now().Unix()}
	switch {
	case v.PowerState == "stopped" || v.PowerState == "suspended":
		st.State = agentv1.DeviceState_DEVICE_STATE_STOPPED
	case v.Status == "running":
		st.State = agentv1.DeviceState_DEVICE_STATE_RUNNING
	case v.Status == "failed":
		st.State = agentv1.DeviceState_DEVICE_STATE_FAILED
	default:
		st.State = agentv1.DeviceState_DEVICE_STATE_PROVISIONING
		if st.Message == "" {
			st.Message = "Waiting for an Orchard worker"
		}
	}
	if st.State == agentv1.DeviceState_DEVICE_STATE_RUNNING {
		var ip struct {
			IP string `json:"ip"`
		}
		if err := d.do(ctx, http.MethodGet, "/vms/"+url.PathEscape(v.Name)+"/ip", url.Values{"wait": {"0"}}, nil, &ip); err == nil {
			st.InternalIp = ip.IP
		}
	}
	return st
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
		user = "admin"
	}
	return user, password
}

func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}
