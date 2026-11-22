package agent

import (
	"bufio"
	"context"
	"net"
	"strings"
	"time"
)

// defaultPorts are the internal service ports (tb-templates/docs/FORMAT.md).
var defaultPorts = map[string]int32{"ssh": 22, "rdp": 3389, "vnc": 5900, "adb": 5555, "http": 80, "https": 443}

// checkReady runs the template's readiness check against endpoint (host:port).
func checkReady(ctx context.Context, kind, endpoint string) bool {
	if kind == "" || kind == "none" {
		return true
	}
	if endpoint == "" {
		return false
	}
	d := net.Dialer{Timeout: 3 * time.Second}
	c, err := d.DialContext(ctx, "tcp", endpoint)
	if err != nil {
		return false
	}
	defer c.Close()
	if kind != "ssh" {
		return true // rdp, adb: TCP connect is enough
	}
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	line, err := bufio.NewReader(c).ReadString('\n')
	return err == nil && strings.HasPrefix(line, "SSH-")
}
