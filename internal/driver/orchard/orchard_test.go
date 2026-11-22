package orchard

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/finn-freitag/tb-agent/internal/driver"
	agentv1 "github.com/finn-freitag/tb-proto/gen/agent/v1"
)

func spec() *agentv1.DeviceSpec {
	return &agentv1.DeviceSpec{
		DeviceId: "m1", Hostname: "mac-1", Driver: name, Power: agentv1.PowerState_POWER_STATE_RUNNING,
		Template: &agentv1.TemplateSpec{Id: "macos-tahoe", Family: "macos", GuestProfile: "tart", DefaultUser: "admin",
			Image: &agentv1.ImageSpec{Url: "oci://ghcr.io/cirruslabs/macos-tahoe-base:latest"}},
		Resources:         &agentv1.Resources{Vcpus: 4, RamMb: 8192, StorageGb: 80},
		Services:          []*agentv1.ServiceSpec{{Name: "ssh", InternalPort: 22, Username: "admin", Password: "new-password-12345678"}},
		SshAuthorizedKeys: []string{"ssh-ed25519 AAA"},
	}
}

func TestSlots(t *testing.T) {
	ws := []worker{{Name: "a", Resources: map[string]int64{resourceTartVMs: 2}}, {Name: "b"}}
	if got := slots(0, ws); got != 4 {
		t.Error(got)
	}
	if got := slots(3, ws); got != 3 {
		t.Error(got)
	}
}

func TestBuildVM(t *testing.T) {
	d, err := New(driver.Env{}, Config{URL: "http://x"})
	if err != nil {
		t.Fatal(err)
	}
	v, err := d.buildVM(spec())
	if err != nil {
		t.Fatal(err)
	}
	if v.Name != "tb-m1" || v.Image != "ghcr.io/cirruslabs/macos-tahoe-base:latest" || v.CPU != 4 || v.Memory != 8192 || v.DiskSize != 80 || v.OS != "darwin" {
		t.Fatalf("%+v", v)
	}
	if v.Username != "admin" || v.Password != "admin" {
		t.Error("orchard must connect with the image credentials")
	}
	if v.StartupScript == nil || v.StartupScript.Env["TB_PASSWORD"] != "new-password-12345678" || v.StartupScript.Env["TB_OLD_PASSWORD"] != "admin" {
		t.Fatalf("%+v", v.StartupScript)
	}
	s := spec()
	s.Template.Image.Url = "docker://x"
	if _, err := d.buildVM(s); err == nil {
		t.Error("expected error for non-oci image")
	}
	s = spec()
	s.Template.Family = "linux"
	if _, err := d.buildVM(s); err == nil {
		t.Error("expected error for linux without allow_linux")
	}
}

type fakeOrchard struct {
	mu      sync.Mutex
	vms     map[string]*vm
	auth    string
	created vm
}

func (f *fakeOrchard) handler() http.Handler {
	mux := http.NewServeMux()
	wrap := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			f.mu.Lock()
			defer f.mu.Unlock()
			if u, p, ok := r.BasicAuth(); ok {
				f.auth = u + ":" + p
			}
			h(w, r)
		}
	}
	mux.HandleFunc("GET /v1/workers", wrap(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]worker{{Name: "mac-mini", Resources: map[string]int64{resourceTartVMs: 2}}})
	}))
	mux.HandleFunc("POST /v1/vms", wrap(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&f.created)
		v := f.created
		v.Status = "pending"
		f.vms[v.Name] = &v
		json.NewEncoder(w).Encode(v)
	}))
	mux.HandleFunc("GET /v1/vms", wrap(func(w http.ResponseWriter, r *http.Request) {
		out := []vm{{Name: "someone-else", Status: "running"}}
		for _, v := range f.vms {
			out = append(out, *v)
		}
		json.NewEncoder(w).Encode(out)
	}))
	mux.HandleFunc("GET /v1/vms/{name}", wrap(func(w http.ResponseWriter, r *http.Request) {
		v, ok := f.vms[r.PathValue("name")]
		if !ok {
			w.WriteHeader(404)
			return
		}
		json.NewEncoder(w).Encode(v)
	}))
	mux.HandleFunc("PUT /v1/vms/{name}", wrap(func(w http.ResponseWriter, r *http.Request) {
		v, ok := f.vms[r.PathValue("name")]
		if !ok {
			w.WriteHeader(404)
			return
		}
		var upd vm
		json.NewDecoder(r.Body).Decode(&upd)
		v.PowerState = upd.PowerState
		json.NewEncoder(w).Encode(v)
	}))
	mux.HandleFunc("GET /v1/vms/{name}/ip", wrap(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"ip":"192.168.64.7"}`))
	}))
	mux.HandleFunc("DELETE /v1/vms/{name}", wrap(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := f.vms[r.PathValue("name")]; !ok {
			w.WriteHeader(404)
			return
		}
		delete(f.vms, r.PathValue("name"))
	}))
	return mux
}

func TestLifecycle(t *testing.T) {
	fo := &fakeOrchard{vms: map[string]*vm{}}
	srv := httptest.NewServer(fo.handler())
	defer srv.Close()
	d, err := New(driver.Env{}, Config{URL: srv.URL, ServiceAccountName: "tb", ServiceAccountToken: "tok"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	caps := d.Capabilities(ctx)
	if !caps.Healthy || caps.FixedSlots != 2 {
		t.Fatalf("%+v", caps)
	}
	if fo.auth != "tb:tok" {
		t.Error("basic auth not sent:", fo.auth)
	}
	report := func(agentv1.DeviceState, string) {}
	if err := d.Ensure(ctx, spec(), report); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(fo.created.StartupScript.ScriptContent, "dscl") {
		t.Error("startup script missing")
	}
	st, _ := d.Status(ctx, "m1")
	if st.State != agentv1.DeviceState_DEVICE_STATE_PROVISIONING {
		t.Fatal(st)
	}
	fo.mu.Lock()
	fo.vms["tb-m1"].Status = "running"
	fo.mu.Unlock()
	st, _ = d.Status(ctx, "m1")
	if st.State != agentv1.DeviceState_DEVICE_STATE_RUNNING || st.InternalIp != "192.168.64.7" {
		t.Fatal(st)
	}
	if err := d.Ensure(ctx, spec(), report); err != nil { // idempotent
		t.Fatal(err)
	}
	list, err := d.List(ctx)
	if err != nil || len(list) != 1 || list[0].DeviceId != "m1" {
		t.Fatal(list, err)
	}
	s := spec()
	s.Power = agentv1.PowerState_POWER_STATE_STOPPED
	if err := d.Ensure(ctx, s, report); err != nil {
		t.Fatal(err)
	}
	if st, _ := d.Status(ctx, "m1"); st.State != agentv1.DeviceState_DEVICE_STATE_STOPPED {
		t.Fatal(st)
	}
	if err := d.Ensure(ctx, spec(), report); err == nil {
		t.Error("restart of stopped orchard VM must fail")
	}
	if err := d.Destroy(ctx, "m1"); err != nil {
		t.Fatal(err)
	}
	if err := d.Destroy(ctx, "m1"); err != nil {
		t.Fatal(err)
	}
	if st, _ := d.Status(ctx, "m1"); st.State != agentv1.DeviceState_DEVICE_STATE_DESTROYED {
		t.Fatal(st)
	}
}

func TestUnreachable(t *testing.T) {
	d, _ := New(driver.Env{}, Config{URL: "http://127.0.0.1:1", Slots: 2})
	c := d.Capabilities(context.Background())
	if c.Healthy || c.FixedSlots != 2 {
		t.Fatal(c)
	}
}

func TestEndpoints(t *testing.T) {
	fo := &fakeOrchard{vms: map[string]*vm{}}
	srv := httptest.NewServer(fo.handler())
	defer srv.Close()
	d, _ := New(driver.Env{}, Config{URL: srv.URL, UseEndpoints: true, WorkerAddresses: map[string]string{"mac-mini": "100.64.0.7"}})
	ctx := context.Background()
	s := spec()
	s.Services = append(s.Services, &agentv1.ServiceSpec{Name: "vnc"})
	if err := d.Ensure(ctx, s, func(agentv1.DeviceState, string) {}); err != nil {
		t.Fatal(err)
	}
	if len(fo.created.Endpoints) != 2 || fo.created.Endpoints[0].Target.VM.Port != 22 || fo.created.Endpoints[1].Target.VM.Port != 5900 {
		t.Fatalf("%+v", fo.created.Endpoints)
	}
	if ep, ok := d.ServiceEndpoint(ctx, "m1", "ssh"); !ok || ep != "" {
		t.Fatal("not listening yet must be unreachable", ep, ok)
	}
	fo.mu.Lock()
	fo.vms["tb-m1"].Worker = "mac-mini"
	fo.vms["tb-m1"].Observed = []endpointStatus{{Name: "ssh", Protocol: "tcp", WorkerPort: 50022, State: "listening"}}
	fo.mu.Unlock()
	if ep, ok := d.ServiceEndpoint(ctx, "m1", "ssh"); !ok || ep != "100.64.0.7:50022" {
		t.Fatal(ep, ok)
	}
	plain, _ := New(driver.Env{}, Config{URL: srv.URL})
	if _, ok := plain.ServiceEndpoint(ctx, "m1", "ssh"); ok {
		t.Fatal("without use_endpoints the VM IP must be used")
	}
}
