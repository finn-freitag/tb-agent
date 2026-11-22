package agent

import (
	"io"
	"log/slog"
	"net"
	"testing"
)

func tcp(ip string) net.Addr { return &net.TCPAddr{IP: net.ParseIP(ip), Port: 1234} }

func TestSourceFilter(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	f := NewSourceFilter([]string{"dashboard", "10.1.0.0/16", "192.0.2.7"}, "100.103.236.99:7443", log)
	for ip, want := range map[string]bool{
		"127.0.0.1":      true, // loopback
		"::1":            true,
		"100.103.236.99": true, // dashboard
		"10.1.2.3":       true, // CIDR
		"192.0.2.7":      true, // single IP
		"192.0.2.8":      false,
		"203.0.113.9":    false,
	} {
		if got := f.Allowed(tcp(ip)); got != want {
			t.Errorf("%s: got %v, want %v", ip, got, want)
		}
	}
	p := NewSourceFilter([]string{"private"}, "dash.example.com:7443", log)
	for ip, want := range map[string]bool{"192.168.2.5": true, "100.105.1.2": true, "fd00::1": true, "8.8.8.8": false} {
		if got := p.Allowed(tcp(ip)); got != want {
			t.Errorf("private %s: got %v, want %v", ip, got, want)
		}
	}
	if !NewSourceFilter([]string{"any"}, "x:1", log).Allowed(tcp("203.0.113.9")) {
		t.Error("any must allow everything")
	}
	var nilFilter *SourceFilter
	if !nilFilter.Allowed(tcp("203.0.113.9")) {
		t.Error("nil filter must allow everything")
	}
}
