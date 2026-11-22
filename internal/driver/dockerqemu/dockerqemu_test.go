package dockerqemu

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/finn-freitag/tb-agent/internal/cloudinit"
	"github.com/finn-freitag/tb-agent/internal/dockerapi"
	"github.com/finn-freitag/tb-agent/internal/driver"
	agentv1 "github.com/finn-freitag/tb-proto/gen/agent/v1"
	"gopkg.in/yaml.v3"
)

type fakeImages struct{ path string }

func (f fakeImages) Fetch(ctx context.Context, img *agentv1.ImageSpec, progress func(done, total int64)) (string, error) {
	progress(1, 2)
	return f.path, nil
}

type fakeLicenses struct{ released []string }

func (f *fakeLicenses) Resolve(id string, l *agentv1.LicenseSpec) (string, error) {
	if l.GetMode() == agentv1.LicenseMode_LICENSE_MODE_USER_KEY {
		return l.GetUserKey(), nil
	}
	return "", nil
}
func (f *fakeLicenses) Release(id string) { f.released = append(f.released, id) }

func envOf(c []string, key string) string {
	for _, e := range c {
		if strings.HasPrefix(e, key+"=") {
			return strings.TrimPrefix(e, key+"=")
		}
	}
	return ""
}

func winSpec() *agentv1.DeviceSpec {
	return &agentv1.DeviceSpec{
		DeviceId: "d1", Hostname: "win-1", Driver: name,
		Template: &agentv1.TemplateSpec{Id: "win11", Family: "windows", GuestProfile: "cloudbase-init", DefaultUser: "tossbox",
			Image:  &agentv1.ImageSpec{Url: "docker://dockurr/windows:latest", Format: "container"},
			Params: map[string]string{"docker_qemu.version": "11"}},
		Resources: &agentv1.Resources{Vcpus: 4, CpuCapPercent: 50, RamMb: 8192, StorageGb: 150},
		Services: []*agentv1.ServiceSpec{
			{Name: "rdp", InternalPort: 3389, PublicPort: 40001, Username: "tossbox", Password: "pw-123456789012345678"},
			{Name: "ssh", InternalPort: 22, PublicPort: 40000, Username: "tossbox", Password: "pw-123456789012345678"},
		},
		License: &agentv1.LicenseSpec{Mode: agentv1.LicenseMode_LICENSE_MODE_USER_KEY, UserKey: "AAAAA-BBBBB"},
		Power:   agentv1.PowerState_POWER_STATE_RUNNING,
	}
}

func TestConfigDecode(t *testing.T) {
	var n yaml.Node
	if err := yaml.Unmarshal([]byte("network: tbnet\nstop_timeout: 30s\nwindows_image: my/win\n"), &n); err != nil {
		t.Fatal(err)
	}
	var cfg Config
	if err := driver.DecodeConfig(n.Content[0], &cfg); err != nil {
		t.Fatal(err)
	}
	cfg.defaults(driver.Env{DataDir: "/x"})
	if cfg.Network != "tbnet" || cfg.StopTimeout.Seconds() != 30 || cfg.WindowsImage != "my/win" || cfg.LinuxImage != "qemux/qemu" || cfg.StorageDir != "/x/devices" {
		t.Fatalf("%+v", cfg)
	}
}

func TestPlanWindows(t *testing.T) {
	d := New(driver.Env{DataDir: "/data"}, Config{HostStorageDir: "/host/devices"})
	p, err := d.plan(winSpec(), "KEY-1")
	if err != nil {
		t.Fatal(err)
	}
	c := p.Config
	if p.Image != "dockurr/windows:latest" {
		t.Error("image", p.Image)
	}
	for k, want := range map[string]string{"USERNAME": "tossbox", "PASSWORD": "pw-123456789012345678", "RAM_SIZE": "8G", "CPU_CORES": "4", "DISK_SIZE": "150G", "VERSION": "11", "KEY": "KEY-1", "HOST": "win-1"} {
		if got := envOf(c.Env, k); got != want {
			t.Errorf("%s=%q want %q", k, got, want)
		}
	}
	if envOf(c.Env, "COMMAND") == "" {
		t.Error("ssh requested but no COMMAND")
	}
	if c.Labels[LabelDevice] != "d1" || c.Labels[LabelDriver] != name {
		t.Error("labels", c.Labels)
	}
	if c.HostConfig.Binds[0] != "/host/devices/d1/storage:/storage" {
		t.Error("binds", c.HostConfig.Binds)
	}
	if !slices.Contains(c.HostConfig.CapAdd, "NET_ADMIN") || len(c.HostConfig.Devices) != 2 || c.HostConfig.NanoCPUs == 0 {
		t.Errorf("hostconfig %+v", c.HostConfig)
	}
}

func TestPlanLinux(t *testing.T) {
	d := New(driver.Env{DataDir: "/data"}, Config{})
	spec := &agentv1.DeviceSpec{DeviceId: "d2", Hostname: "deb",
		Template: &agentv1.TemplateSpec{Id: "debian-13", Family: "linux", GuestProfile: "cloud-init", DefaultUser: "tossbox",
			Image:  &agentv1.ImageSpec{Url: "docker://qemux/qemu", Format: "container"},
			Params: map[string]string{"docker_qemu.boot_image": "https://x/debian.qcow2"}},
		Resources: &agentv1.Resources{Vcpus: 2, RamMb: 1536, StorageGb: 20}}
	p, err := d.plan(spec, "")
	if err != nil {
		t.Fatal(err)
	}
	if p.BootImage.GetUrl() != "https://x/debian.qcow2" || !p.SeedISO || p.BootFile != "/data/devices/d2/boot.qcow2" {
		t.Fatalf("%+v", p)
	}
	if envOf(p.Config.Env, "RAM_SIZE") != "1536M" || !strings.Contains(envOf(p.Config.Env, "ARGUMENTS"), "/seed.iso") {
		t.Error(p.Config.Env)
	}
	if !slices.Contains(p.Config.HostConfig.Binds, "/data/devices/d2/boot.qcow2:/boot.qcow2") {
		t.Error(p.Config.HostConfig.Binds)
	}
	spec.Template.Params = nil
	if _, err := d.plan(spec, ""); err == nil {
		t.Error("expected error without boot image")
	}
}

// fakeDocker is an in-memory Docker daemon good enough for the driver.
type fakeDocker struct {
	mu         sync.Mutex
	containers map[string]*dockerapi.ContainerInfo
	networks   map[string]map[string]string // created networks -> options
	created    dockerapi.ContainerConfig
}

func (f *fakeDocker) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /_ping", func(w http.ResponseWriter, r *http.Request) {})
	mux.HandleFunc("GET /networks/{name}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if _, ok := f.networks[r.PathValue("name")]; !ok {
			w.WriteHeader(404)
			return
		}
		w.Write([]byte(`{}`))
	})
	mux.HandleFunc("POST /networks/create", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Name    string
			Options map[string]string
		}
		json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		if f.networks == nil {
			f.networks = map[string]map[string]string{}
		}
		f.networks[body.Name] = body.Options
		f.mu.Unlock()
		w.WriteHeader(201)
		w.Write([]byte(`{"Id":"n1"}`))
	})
	mux.HandleFunc("GET /images/{ref...}", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(404) })
	mux.HandleFunc("POST /images/create", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"status":"ok"}` + "\n")) })
	mux.HandleFunc("POST /containers/create", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		json.NewDecoder(r.Body).Decode(&f.created)
		n := r.URL.Query().Get("name")
		ci := &dockerapi.ContainerInfo{ID: n, Name: "/" + n}
		ci.State.Status = "created"
		ci.Config.Labels = f.created.Labels
		f.containers[n] = ci
		w.Write([]byte(`{"Id":"` + n + `"}`))
	})
	mux.HandleFunc("GET /containers/json", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		var out []dockerapi.ContainerSummary
		for _, c := range f.containers {
			out = append(out, dockerapi.ContainerSummary{ID: c.ID, Labels: c.Config.Labels, State: c.State.Status})
		}
		json.NewEncoder(w).Encode(out)
	})
	mux.HandleFunc("GET /containers/{id}/json", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		c, ok := f.containers[r.PathValue("id")]
		if !ok {
			w.WriteHeader(404)
			return
		}
		json.NewEncoder(w).Encode(c)
	})
	mux.HandleFunc("POST /containers/{id}/{action}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		c, ok := f.containers[r.PathValue("id")]
		if !ok {
			w.WriteHeader(404)
			return
		}
		switch r.PathValue("action") {
		case "start":
			c.State.Status, c.State.Running = "running", true
			c.NetworkSettings.Networks = map[string]struct {
				IPAddress string `json:"IPAddress"`
			}{"bridge": {IPAddress: "172.17.0.9"}}
		case "stop":
			c.State.Status, c.State.Running = "exited", false
		}
		w.WriteHeader(204)
	})
	mux.HandleFunc("DELETE /containers/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		delete(f.containers, r.PathValue("id"))
		w.WriteHeader(204)
	})
	return mux
}

func TestLifecycle(t *testing.T) {
	fd := &fakeDocker{containers: map[string]*dockerapi.ContainerInfo{}}
	srv := httptest.NewServer(fd.handler())
	defer srv.Close()
	dir := t.TempDir()
	img := filepath.Join(dir, "src.qcow2")
	os.WriteFile(img, []byte("disk"), 0o600)
	lic := &fakeLicenses{}
	d := New(driver.Env{DataDir: dir, Images: fakeImages{img}, Licenses: lic}, Config{KVMDevice: img})
	d.docker = dockerapi.NewWithHTTPClient(srv.URL, srv.Client())
	var seeded []byte
	d.seedISO = func(ctx context.Context, path string, seed cloudinit.Seed) error {
		seeded = seed.UserData
		return os.WriteFile(path, seed.UserData, 0o600)
	}
	ctx := context.Background()

	if caps := d.Capabilities(ctx); !caps.Healthy {
		t.Fatal(caps.Message)
	}
	// Windows
	report := func(agentv1.DeviceState, string) {}
	if err := d.Ensure(ctx, winSpec(), report); err != nil {
		t.Fatal(err)
	}
	if fd.networks["tossbox"]["com.docker.network.bridge.enable_icc"] != "false" || fd.networks["tossbox"]["com.docker.network.bridge.name"] != "tb-docker" {
		t.Errorf("device network not isolated: %v", fd.networks)
	}
	if envOf(fd.created.Env, "KEY") != "AAAAA-BBBBB" {
		t.Error("license key not passed", fd.created.Env)
	}
	st, _ := d.Status(ctx, "d1")
	if st.State != agentv1.DeviceState_DEVICE_STATE_RUNNING || st.InternalIp != "172.17.0.9" {
		t.Fatalf("%+v", st)
	}
	// idempotent
	if err := d.Ensure(ctx, winSpec(), report); err != nil {
		t.Fatal(err)
	}
	// stop
	s := winSpec()
	s.Power = agentv1.PowerState_POWER_STATE_STOPPED
	if err := d.Ensure(ctx, s, report); err != nil {
		t.Fatal(err)
	}
	if st, _ := d.Status(ctx, "d1"); st.State != agentv1.DeviceState_DEVICE_STATE_STOPPED || st.InternalIp != "" {
		t.Fatalf("%+v", st)
	}
	// Linux with seed
	lin := &agentv1.DeviceSpec{DeviceId: "d2", Hostname: "deb", Power: agentv1.PowerState_POWER_STATE_RUNNING,
		Template:  &agentv1.TemplateSpec{Family: "linux", GuestProfile: "cloud-init", DefaultUser: "tossbox", Image: &agentv1.ImageSpec{Url: "https://x/deb.qcow2", Format: "qcow2"}},
		Resources: &agentv1.Resources{Vcpus: 1, RamMb: 1024},
		Services:  []*agentv1.ServiceSpec{{Name: "ssh", Password: "secret-secret-secret"}}}
	if err := d.Ensure(ctx, lin, report); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(seeded), "secret-secret-secret") {
		t.Error("seed missing password")
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "devices", "d2", "boot.qcow2")); string(b) != "disk" {
		t.Error("boot disk not copied")
	}
	list, err := d.List(ctx)
	if err != nil || len(list) != 2 {
		t.Fatal(list, err)
	}
	for _, id := range []string{"d1", "d2"} {
		if err := d.Destroy(ctx, id); err != nil {
			t.Fatal(err)
		}
		if st, _ := d.Status(ctx, id); st.State != agentv1.DeviceState_DEVICE_STATE_DESTROYED {
			t.Fatal(st)
		}
	}
	if err := d.Destroy(ctx, "unknown"); err != nil {
		t.Fatal(err)
	}
	if len(lic.released) != 3 {
		t.Error("released", lic.released)
	}
	if _, err := os.Stat(filepath.Join(dir, "devices", "d2")); !os.IsNotExist(err) {
		t.Error("device dir not removed")
	}
}

type fakeGPU struct{ node string }

func (f fakeGPU) RenderNode() string             { return f.node }
func (f fakeGPU) VFs() []string                  { return nil }
func (f fakeGPU) Acquire(string) (string, error) { return "", errors.New("no VFs") }
func (f fakeGPU) Release(string)                 {}

func TestPlanOfflineGPU(t *testing.T) {
	d := New(driver.Env{DataDir: "/data", GPU: fakeGPU{"/dev/dri/renderD128"}}, Config{})
	spec := &agentv1.DeviceSpec{DeviceId: "d3", Hostname: "deb", NetworkPolicy: "offline",
		Template: &agentv1.TemplateSpec{Id: "debian-13", Family: "linux", GuestProfile: "cloud-init", DefaultUser: "tossbox",
			Image:  &agentv1.ImageSpec{Url: "docker://qemux/qemu", Format: "container"},
			Params: map[string]string{"docker_qemu.boot_image": "https://x/debian.qcow2"}},
		Resources: &agentv1.Resources{Vcpus: 2, RamMb: 1536, StorageGb: 20, Gpus: 1}}
	p, err := d.plan(spec, "")
	if err != nil {
		t.Fatal(err)
	}
	if !p.Offline || p.Config.HostConfig.NetworkMode != "tossbox-offline" {
		t.Errorf("offline network not used: %+v", p.Config.HostConfig.NetworkMode)
	}
	if envOf(p.Config.Env, "GPU") != "Y" || p.Config.HostConfig.Devices[len(p.Config.HostConfig.Devices)-1].PathOnHost != "/dev/dri" {
		t.Errorf("GPU not passed: %v %v", p.Config.Env, p.Config.HostConfig.Devices)
	}
	spec.Template.Family = "windows"
	if _, err := d.plan(spec, ""); err == nil {
		t.Error("GPU for Windows guests must be rejected")
	}
	spec.NetworkPolicy = "lan"
	spec.Template.Family = "linux"
	if _, err := d.plan(spec, ""); err == nil {
		t.Error("unknown policy accepted")
	}
}
