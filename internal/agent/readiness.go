package agent

import (
	"bufio"
	"context"
	"io"
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
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	switch kind {
	case "ssh":
		line, err := bufio.NewReader(c).ReadString('\n')
		return err == nil && strings.HasPrefix(line, "SSH-")
	case "rdp":
		return rdpAnswers(c)
	default:
		return true // adb, vnc, http(s): TCP connect is enough
	}
}

// rdpConnectionRequest is a TPKT + X.224 Connection Request carrying an RDP
// Negotiation Request (TLS | CredSSP), [MS-RDPBCGR] 2.2.1.1.
var rdpConnectionRequest = []byte{
	0x03, 0x00, 0x00, 0x13, // TPKT v3, length 19
	0x0e, 0xe0, 0x00, 0x00, 0x00, 0x00, 0x00, // X.224 CR TPDU
	0x01, 0x00, 0x08, 0x00, 0x03, 0x00, 0x00, 0x00, // RDP_NEG_REQ
}

// rdpAnswers reports whether a real RDP server answers on c. Port forwarders
// (e.g. QEMU user networking in docker-qemu) accept TCP connections even
// while Windows is still installing, so a plain connect is not enough.
func rdpAnswers(c net.Conn) bool {
	if _, err := c.Write(rdpConnectionRequest); err != nil {
		return false
	}
	var hdr [6]byte
	if _, err := io.ReadFull(c, hdr[:]); err != nil {
		return false
	}
	// TPKT version 3, X.224 length indicator, then Connection Confirm (0xd0).
	return hdr[0] == 0x03 && hdr[1] == 0x00 && hdr[5]&0xf0 == 0xd0
}
