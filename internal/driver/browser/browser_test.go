package browser

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/finn-freitag/tb-agent/internal/dockerapi"
	"github.com/finn-freitag/tb-agent/internal/driver"
	agentv1 "github.com/finn-freitag/tb-proto/gen/agent/v1"
)

func spec() *agentv1.DeviceSpec {
	return &agentv1.DeviceSpec{
		DeviceId: "b1", Hostname: "web-test", Driver: name, Power: agentv1.PowerState_POWER_STATE_RUNNING,
		Template: &agentv1.TemplateSpec{Id: "browser-chromium", Family: "browser", DefaultUser: "tossbox",
			Image: &agentv1.ImageSpec{Url: "docker://jlesage/chromium:latest", Format: "container"},
			Params: map[string]string{
				"browser.env.VNC_PASSWORD":                "{password}",
				"browser.env.WEB_AUTHENTICATION_USERNAME": "{user}",
				"browser.env.KEEP_APP_RUNNING":            "1",
				"browser.shm_mb":                          "2048",
				"other.param":                             "x",
			}},
		Resources: &agentv1.Resources{Vcpus: 2, RamMb: 2048},
		Services:  []*agentv1.ServiceSpec{{Name: "vnc", InternalPort: 5900, PublicPort: 45010, Username: "tossbox", Password: "s3cret-pw"}},
	}
}

func TestContainerConfig(t *testing.T) {
	d := New(driver.Env{}, Config{})
	c, network, offline, err := d.containerConfig(spec())
	if err != nil {
		t.Fatal(err)
	}
	if c.Image != "jlesage/chromium:latest" || network != "tossbox" || offline {
		t.Fatalf("%s %s %v", c.Image, network, offline)
	}
	if !slices.Equal(c.Env, []string{"KEEP_APP_RUNNING=1", "VNC_PASSWORD=s3cret-pw", "WEB_AUTHENTICATION_USERNAME=tossbox"}) {
		t.Errorf("env %v", c.Env)
	}
	hc := c.HostConfig
	if hc.ShmSize != 2048<<20 || hc.Memory != 2048<<20 || hc.NanoCPUs != 2e9 || hc.Privileged || !slices.Contains(hc.SecurityOpt, "no-new-privileges:true") || hc.PidsLimit == 0 {
		t.Errorf("hostconfig %+v", hc)
	}
	s := spec()
	s.NetworkPolicy = "offline"
	if _, network, offline, _ := d.containerConfig(s); network != "tossbox-offline" || !offline {
		t.Errorf("offline: %s %v", network, offline)
	}
	s = spec()
	s.Services[0].Password = ""
	if _, _, _, err := d.containerConfig(s); err == nil {
		t.Error("must refuse to start without password")
	}
	s = spec()
	s.Template.Image.Url = "https://x/y.qcow2"
	if _, _, _, err := d.containerConfig(s); err == nil {
		t.Error("non-docker image accepted")
	}
}

type fakeDocker struct {
	mu         sync.Mutex
	containers map[string]*dockerapi.ContainerInfo
	networks   map[string]bool
	created    dockerapi.ContainerConfig
}

func (f *fakeDocker) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /_ping", func(w http.ResponseWriter, r *http.Request) {})
	mux.HandleFunc("GET /images/{ref...}", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{}`)) })
	mux.HandleFunc("GET /networks/{name}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if !f.networks[r.PathValue("name")] {
			w.WriteHeader(404)
			return
		}
		w.Write([]byte(`{}`))
	})
	mux.HandleFunc("POST /networks/create", func(w http.ResponseWriter, r *http.Request) {
		var b struct{ Name string }
		json.NewDecoder(r.Body).Decode(&b)
		f.mu.Lock()
		f.networks[b.Name] = true
		f.mu.Unlock()
		w.WriteHeader(201)
		w.Write([]byte(`{}`))
	})
	mux.HandleFunc("POST /containers/create", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		json.NewDecoder(r.Body).Decode(&f.created)
		ci := &dockerapi.ContainerInfo{ID: "c1", Name: r.URL.Query().Get("name")}
		ci.State.Status = "created"
		ci.Config.Labels = f.created.Labels
		f.containers[ci.Name] = ci
		w.WriteHeader(201)
		w.Write([]byte(`{"Id":"c1"}`))
	})
	find := func(id string) *dockerapi.ContainerInfo {
		for _, c := range f.containers {
			if c.ID == id || c.Name == id {
				return c
			}
		}
		return nil
	}
	mux.HandleFunc("GET /containers/{id}/json", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		c := find(r.PathValue("id"))
		if c == nil {
			w.WriteHeader(404)
			return
		}
		json.NewEncoder(w).Encode(c)
	})
	mux.HandleFunc("GET /containers/{id}/stats", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{}`)) })
	mux.HandleFunc("POST /containers/{id}/{action}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		c := find(r.PathValue("id"))
		if c == nil {
			w.WriteHeader(404)
			return
		}
		switch r.PathValue("action") {
		case "start":
			c.State.Status, c.State.Running = "running", true
			c.NetworkSettings.Networks = map[string]struct {
				IPAddress string `json:"IPAddress"`
			}{"tossbox": {IPAddress: "172.25.0.7"}}
		case "stop":
			c.State.Status, c.State.Running = "exited", false
		}
		w.WriteHeader(204)
	})
	mux.HandleFunc("GET /containers/json", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		var out []map[string]any
		for _, c := range f.containers {
			out = append(out, map[string]any{"Id": c.ID, "Labels": c.Config.Labels})
		}
		json.NewEncoder(w).Encode(out)
	})
	mux.HandleFunc("DELETE /containers/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		for k, c := range f.containers {
			if c.ID == r.PathValue("id") || c.Name == r.PathValue("id") {
				delete(f.containers, k)
				w.WriteHeader(204)
				return
			}
		}
		w.WriteHeader(404)
	})
	return mux
}

func TestLifecycle(t *testing.T) {
	fd := &fakeDocker{containers: map[string]*dockerapi.ContainerInfo{}, networks: map[string]bool{}}
	srv := httptest.NewServer(fd.handler())
	defer srv.Close()
	d := New(driver.Env{}, Config{})
	d.docker = dockerapi.NewWithHTTPClient(srv.URL, srv.Client())
	ctx := context.Background()
	if c := d.Capabilities(ctx); !c.Healthy || !slices.Contains(c.NetworkPolicies, "offline") {
		t.Fatalf("%+v", c)
	}
	report := func(agentv1.DeviceState, string) {}
	if err := d.Ensure(ctx, spec(), report); err != nil {
		t.Fatal(err)
	}
	if !fd.networks["tossbox"] || !strings.HasPrefix(fd.created.Image, "jlesage/") {
		t.Fatalf("network/image: %v %s", fd.networks, fd.created.Image)
	}
	st, _ := d.Status(ctx, "b1")
	if st.State != agentv1.DeviceState_DEVICE_STATE_RUNNING || st.InternalIp != "172.25.0.7" {
		t.Fatalf("%+v", st)
	}
	if l, err := d.List(ctx); err != nil || len(l) != 1 {
		t.Fatal(l, err)
	}
	s := spec()
	s.Power = agentv1.PowerState_POWER_STATE_STOPPED
	if err := d.Ensure(ctx, s, report); err != nil {
		t.Fatal(err)
	}
	if st, _ := d.Status(ctx, "b1"); st.State != agentv1.DeviceState_DEVICE_STATE_STOPPED {
		t.Fatal(st)
	}
	if err := d.Destroy(ctx, "b1"); err != nil {
		t.Fatal(err)
	}
	if st, _ := d.Status(ctx, "b1"); st.State != agentv1.DeviceState_DEVICE_STATE_DESTROYED {
		t.Fatal(st)
	}
}
