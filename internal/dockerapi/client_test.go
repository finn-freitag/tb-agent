package dockerapi

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSplitImageRef(t *testing.T) {
	cases := map[string][2]string{
		"dockurr/windows":                      {"dockurr/windows", "latest"},
		"redroid/redroid:15.0.0_64only-latest": {"redroid/redroid", "15.0.0_64only-latest"},
		"localhost:5000/x/y":                   {"localhost:5000/x/y", "latest"},
		"localhost:5000/x/y:1":                 {"localhost:5000/x/y", "1"},
		"ghcr.io/a/b@sha256:abc":               {"ghcr.io/a/b", "sha256:abc"},
	}
	for in, want := range cases {
		i, tg := SplitImageRef(in)
		if i != want[0] || tg != want[1] {
			t.Errorf("%s: got %s %s", in, i, tg)
		}
	}
}

func TestUnixSocketAndLifecycle(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "docker.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Skip("unix sockets unavailable:", err)
	}
	var created ContainerConfig
	mux := http.NewServeMux()
	mux.HandleFunc("GET /_ping", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("OK")) })
	mux.HandleFunc("POST /images/create", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("fromImage") != "dockurr/windows" || r.URL.Query().Get("tag") != "latest" {
			t.Errorf("bad pull query %v", r.URL.Query())
		}
		w.Write([]byte(`{"status":"Pulling fs layer","id":"a"}
{"status":"Downloading","id":"a","progressDetail":{"current":50,"total":100}}
{"status":"Download complete","id":"a"}
{"status":"Status: Downloaded newer image"}
`))
	})
	mux.HandleFunc("POST /containers/create", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("name") != "tb-1" {
			t.Errorf("name %q", r.URL.Query().Get("name"))
		}
		json.NewDecoder(r.Body).Decode(&created)
		w.WriteHeader(201)
		w.Write([]byte(`{"Id":"cid"}`))
	})
	mux.HandleFunc("POST /containers/cid/start", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
	mux.HandleFunc("GET /containers/cid/json", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"Id":"cid","State":{"Status":"running","Running":true},"Config":{"Labels":{"tossbox.device":"1"}},"NetworkSettings":{"Networks":{"bridge":{"IPAddress":"172.17.0.5"}}}}`))
	})
	mux.HandleFunc("GET /containers/nope/json", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
		w.Write([]byte(`{"message":"No such container"}`))
	})
	mux.HandleFunc("DELETE /containers/nope", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(404) })
	mux.HandleFunc("GET /containers/json", func(w http.ResponseWriter, r *http.Request) {
		var f map[string][]string
		json.Unmarshal([]byte(r.URL.Query().Get("filters")), &f)
		if len(f["label"]) != 1 || f["label"][0] != "tossbox.driver=docker-qemu" || r.URL.Query().Get("all") != "true" {
			t.Errorf("bad filters %v", r.URL.Query())
		}
		w.Write([]byte(`[{"Id":"cid","Names":["/tb-1"],"State":"running","Labels":{"tossbox.device":"1"}}]`))
	})
	mux.HandleFunc("POST /containers/bad/start", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		w.Write([]byte(`{"message":"kvm missing"}`))
	})
	srv := &httptest.Server{Listener: ln, Config: &http.Server{Handler: mux}}
	srv.Start()
	defer srv.Close()

	c := New("unix://" + sock)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	var lastDone, lastTotal int64
	if err := c.PullImage(ctx, "dockurr/windows", func(d, tot int64, _ string) { lastDone, lastTotal = d, tot }); err != nil {
		t.Fatal(err)
	}
	if lastDone != 100 || lastTotal != 100 {
		t.Errorf("progress %d/%d", lastDone, lastTotal)
	}
	st := 120
	id, err := c.CreateContainer(ctx, "tb-1", ContainerConfig{Image: "dockurr/windows", Env: []string{"A=1"}, StopTimeout: &st,
		HostConfig: HostConfig{Devices: []DeviceMapping{{PathOnHost: "/dev/kvm", PathInContainer: "/dev/kvm", CgroupPermissions: "rwm"}}, CapAdd: []string{"NET_ADMIN"}}})
	if err != nil || id != "cid" {
		t.Fatal(id, err)
	}
	if created.HostConfig.Devices[0].PathOnHost != "/dev/kvm" || *created.StopTimeout != 120 {
		t.Errorf("created %+v", created)
	}
	if err := c.StartContainer(ctx, "cid"); err != nil {
		t.Fatal(err)
	}
	ci, err := c.InspectContainer(ctx, "cid")
	if err != nil || !ci.State.Running || ci.IP("bridge") != "172.17.0.5" || ci.IP("") != "172.17.0.5" {
		t.Fatalf("%+v %v", ci, err)
	}
	if _, err := c.InspectContainer(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want not found, got %v", err)
	}
	if err := c.RemoveContainer(ctx, "nope"); err != nil {
		t.Fatal(err)
	}
	list, err := c.ListContainers(ctx, "tossbox.driver=docker-qemu")
	if err != nil || len(list) != 1 || list[0].Labels["tossbox.device"] != "1" {
		t.Fatal(list, err)
	}
	err = c.StartContainer(ctx, "bad")
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != 500 || !strings.Contains(ae.Message, "kvm") {
		t.Fatalf("got %v", err)
	}
}

func TestPullError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"error":"manifest unknown"}` + "\n"))
	}))
	defer srv.Close()
	c := NewWithHTTPClient(srv.URL, srv.Client())
	if err := c.PullImage(context.Background(), "x:y", nil); err == nil || !strings.Contains(err.Error(), "manifest unknown") {
		t.Fatal(err)
	}
}
