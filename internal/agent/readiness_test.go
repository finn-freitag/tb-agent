package agent

import (
	"context"
	"io"
	"net"
	"testing"
)

func serve(t *testing.T, handle func(net.Conn)) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); handle(c) }()
		}
	}()
	return l.Addr().String()
}

func TestCheckReadyRDP(t *testing.T) {
	real := serve(t, func(c net.Conn) {
		buf := make([]byte, len(rdpConnectionRequest))
		if _, err := io.ReadFull(c, buf); err != nil {
			return
		}
		// TPKT + X.224 CC + RDP_NEG_RSP (PROTOCOL_HYBRID)
		c.Write([]byte{0x03, 0x00, 0x00, 0x13, 0x0e, 0xd0, 0x00, 0x00, 0x12, 0x34, 0x00, 0x02, 0x1f, 0x08, 0x00, 0x02, 0x00, 0x00, 0x00})
	})
	forwarder := serve(t, func(c net.Conn) {}) // accepts, then closes
	ctx := context.Background()
	if !checkReady(ctx, "rdp", real) {
		t.Error("real RDP server not detected as ready")
	}
	if checkReady(ctx, "rdp", forwarder) {
		t.Error("accept-and-close forwarder detected as ready")
	}
	if !checkReady(ctx, "adb", forwarder) {
		t.Error("adb should only need a TCP connect")
	}
}
