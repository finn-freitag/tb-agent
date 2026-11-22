// Package dockerapi is a minimal Docker Engine API client (standard library
// only) used by the docker-qemu and android (redroid) drivers.
//
// It talks to the daemon over its unix socket (default /var/run/docker.sock)
// or over plain TCP ("tcp://host:2375"). Requests use unversioned API paths,
// so the daemon's current API version is used.
package dockerapi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultSocket is the default Docker daemon socket.
const DefaultSocket = "/var/run/docker.sock"

// ErrNotFound is returned when a container, image or volume does not exist.
var ErrNotFound = errors.New("docker: not found")

// Client is a Docker Engine API client.
type Client struct {
	hc   *http.Client
	base string // e.g. "http://docker"
}

// New creates a client. addr is a unix socket path (optionally prefixed with
// "unix://"), or "tcp://host:port" / "http://host:port". Empty = DefaultSocket.
func New(addr string) *Client {
	if addr == "" {
		addr = DefaultSocket
	}
	switch {
	case strings.HasPrefix(addr, "tcp://"):
		return &Client{hc: &http.Client{}, base: "http://" + strings.TrimPrefix(addr, "tcp://")}
	case strings.HasPrefix(addr, "http://"), strings.HasPrefix(addr, "https://"):
		return &Client{hc: &http.Client{}, base: strings.TrimSuffix(addr, "/")}
	}
	sock := strings.TrimPrefix(addr, "unix://")
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		},
	}
	return &Client{hc: &http.Client{Transport: tr}, base: "http://docker"}
}

// NewWithHTTPClient creates a client for an arbitrary base URL (used in tests).
func NewWithHTTPClient(base string, hc *http.Client) *Client {
	return &Client{hc: hc, base: strings.TrimSuffix(base, "/")}
}

// APIError is a non-2xx response from the daemon.
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string { return fmt.Sprintf("docker: %d: %s", e.Status, e.Message) }

func (c *Client) do(ctx context.Context, method, path string, query url.Values, in any) (*http.Response, error) {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(b)
	}
	u := c.base + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusNotFound {
		resp.Body.Close()
		return nil, ErrNotFound
	}
	if resp.StatusCode >= 300 && resp.StatusCode != http.StatusNotModified {
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		var m struct {
			Message string `json:"message"`
		}
		msg := strings.TrimSpace(string(b))
		if json.Unmarshal(b, &m) == nil && m.Message != "" {
			msg = m.Message
		}
		return nil, &APIError{Status: resp.StatusCode, Message: msg}
	}
	return resp, nil
}

func (c *Client) doJSON(ctx context.Context, method, path string, query url.Values, in, out any) error {
	resp, err := c.do(ctx, method, path, query, in)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// Ping checks that the daemon is reachable.
func (c *Client) Ping(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return c.doJSON(ctx, http.MethodGet, "/_ping", nil, nil, nil)
}

// SplitImageRef splits "repo[:tag][@digest]" into fromImage and tag for
// POST /images/create. Tag defaults to "latest".
func SplitImageRef(ref string) (image, tag string) {
	if i := strings.Index(ref, "@"); i >= 0 {
		return ref[:i], ref[i+1:]
	}
	slash := strings.LastIndex(ref, "/")
	if i := strings.LastIndex(ref, ":"); i > slash {
		return ref[:i], ref[i+1:]
	}
	return ref, "latest"
}

// ImageExists reports whether an image is present locally.
func (c *Client) ImageExists(ctx context.Context, ref string) (bool, error) {
	err := c.doJSON(ctx, http.MethodGet, "/images/"+url.PathEscape(ref)+"/json", nil, nil, nil)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

// PullProgress is called while pulling: done/total bytes summed over layers
// (total may be 0 while unknown) and the latest status line.
type PullProgress func(done, total int64, status string)

// PullImage pulls an image and blocks until done.
func (c *Client) PullImage(ctx context.Context, ref string, progress PullProgress) error {
	image, tag := SplitImageRef(ref)
	q := url.Values{"fromImage": {image}, "tag": {tag}}
	resp, err := c.do(ctx, http.MethodPost, "/images/create", q, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	type layer struct{ cur, tot int64 }
	layers := map[string]layer{}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		var m struct {
			Status         string `json:"status"`
			ID             string `json:"id"`
			Error          string `json:"error"`
			ProgressDetail struct {
				Current int64 `json:"current"`
				Total   int64 `json:"total"`
			} `json:"progressDetail"`
		}
		if json.Unmarshal(sc.Bytes(), &m) != nil {
			continue
		}
		if m.Error != "" {
			return fmt.Errorf("docker: pull %s: %s", ref, m.Error)
		}
		if m.ID != "" && m.ProgressDetail.Total > 0 && strings.HasPrefix(m.Status, "Downloading") {
			layers[m.ID] = layer{m.ProgressDetail.Current, m.ProgressDetail.Total}
		}
		if m.ID != "" && (m.Status == "Download complete" || m.Status == "Pull complete") {
			if l, ok := layers[m.ID]; ok {
				l.cur = l.tot
				layers[m.ID] = l
			}
		}
		if progress != nil {
			var d, t int64
			for _, l := range layers {
				d += l.cur
				t += l.tot
			}
			progress(d, t, m.Status)
		}
	}
	return sc.Err()
}

// ---------------------------------------------------------------- containers

// DeviceMapping maps a host device into a container.
type DeviceMapping struct {
	PathOnHost        string `json:"PathOnHost"`
	PathInContainer   string `json:"PathInContainer"`
	CgroupPermissions string `json:"CgroupPermissions"`
}

// RestartPolicy of a container.
type RestartPolicy struct {
	Name string `json:"Name"`
}

// HostConfig is the subset of the Docker HostConfig we use.
type HostConfig struct {
	Binds             []string        `json:"Binds,omitempty"`
	Devices           []DeviceMapping `json:"Devices,omitempty"`
	DeviceCgroupRules []string        `json:"DeviceCgroupRules,omitempty"`
	CapAdd            []string        `json:"CapAdd,omitempty"`
	Privileged        bool            `json:"Privileged,omitempty"`
	NanoCPUs          int64           `json:"NanoCpus,omitempty"`
	Memory            int64           `json:"Memory,omitempty"`
	RestartPolicy     RestartPolicy   `json:"RestartPolicy,omitempty"`
	NetworkMode       string          `json:"NetworkMode,omitempty"`
	SecurityOpt       []string        `json:"SecurityOpt,omitempty"`
}

// ContainerConfig is the body of POST /containers/create.
type ContainerConfig struct {
	Image       string            `json:"Image"`
	Hostname    string            `json:"Hostname,omitempty"`
	Env         []string          `json:"Env,omitempty"`
	Cmd         []string          `json:"Cmd,omitempty"`
	Labels      map[string]string `json:"Labels,omitempty"`
	StopTimeout *int              `json:"StopTimeout,omitempty"`
	HostConfig  HostConfig        `json:"HostConfig"`
}

// CreateContainer creates a container and returns its ID.
func (c *Client) CreateContainer(ctx context.Context, name string, cfg ContainerConfig) (string, error) {
	var out struct {
		ID string `json:"Id"`
	}
	err := c.doJSON(ctx, http.MethodPost, "/containers/create", url.Values{"name": {name}}, cfg, &out)
	if errors.Is(err, ErrNotFound) {
		return "", fmt.Errorf("docker: image %s not found: %w", cfg.Image, err)
	}
	return out.ID, err
}

// StartContainer starts a container (no-op if already running).
func (c *Client) StartContainer(ctx context.Context, id string) error {
	return c.doJSON(ctx, http.MethodPost, "/containers/"+url.PathEscape(id)+"/start", nil, nil, nil)
}

// StopContainer stops a container, waiting up to timeout before killing it.
func (c *Client) StopContainer(ctx context.Context, id string, timeout time.Duration) error {
	q := url.Values{"t": {fmt.Sprint(int(timeout.Seconds()))}}
	return c.doJSON(ctx, http.MethodPost, "/containers/"+url.PathEscape(id)+"/stop", q, nil, nil)
}

// RemoveContainer force-removes a container and its anonymous volumes.
// Returns nil if it doesn't exist.
func (c *Client) RemoveContainer(ctx context.Context, id string) error {
	q := url.Values{"force": {"true"}, "v": {"true"}}
	err := c.doJSON(ctx, http.MethodDelete, "/containers/"+url.PathEscape(id), q, nil, nil)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

// ContainerInfo is the subset of GET /containers/{id}/json we use.
type ContainerInfo struct {
	ID    string `json:"Id"`
	Name  string `json:"Name"`
	State struct {
		Status     string `json:"Status"` // created, running, paused, restarting, removing, exited, dead
		Running    bool   `json:"Running"`
		ExitCode   int    `json:"ExitCode"`
		Error      string `json:"Error"`
		StartedAt  string `json:"StartedAt"`
		FinishedAt string `json:"FinishedAt"`
	} `json:"State"`
	Config struct {
		Image  string            `json:"Image"`
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
	NetworkSettings struct {
		IPAddress string `json:"IPAddress"`
		Networks  map[string]struct {
			IPAddress string `json:"IPAddress"`
		} `json:"Networks"`
	} `json:"NetworkSettings"`
}

// IP returns the container's IPv4 address on the given network (or the first
// network with an address when network is empty).
func (ci *ContainerInfo) IP(network string) string {
	if network != "" {
		if n, ok := ci.NetworkSettings.Networks[network]; ok {
			return n.IPAddress
		}
	}
	if ci.NetworkSettings.IPAddress != "" {
		return ci.NetworkSettings.IPAddress
	}
	for _, n := range ci.NetworkSettings.Networks {
		if n.IPAddress != "" {
			return n.IPAddress
		}
	}
	return ""
}

// InspectContainer returns container details or ErrNotFound.
func (c *Client) InspectContainer(ctx context.Context, id string) (*ContainerInfo, error) {
	var out ContainerInfo
	if err := c.doJSON(ctx, http.MethodGet, "/containers/"+url.PathEscape(id)+"/json", nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ContainerSummary is an entry of GET /containers/json.
type ContainerSummary struct {
	ID     string            `json:"Id"`
	Names  []string          `json:"Names"`
	Image  string            `json:"Image"`
	State  string            `json:"State"`
	Labels map[string]string `json:"Labels"`
}

// ListContainers lists all containers (running or not) matching every
// "key=value" (or "key") label filter.
func (c *Client) ListContainers(ctx context.Context, labels ...string) ([]ContainerSummary, error) {
	f, _ := json.Marshal(map[string][]string{"label": labels})
	q := url.Values{"all": {"true"}, "filters": {string(f)}}
	var out []ContainerSummary
	err := c.doJSON(ctx, http.MethodGet, "/containers/json", q, nil, &out)
	return out, err
}

// ---------------------------------------------------------------- volumes

// CreateVolume creates a named local volume (idempotent).
func (c *Client) CreateVolume(ctx context.Context, name string, labels map[string]string) error {
	return c.doJSON(ctx, http.MethodPost, "/volumes/create", nil, map[string]any{"Name": name, "Labels": labels}, nil)
}

// RemoveVolume removes a volume; nil if it doesn't exist.
func (c *Client) RemoveVolume(ctx context.Context, name string) error {
	err := c.doJSON(ctx, http.MethodDelete, "/volumes/"+url.PathEscape(name), url.Values{"force": {"true"}}, nil, nil)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}
