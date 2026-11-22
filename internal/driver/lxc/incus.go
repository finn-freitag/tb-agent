package lxc

import (
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

// errNotFound is returned for 404 responses.
var errNotFound = errors.New("incus: not found")

// incusClient is a minimal Incus REST API client (unix socket, standard library only).
type incusClient struct {
	hc      *http.Client
	base    string
	project string
}

func newIncusClient(socket, project string) *incusClient {
	tr := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", socket)
	}}
	return &incusClient{hc: &http.Client{Transport: tr}, base: "http://incus", project: project}
}

// response is the standard Incus response envelope.
type response struct {
	Type       string          `json:"type"` // sync, async, error
	Status     string          `json:"status"`
	StatusCode int             `json:"status_code"`
	Operation  string          `json:"operation"`
	ErrorCode  int             `json:"error_code"`
	Error      string          `json:"error"`
	Metadata   json.RawMessage `json:"metadata"`
}

func (c *incusClient) do(ctx context.Context, method, path string, query url.Values, in any) (*response, error) {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(b)
	}
	if query == nil {
		query = url.Values{}
	}
	if c.project != "" && c.project != "default" {
		query.Set("project", c.project)
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
	defer resp.Body.Close()
	var r response
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return nil, fmt.Errorf("incus: %s %s: decode: %w (HTTP %d)", method, path, err, resp.StatusCode)
	}
	if r.Type == "error" || resp.StatusCode >= 400 {
		if r.ErrorCode == http.StatusNotFound || resp.StatusCode == http.StatusNotFound {
			return nil, errNotFound
		}
		return nil, fmt.Errorf("incus: %s %s: %s", method, path, r.Error)
	}
	return &r, nil
}

// wait blocks until an async operation finishes and returns its error.
func (c *incusClient) wait(ctx context.Context, r *response) error {
	if r.Type != "async" || r.Operation == "" {
		return nil
	}
	op := r.Operation
	if i := strings.Index(op, "?"); i >= 0 {
		op = op[:i]
	}
	for {
		wr, err := c.do(ctx, http.MethodGet, op+"/wait", url.Values{"timeout": {"30"}}, nil)
		if err != nil {
			return err
		}
		var m struct {
			Status     string `json:"status"`
			StatusCode int    `json:"status_code"`
			Err        string `json:"err"`
		}
		if err := json.Unmarshal(wr.Metadata, &m); err != nil {
			return err
		}
		switch {
		case m.StatusCode >= 400 || m.Status == "Failure" || m.Err != "":
			return fmt.Errorf("incus: operation failed: %s", m.Err)
		case m.Status == "Success" || m.StatusCode == 200:
			return nil
		case m.Status == "Cancelled":
			return errors.New("incus: operation cancelled")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func (c *incusClient) ping(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, err := c.do(ctx, http.MethodGet, "/1.0", nil, nil)
	return err
}

// instancesPost is the body of POST /1.0/instances.
type instancesPost struct {
	Name     string                       `json:"name"`
	Type     string                       `json:"type"`
	Source   instanceSource               `json:"source"`
	Profiles []string                     `json:"profiles,omitempty"`
	Config   map[string]string            `json:"config"`
	Devices  map[string]map[string]string `json:"devices,omitempty"`
	Start    bool                         `json:"start"`
}

type instanceSource struct {
	Type     string `json:"type"`
	Protocol string `json:"protocol,omitempty"`
	Server   string `json:"server,omitempty"`
	Alias    string `json:"alias,omitempty"`
	Mode     string `json:"mode,omitempty"`
}

type instance struct {
	Name   string            `json:"name"`
	Status string            `json:"status"`
	Type   string            `json:"type"`
	Config map[string]string `json:"config"`
}

type instanceState struct {
	Status  string `json:"status"`
	Network map[string]struct {
		Addresses []struct {
			Family  string `json:"family"`
			Address string `json:"address"`
			Scope   string `json:"scope"`
		} `json:"addresses"`
	} `json:"network"`
	Disk map[string]struct {
		Usage int64 `json:"usage"`
	} `json:"disk"`
	Memory struct {
		Usage int64 `json:"usage"`
	} `json:"memory"`
}

func (c *incusClient) create(ctx context.Context, body instancesPost) error {
	r, err := c.do(ctx, http.MethodPost, "/1.0/instances", nil, body)
	if err != nil {
		return err
	}
	return c.wait(ctx, r)
}

func (c *incusClient) get(ctx context.Context, name string) (*instance, error) {
	r, err := c.do(ctx, http.MethodGet, "/1.0/instances/"+url.PathEscape(name), nil, nil)
	if err != nil {
		return nil, err
	}
	var in instance
	return &in, json.Unmarshal(r.Metadata, &in)
}

func (c *incusClient) list(ctx context.Context) ([]instance, error) {
	r, err := c.do(ctx, http.MethodGet, "/1.0/instances", url.Values{"recursion": {"1"}}, nil)
	if err != nil {
		return nil, err
	}
	var out []instance
	return out, json.Unmarshal(r.Metadata, &out)
}

func (c *incusClient) state(ctx context.Context, name string) (*instanceState, error) {
	r, err := c.do(ctx, http.MethodGet, "/1.0/instances/"+url.PathEscape(name)+"/state", nil, nil)
	if err != nil {
		return nil, err
	}
	var st instanceState
	return &st, json.Unmarshal(r.Metadata, &st)
}

func (c *incusClient) setState(ctx context.Context, name, action string, timeout int, force bool) error {
	body := map[string]any{"action": action, "timeout": timeout, "force": force}
	r, err := c.do(ctx, http.MethodPut, "/1.0/instances/"+url.PathEscape(name)+"/state", nil, body)
	if err != nil {
		return err
	}
	return c.wait(ctx, r)
}

func (c *incusClient) delete(ctx context.Context, name string) error {
	r, err := c.do(ctx, http.MethodDelete, "/1.0/instances/"+url.PathEscape(name), nil, nil)
	if err != nil {
		return err
	}
	return c.wait(ctx, r)
}
