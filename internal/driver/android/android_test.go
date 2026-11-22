package android

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/finn-freitag/tb-agent/internal/adbkey"
	"github.com/finn-freitag/tb-agent/internal/dockerapi"
	"github.com/finn-freitag/tb-agent/internal/driver"
	agentv1 "github.com/finn-freitag/tb-proto/gen/agent/v1"
	"gopkg.in/yaml.v3"
)

func spec(backend string) *agentv1.DeviceSpec {
	return &agentv1.DeviceSpec{
		DeviceId: "a1", Hostname: "pixel", Driver: name, Power: agentv1.PowerState_POWER_STATE_RUNNING,
		Template: &agentv1.TemplateSpec{Id: "android-15", Family: "android", GuestProfile: "android",
			Image:  &agentv1.ImageSpec{Url: "docker://redroid/redroid:15.0.0_64only-latest", Format: "container"},
			Params: map[string]string{"android.backend": backend, "redroid.dpi": "480"}},
		Resources: &agentv1.Resources{Vcpus: 2, RamMb: 3072, StorageGb: 8},
		Services:  []*agentv1.ServiceSpec{{Name: "adb", InternalPort: 5555, PublicPort: 40010}, {Name: "vnc", InternalPort: 5900, PublicPort: 40011}},
	}
}

func TestConfigDefaults(t *testing.T) {
	var n yaml.Node
	yaml.Unmarshal([]byte("redroid:\n  width: 1080\n"), &n)
	var c Config
	if err := driver.DecodeConfig(n.Content[0], &c); err != nil {
		t.Fatal(err)
	}
	d, err := New(driver.Env{DataDir: t.TempDir()}, c)
	if err != nil {
		t.Fatal(err)
	}
	if d.cfg.DefaultBackend != "redroid" || d.backends["redroid"].(*redroid).cfg.Width != 1080 {
		t.Fatalf("%+v", d.cfg)
	}
	if _, err := New(driver.Env{}, Config{DefaultBackend: "cuttlefish", Redroid: RedroidConfig{Enabled: true}}); err == nil {
		t.Error("expected error: default backend not enabled")
	}
	if _, err := New(driver.Env{}, Config{Cuttlefish: CuttlefishConfig{Enabled: true}}); err == nil {
		t.Error("expected error: host_package_dir missing")
	}
}

func TestRedroidContainerConfig(t *testing.T) {
	r := newRedroid(driver.Env{DataDir: "/var/lib/tb"}, RedroidConfig{})
	c := r.containerConfig(spec("redroid"))
	if c.Image != "redroid/redroid:15.0.0_64only-latest" || !c.HostConfig.Privileged {
		t.Fatalf("%+v", c)
	}
	if !slices.Contains(c.Cmd, "androidboot.redroid_dpi=480") || !slices.Contains(c.Cmd, "androidboot.use_memfd=true") || !slices.Contains(c.Cmd, "androidboot.redroid_width=720") {
		t.Error(c.Cmd)
	}
	if c.HostConfig.Binds[0] != "/var/lib/tb/redroid/a1:/data" || c.HostConfig.Memory != 3072<<20 || c.HostConfig.NanoCPUs != 2e9 {
		t.Errorf("%+v", c.HostConfig)
	}
	if c.Labels[labelBackend] != "redroid" || c.Labels[labelDevice] != "a1" {
		t.Error(c.Labels)
	}
}

type fakeDocker struct {
	mu         sync.Mutex
	containers map[string]*dockerapi.ContainerInfo
	networks   map[string]map[string]string // created networks -> options
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
	mux.HandleFunc("GET /images/{ref...}", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{}`)) })
	mux.HandleFunc("POST /containers/create", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		var cfg dockerapi.ContainerConfig
		json.NewDecoder(r.Body).Decode(&cfg)
		n := r.URL.Query().Get("name")
		ci := &dockerapi.ContainerInfo{ID: n}
		ci.State.Status = "created"
		ci.Config.Labels = cfg.Labels
		f.containers[n] = ci
		w.Write([]byte(`{"Id":"` + n + `"}`))
	})
	mux.HandleFunc("GET /containers/json", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		var out []dockerapi.ContainerSummary
		for _, c := range f.containers {
			out = append(out, dockerapi.ContainerSummary{ID: c.ID, Labels: c.Config.Labels})
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
		c := f.containers[r.PathValue("id")]
		if r.PathValue("action") == "start" {
			c.State.Status, c.State.Running = "running", true
			c.NetworkSettings.IPAddress = "172.17.0.20"
		} else {
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

func TestRedroidLifecycle(t *testing.T) {
	fd := &fakeDocker{containers: map[string]*dockerapi.ContainerInfo{}}
	srv := httptest.NewServer(fd.handler())
	defer srv.Close()
	userKey, err := adbkey.LoadOrCreate(filepath.Join(t.TempDir(), "user"))
	if err != nil {
		t.Fatal(err)
	}
	d, err := New(driver.Env{DataDir: t.TempDir(), ADBPublicKey: "AGENTKEY tossbox-agent@n1"}, Config{})
	if err != nil {
		t.Fatal(err)
	}
	r := d.backends["redroid"].(*redroid)
	r.docker = dockerapi.NewWithHTTPClient(srv.URL, srv.Client())
	r.binderCheck = func() error { return nil }
	ctx := context.Background()
	if _, ok := d.ServiceEndpoint(ctx, "a1", "adb"); ok {
		t.Fatal("redroid must use internal_ip")
	}
	caps := d.Capabilities(ctx)
	if !caps.Healthy || !slices.Equal(caps.Backends, []string{"redroid"}) || !slices.Equal(caps.Services, []string{"adb"}) {
		t.Fatalf("%+v", caps)
	}
	report := func(agentv1.DeviceState, string) {}
	first := spec("redroid")
	first.AdbAuthorizedKeys = []string{userKey.PublicKey("me@laptop"), "not-a-key"}
	if err := d.Ensure(ctx, first, report); err != nil {
		t.Fatal(err)
	}
	keys, err := os.ReadFile(filepath.Join(r.dataDir("a1"), "misc", "adb", "adb_keys"))
	if err != nil || string(keys) != userKey.PublicKey("me@laptop")+"\nAGENTKEY tossbox-agent@n1\n" {
		t.Fatalf("adb_keys = %q, %v", keys, err)
	}
	if !slices.Contains(r.containerConfig(first).Cmd, "ro.adb.secure=1") {
		t.Error("adb auth not enabled")
	}
	st, _ := d.Status(ctx, "a1")
	if st.State != agentv1.DeviceState_DEVICE_STATE_RUNNING || st.InternalIp != "172.17.0.20" {
		t.Fatal(st)
	}
	s := spec("redroid")
	s.Power = agentv1.PowerState_POWER_STATE_STOPPED
	if err := d.Ensure(ctx, s, report); err != nil {
		t.Fatal(err)
	}
	if st, _ := d.Status(ctx, "a1"); st.State != agentv1.DeviceState_DEVICE_STATE_STOPPED {
		t.Fatal(st)
	}
	if l, err := d.List(ctx); err != nil || len(l) != 1 {
		t.Fatal(l, err)
	}
	if err := d.Destroy(ctx, "a1"); err != nil {
		t.Fatal(err)
	}
	if st, _ := d.Status(ctx, "a1"); st.State != agentv1.DeviceState_DEVICE_STATE_DESTROYED {
		t.Fatal(st)
	}
	r.binderCheck = func() error { return io.EOF }
	if c := d.Capabilities(ctx); c.Healthy || !strings.Contains(c.Message, "redroid") {
		t.Fatal(c)
	}
	// Images that need ashmem fail fast on kernels without it.
	r.ashmemCheck = func() bool { return false }
	s = spec("redroid")
	s.Template.Params["redroid.requires_ashmem"] = "true"
	if err := d.Ensure(ctx, s, report); err == nil || !strings.Contains(err.Error(), "ashmem") {
		t.Fatalf("want ashmem error, got %v", err)
	}
	r.ashmemCheck = func() bool { return true }
	r.binderCheck = func() error { return nil }
	if err := d.Ensure(ctx, s, report); err != nil {
		t.Fatal(err)
	}
}

func TestCuttlefish(t *testing.T) {
	pkg := t.TempDir()
	os.MkdirAll(filepath.Join(pkg, "bin"), 0o755)
	os.WriteFile(filepath.Join(pkg, "bin", "launch_cvd"), nil, 0o755)
	d, err := New(driver.Env{DataDir: t.TempDir()}, Config{Cuttlefish: CuttlefishConfig{Enabled: true, HostPackageDir: pkg, ImagesDir: "/imgs", MaxInstances: 2}})
	if err != nil {
		t.Fatal(err)
	}
	cf := d.backends["cuttlefish"].(*cuttlefish)
	var calls []string
	cf.run = func(ctx context.Context, dir string, env []string, bin string, args ...string) error {
		calls = append(calls, filepath.Base(bin)+" "+strings.Join(args, " "))
		if env[0] != "HOME="+dir {
			t.Errorf("HOME not set: %v", env)
		}
		return nil
	}
	ctx := context.Background()
	report := func(agentv1.DeviceState, string) {}
	s := spec("cuttlefish")
	s.Template.Image = &agentv1.ImageSpec{Url: "local:aosp-15"}
	if err := d.Ensure(ctx, s, report); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || !strings.Contains(calls[0], "--base_instance_num=1") || !strings.Contains(calls[0], "--system_image_dir=/imgs/aosp-15") ||
		!strings.Contains(calls[0], "--data_policy=always_create") || !strings.Contains(calls[0], "--start_vnc_server=true") || !strings.Contains(calls[0], "--memory_mb=3072") {
		t.Fatal(calls)
	}
	if _, err := os.Lstat(filepath.Join(cf.home("a1"), "bin")); err != nil {
		t.Error("host package not linked")
	}
	st, _ := d.Status(ctx, "a1")
	if st.State != agentv1.DeviceState_DEVICE_STATE_RUNNING {
		t.Fatal(st)
	}
	if ep, ok := d.ServiceEndpoint(ctx, "a1", "adb"); !ok || ep != "127.0.0.1:6520" {
		t.Fatal("adb endpoint", ep, ok)
	}
	if ep, ok := d.ServiceEndpoint(ctx, "a1", "vnc"); !ok || ep != "127.0.0.1:6444" {
		t.Fatal("vnc endpoint", ep, ok)
	}
	if _, ok := d.ServiceEndpoint(ctx, "unknown", "adb"); ok {
		t.Fatal("unknown device must not be handled")
	}
	// second device gets instance 2, a third fails
	s2 := spec("cuttlefish")
	s2.DeviceId = "a2"
	if err := d.Ensure(ctx, s2, report); err != nil {
		t.Fatal(err)
	}
	if ep, _ := d.ServiceEndpoint(ctx, "a2", "adb"); ep != "127.0.0.1:6521" {
		t.Error("instance 2 adb endpoint", ep)
	}
	s3 := spec("cuttlefish")
	s3.DeviceId = "a3"
	if err := d.Ensure(ctx, s3, report); err == nil {
		t.Error("expected no free instance")
	}
	// stop + restart uses existing data
	s.Power = agentv1.PowerState_POWER_STATE_STOPPED
	d.Ensure(ctx, s, report)
	if st, _ := d.Status(ctx, "a1"); st.State != agentv1.DeviceState_DEVICE_STATE_STOPPED {
		t.Fatal(st)
	}
	if ep, ok := d.ServiceEndpoint(ctx, "a1", "adb"); !ok || ep != "" {
		t.Fatal("stopped device must be unreachable", ep, ok)
	}
	s.Power = agentv1.PowerState_POWER_STATE_RUNNING
	d.Ensure(ctx, s, report)
	if last := calls[len(calls)-1]; !strings.Contains(last, "--data_policy=use_existing") {
		t.Error(last)
	}
	if l, _ := d.List(ctx); len(l) != 2 {
		t.Fatal(l)
	}
	for _, id := range []string{"a1", "a2"} {
		if err := d.Destroy(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	if l, _ := d.List(ctx); len(l) != 0 {
		t.Fatal(l)
	}
}
