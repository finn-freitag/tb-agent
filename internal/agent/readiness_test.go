package agent

import (
	"context"
	"io"
	"net"
	"os"
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
}

// fakeAdbd answers CNXN and a getprop shell with value.
func fakeAdbd(value string) func(net.Conn) {
	return func(c net.Conn) {
		for {
			m, err := adbRead(c)
			if err != nil {
				return
			}
			switch m.cmd {
			case adbCNXN:
				adbWrite(c, adbCNXN, 0x01000001, 256*1024, []byte("device::ro.product.name=redroid;\x00"))
			case adbOPEN:
				adbWrite(c, adbOKAY, 7, m.arg0, nil)
				adbWrite(c, adbWRTE, 7, m.arg0, []byte(value))
				if r, err := adbRead(c); err != nil || r.cmd != adbOKAY {
					return
				}
				adbWrite(c, adbCLSE, 7, m.arg0, nil)
			}
		}
	}
}

func TestCheckReadyADB(t *testing.T) {
	ctx := context.Background()
	if !checkReady(ctx, "adb", serve(t, fakeAdbd("1\n"))) {
		t.Error("booted device not ready")
	}
	if checkReady(ctx, "adb", serve(t, fakeAdbd("\n"))) {
		t.Error("device still booting counted as ready")
	}
	if checkReady(ctx, "adb", serve(t, func(c net.Conn) {})) {
		t.Error("accept-and-close counted as ready")
	}
	auth := serve(t, func(c net.Conn) {
		if _, err := adbRead(c); err == nil {
			adbWrite(c, adbAUTH, 1, 0, make([]byte, 20))
		}
	})
	if !checkReady(ctx, "adb", auth) {
		t.Error("adbd with auth should fall back to ready")
	}
}

// TB_ADB_ADDR=host:port go test -run TestRealADB ./internal/agent/ checks a
// real adbd; TB_ADB_BOOTED is the expected result ("1" or "0").
func TestRealADB(t *testing.T) {
	addr := os.Getenv("TB_ADB_ADDR")
	if addr == "" {
		t.Skip("TB_ADB_ADDR not set")
	}
	want := os.Getenv("TB_ADB_BOOTED") == "1"
	if got := checkReady(context.Background(), "adb", addr); got != want {
		t.Fatalf("checkReady(adb, %s) = %v, want %v", addr, got, want)
	}
}
