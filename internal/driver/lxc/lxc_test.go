package lxc

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/finn-freitag/tb-agent/internal/driver"
	agentv1 "github.com/finn-freitag/tb-proto/gen/agent/v1"
	"gopkg.in/yaml.v3"
)

func spec() *agentv1.DeviceSpec {
	return &agentv1.DeviceSpec{
		DeviceId: "d1", Hostname: "deb", Driver: name, Power: agentv1.PowerState_POWER_STATE_RUNNING,
		Template: &agentv1.TemplateSpec{Id: "debian-13-lxc", Family: "linux", GuestProfile: "cloud-init", DefaultUser: "tossbox",
			Image: &agentv1.ImageSpec{Url: "incus:images:debian/13/cloud"}},
		Resources:         &agentv1.Resources{Vcpus: 2, CpuCapPercent: 25, RamMb: 2048, StorageGb: 10},
		Services:          []*agentv1.ServiceSpec{{Name: "ssh", InternalPort: 22, PublicPort: 40000, Username: "tossbox", Password: "pw-abcdefghijklmnopqrst"}},
		SshAuthorizedKeys: []string{"ssh-ed25519 AAAA test"},
	}
}

func TestParseImage(t *testing.T) {
	r := map[string]string{"images": "https://images.linuxcontainers.org"}
	s, a, err := parseImage("incus:images:debian/13/cloud", r)
	if err != nil || s != r["images"] || a != "debian/13/cloud" {
		t.Fatal(s, a, err)
	}
	for _, bad := range []string{"https://x", "incus:images", "incus:nope:debian/13", "incus::x"} {
		if _, _, err := parseImage(bad, r); err == nil {
			t.Errorf("expected error for %q", bad)
		}
	}
}

func TestConfig(t *testing.T) {
	var n yaml.Node
	yaml.Unmarshal([]byte("project: tb\nremotes:\n  mine: https://img.example\n"), &n)
	var c Config
	if err := driver.DecodeConfig(n.Content[0], &c); err != nil {
		t.Fatal(err)
	}
	c.defaults()
	if c.Project != "tb" || c.Remotes["mine"] == "" || c.Remotes["images"] == "" || c.Profiles[0] != "default" {
		t.Fatalf("%+v", c)
	}
}

func TestBuildCreate(t *testing.T) {
	d := New(driver.Env{}, Config{})
	d.cores = 8
	b, err := d.buildCreate(spec())
	if err != nil {
		t.Fatal(err)
	}
	if b.Name != "tb-d1" || b.Type != "container" || b.Source.Alias != "debian/13/cloud" || b.Source.Protocol != "simplestreams" {
		t.Fatalf("%+v", b)
	}
	if b.Config["limits.cpu"] != "2" || b.Config["limits.memory"] != "2048MiB" || b.Config["limits.cpu.allowance"] != "200ms/100ms" {
		t.Fatalf("%+v", b.Config)
	}
	if b.Devices["root"]["size"] != "10GiB" || b.Config[keyDevice] != "d1" {
		t.Fatalf("%+v", b.Devices)
	}
	if b.Config["security.nesting"] != "true" {
		t.Errorf("security.nesting = %q, want true by default", b.Config["security.nesting"])
	}
	ud := b.Config["cloud-init.user-data"]
	if !strings.Contains(ud, "pw-abcdefghijklmnopqrst") || !strings.Contains(ud, "ssh-ed25519 AAAA test") {
		t.Fatalf("user data: %s", ud)
	}
	s := spec()
	s.Template.Params = map[string]string{"lxc.type": "virtual-machine"}
	if _, err := d.buildCreate(s); err == nil {
		t.Error("VM must be rejected without allow_vms")
	}
}

// fakeIncus is a tiny in-memory Incus daemon.
type fakeIncus struct {
	mu        sync.Mutex
	instances map[string]*instance
	created   instancesPost
	ops       int
}

func (f *fakeIncus) async(w http.ResponseWriter) {
	f.ops++
	json.NewEncoder(w).Encode(map[string]any{"type": "async", "status_code": 100, "operation": "/1.0/operations/op1"})
}

func sync200(w http.ResponseWriter, md any) {
	json.NewEncoder(w).Encode(map[string]any{"type": "sync", "status": "Success", "status_code": 200, "metadata": md})
}

func notFound(w http.ResponseWriter) {
	w.WriteHeader(404)
	json.NewEncoder(w).Encode(map[string]any{"type": "error", "error": "Instance not found", "error_code": 404})
}

func (f *fakeIncus) handler(t *testing.T) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /1.0", func(w http.ResponseWriter, r *http.Request) { sync200(w, map[string]any{"api_status": "stable"}) })
	mux.HandleFunc("GET /1.0/operations/op1/wait", func(w http.ResponseWriter, r *http.Request) {
		sync200(w, map[string]any{"status": "Success", "status_code": 200})
	})
	mux.HandleFunc("POST /1.0/instances", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		json.NewDecoder(r.Body).Decode(&f.created)
		f.instances[f.created.Name] = &instance{Name: f.created.Name, Status: "Stopped", Config: f.created.Config}
		f.async(w)
	})
	mux.HandleFunc("GET /1.0/instances", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.URL.Query().Get("recursion") != "1" {
			t.Error("list without recursion")
		}
		var out []instance
		for _, in := range f.instances {
			out = append(out, *in)
		}
		out = append(out, instance{Name: "unrelated", Status: "Running", Config: map[string]string{}})
		sync200(w, out)
	})
	mux.HandleFunc("GET /1.0/instances/{name}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		in, ok := f.instances[r.PathValue("name")]
		if !ok {
			notFound(w)
			return
		}
		sync200(w, in)
	})
	mux.HandleFunc("GET /1.0/instances/{name}/state", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		in, ok := f.instances[r.PathValue("name")]
		if !ok {
			notFound(w)
			return
		}
		st := map[string]any{"status": in.Status, "disk": map[string]any{"root": map[string]any{"usage": 300 << 20}}}
		if in.Status == "Running" {
			st["network"] = map[string]any{
				"lo":   map[string]any{"addresses": []any{map[string]any{"family": "inet", "address": "127.0.0.1", "scope": "local"}}},
				"eth0": map[string]any{"addresses": []any{map[string]any{"family": "inet6", "address": "fd42::1", "scope": "global"}, map[string]any{"family": "inet", "address": "10.10.10.5", "scope": "global"}}},
			}
		}
		sync200(w, st)
	})
	mux.HandleFunc("PUT /1.0/instances/{name}/state", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		in, ok := f.instances[r.PathValue("name")]
		if !ok {
			notFound(w)
			return
		}
		var body struct{ Action string }
		json.NewDecoder(r.Body).Decode(&body)
		switch body.Action {
		case "start":
			in.Status = "Running"
		case "stop":
			in.Status = "Stopped"
		}
		f.async(w)
	})
	mux.HandleFunc("DELETE /1.0/instances/{name}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		in, ok := f.instances[r.PathValue("name")]
		if !ok {
			notFound(w)
			return
		}
		if in.Status == "Running" {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]any{"type": "error", "error": "Instance is running", "error_code": 400})
			return
		}
		delete(f.instances, in.Name)
		f.async(w)
	})
	return mux
}

func TestLifecycle(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "incus.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Skip(err)
	}
	fi := &fakeIncus{instances: map[string]*instance{}}
	srv := &httptest.Server{Listener: ln, Config: &http.Server{Handler: fi.handler(t)}}
	srv.Start()
	defer srv.Close()

	d := New(driver.Env{}, Config{Socket: sock})
	ctx := context.Background()
	if c := d.Capabilities(ctx); !c.Healthy {
		t.Fatal(c.Message)
	}
	report := func(agentv1.DeviceState, string) {}
	if err := d.Ensure(ctx, spec(), report); err != nil {
		t.Fatal(err)
	}
	st, err := d.Status(ctx, "d1")
	if err != nil || st.State != agentv1.DeviceState_DEVICE_STATE_RUNNING || st.InternalIp != "10.10.10.5" || st.StorageUsedMb != 300 {
		t.Fatalf("%+v %v", st, err)
	}
	ops := fi.ops
	if err := d.Ensure(ctx, spec(), report); err != nil || fi.ops != ops {
		t.Fatalf("not idempotent: %v ops %d->%d", err, ops, fi.ops)
	}
	s := spec()
	s.Power = agentv1.PowerState_POWER_STATE_STOPPED
	if err := d.Ensure(ctx, s, report); err != nil {
		t.Fatal(err)
	}
	if st, _ := d.Status(ctx, "d1"); st.State != agentv1.DeviceState_DEVICE_STATE_STOPPED {
		t.Fatal(st)
	}
	list, err := d.List(ctx)
	if err != nil || len(list) != 1 || list[0].DeviceId != "d1" {
		t.Fatal(list, err)
	}
	d.Ensure(ctx, spec(), report) // running again, Destroy must stop first
	if err := d.Destroy(ctx, "d1"); err != nil {
		t.Fatal(err)
	}
	if st, _ := d.Status(ctx, "d1"); st.State != agentv1.DeviceState_DEVICE_STATE_DESTROYED {
		t.Fatal(st)
	}
	if err := d.Destroy(ctx, "d1"); err != nil {
		t.Fatal(err)
	}

	// Unreachable socket -> unhealthy.
	bad := New(driver.Env{}, Config{Socket: filepath.Join(t.TempDir(), "nope.sock")})
	if c := bad.Capabilities(ctx); c.Healthy || c.Message == "" {
		t.Fatal("expected unhealthy")
	}
}
