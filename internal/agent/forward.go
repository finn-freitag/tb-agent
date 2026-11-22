package agent

import (
	"errors"
	"io"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"time"
)

// Rule forwards a public port to a target ("host:port"; "" = not reachable yet).
type Rule struct {
	PublicPort int32
	Target     string
	Service    string
}

type listener struct {
	ln       net.Listener
	deviceID string
	service  string
	mu       sync.Mutex
	target   string
}

func (l *listener) getTarget() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.target
}

// Forwarder is a userspace TCP port forwarder (public_port -> device endpoint).
type Forwarder struct {
	addr    string
	log     *slog.Logger
	sources *SourceFilter // nil = allow all

	mu sync.Mutex
	ls map[int32]*listener
}

// NewForwarder creates a forwarder binding on addr (e.g. "0.0.0.0").
// sources restricts who may connect (nil = everyone).
func NewForwarder(addr string, sources *SourceFilter, log *slog.Logger) *Forwarder {
	return &Forwarder{addr: addr, sources: sources, log: log, ls: map[int32]*listener{}}
}

// Sync makes the device's listeners match rules exactly.
func (f *Forwarder) Sync(deviceID string, rules []Rule) {
	f.mu.Lock()
	defer f.mu.Unlock()
	want := map[int32]Rule{}
	for _, r := range rules {
		if r.PublicPort > 0 {
			want[r.PublicPort] = r
		}
	}
	for port, l := range f.ls {
		if l.deviceID != deviceID {
			continue
		}
		if _, ok := want[port]; !ok {
			l.ln.Close()
			delete(f.ls, port)
		}
	}
	for port, r := range want {
		if l, ok := f.ls[port]; ok {
			if l.deviceID != deviceID {
				f.log.Error("port already forwarded for another device", "port", port, "device", deviceID, "other", l.deviceID)
				continue
			}
			l.mu.Lock()
			l.target = r.Target
			l.mu.Unlock()
			continue
		}
		ln, err := net.Listen("tcp", net.JoinHostPort(f.addr, strconv.Itoa(int(port))))
		if err != nil {
			f.log.Error("cannot listen on forwarded port", "port", port, "device", deviceID, "err", err)
			continue
		}
		l := &listener{ln: ln, deviceID: deviceID, service: r.Service, target: r.Target}
		f.ls[port] = l
		go f.serve(l)
	}
}

// Remove closes all listeners of a device.
func (f *Forwarder) Remove(deviceID string) { f.Sync(deviceID, nil) }

// Ports lists active ports per device (for tests / diagnostics).
func (f *Forwarder) Ports(deviceID string) []int32 {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []int32
	for p, l := range f.ls {
		if l.deviceID == deviceID {
			out = append(out, p)
		}
	}
	return out
}

// Close stops everything.
func (f *Forwarder) Close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for p, l := range f.ls {
		l.ln.Close()
		delete(f.ls, p)
	}
}

func (f *Forwarder) serve(l *listener) {
	for {
		c, err := l.ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			time.Sleep(100 * time.Millisecond)
			continue
		}
		go f.handle(l, c)
	}
}

func (f *Forwarder) handle(l *listener, c net.Conn) {
	defer c.Close()
	if !f.sources.Allowed(c.RemoteAddr()) {
		return
	}
	target := l.getTarget()
	if target == "" {
		return
	}
	u, err := net.DialTimeout("tcp", target, 10*time.Second)
	if err != nil {
		f.log.Debug("forward dial failed", "device", l.deviceID, "service", l.service, "target", target, "err", err)
		return
	}
	defer u.Close()
	pipe(c, u)
}

// pipe copies both directions with half-close support.
func pipe(a, b net.Conn) {
	var wg sync.WaitGroup
	cp := func(dst, src net.Conn) {
		defer wg.Done()
		_, _ = io.Copy(dst, src)
		if tc, ok := dst.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		} else {
			_ = dst.Close()
		}
	}
	wg.Add(2)
	go cp(a, b)
	go cp(b, a)
	wg.Wait()
}
