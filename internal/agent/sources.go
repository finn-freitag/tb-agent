package agent

import (
	"context"
	"log/slog"
	"net"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// SourceFilter decides which peers may connect to forwarded device ports.
//
// Device ports are meant to be reached through the dashboard gateway, which
// enforces per-device allowlists and rate limits. Without a filter anyone who
// can reach the node could connect to the devices directly, bypassing both.
//
// Entries: IPs/CIDRs, "dashboard" (the addresses dashboard.endpoint resolves
// to, refreshed periodically), "private" (RFC 1918, IPv6 ULA and the
// 100.64.0.0/10 range Tailscale uses; for exposure "none", where users reach
// the node directly) and "any". Loopback and this host's own addresses are
// always allowed.
type SourceFilter struct {
	any      bool
	private  bool
	static   []netip.Prefix
	dashHost string // "" = keyword not used
	log      *slog.Logger

	dynamic atomic.Pointer[[]netip.Prefix] // resolved dashboard + local addresses

	mu     sync.Mutex
	warned map[netip.Addr]time.Time
}

// cgnat is the shared address space Tailscale assigns node addresses from.
var cgnat = netip.MustParsePrefix("100.64.0.0/10")

// NewSourceFilter builds a filter from the config entries (already validated).
func NewSourceFilter(entries []string, dashboardEndpoint string, log *slog.Logger) *SourceFilter {
	f := &SourceFilter{log: log, warned: map[netip.Addr]time.Time{}}
	for _, e := range entries {
		switch strings.ToLower(strings.TrimSpace(e)) {
		case "any":
			f.any = true
		case "private":
			f.private = true
		case "dashboard":
			host, _, err := net.SplitHostPort(dashboardEndpoint)
			if err != nil {
				host = dashboardEndpoint
			}
			f.dashHost = host
		default:
			if p, err := ParseSource(e); err == nil {
				f.static = append(f.static, p)
			}
		}
	}
	empty := []netip.Prefix{}
	f.dynamic.Store(&empty)
	f.refresh(context.Background())
	return f
}

// ParseSource parses an IP or CIDR entry.
func ParseSource(e string) (netip.Prefix, error) {
	e = strings.TrimSpace(e)
	if p, err := netip.ParsePrefix(e); err == nil {
		return p.Masked(), nil
	}
	a, err := netip.ParseAddr(e)
	if err != nil {
		return netip.Prefix{}, err
	}
	a = a.Unmap()
	return netip.PrefixFrom(a, a.BitLen()), nil
}

// Run refreshes the dynamic addresses until ctx ends.
func (f *SourceFilter) Run(ctx context.Context) {
	if f.any {
		return
	}
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			f.refresh(ctx)
		}
	}
}

func (f *SourceFilter) refresh(ctx context.Context) {
	var out []netip.Prefix
	add := func(a netip.Addr) {
		a = a.Unmap()
		out = append(out, netip.PrefixFrom(a, a.BitLen()))
	}
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, ia := range addrs {
			if n, ok := ia.(*net.IPNet); ok {
				if a, ok := netip.AddrFromSlice(n.IP); ok {
					add(a)
				}
			}
		}
	}
	if f.dashHost != "" {
		if a, err := netip.ParseAddr(f.dashHost); err == nil {
			add(a)
		} else {
			rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			ips, err := net.DefaultResolver.LookupNetIP(rctx, "ip", f.dashHost)
			cancel()
			if err != nil {
				// Keep the previous dashboard addresses on lookup failures.
				f.log.Warn("cannot resolve dashboard host for allowed_sources", "host", f.dashHost, "err", err)
				return
			}
			for _, a := range ips {
				add(a)
			}
		}
	}
	f.dynamic.Store(&out)
}

// Allowed reports whether a connection from addr may be forwarded.
func (f *SourceFilter) Allowed(addr net.Addr) bool {
	if f == nil || f.any {
		return true
	}
	ta, ok := addr.(*net.TCPAddr)
	if !ok {
		return false
	}
	ip, ok := netip.AddrFromSlice(ta.IP)
	if !ok {
		return false
	}
	ip = ip.Unmap()
	if ip.IsLoopback() || (f.private && (ip.IsPrivate() || cgnat.Contains(ip))) {
		return true
	}
	for _, p := range f.static {
		if p.Contains(ip) {
			return true
		}
	}
	for _, p := range *f.dynamic.Load() {
		if p.Contains(ip) {
			return true
		}
	}
	f.warnOnce(ip)
	return false
}

// warnOnce logs rejected sources at most once per hour per address.
func (f *SourceFilter) warnOnce(ip netip.Addr) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if t, ok := f.warned[ip]; ok && time.Since(t) < time.Hour {
		return
	}
	if len(f.warned) > 1000 {
		clear(f.warned)
	}
	f.warned[ip] = time.Now()
	f.log.Warn("rejected connection to a device port from a source not in allowed_sources", "source", ip.String())
}
