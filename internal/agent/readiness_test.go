package agent

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/finn-freitag/tb-agent/internal/adbkey"
	agentv1 "github.com/finn-freitag/tb-proto/gen/agent/v1"
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

// With a key, the probe answers the AUTH challenge and then really checks
// the boot state.
func TestCheckReadyADBAuth(t *testing.T) {
	key, err := adbkey.LoadOrCreate(filepath.Join(t.TempDir(), "adbkey"))
	if err != nil {
		t.Fatal(err)
	}
	pub := key.Public()
	authd := func(value string) func(net.Conn) {
		inner := fakeAdbd(value)
		return func(c net.Conn) {
			if m, err := adbRead(c); err != nil || m.cmd != adbCNXN {
				return
			}
			token := make([]byte, 20)
			rand.Read(token)
			adbWrite(c, adbAUTH, adbAuthToken, 0, token)
			m, err := adbRead(c)
			if err != nil || m.cmd != adbAUTH || m.arg0 != adbAuthSignature ||
				rsa.VerifyPKCS1v15(pub, crypto.SHA1, token, m.data) != nil {
				adbWrite(c, adbAUTH, adbAuthToken, 0, token) // rejected: new challenge
				return
			}
			inner(&replayConn{Conn: c, first: true})
		}
	}
	ctx := context.Background()
	if checkReadyWith(ctx, "adb", serve(t, authd("0\n")), key) {
		t.Error("booting device counted as ready after auth")
	}
	if !checkReadyWith(ctx, "adb", serve(t, authd("1\n")), key) {
		t.Error("booted device not ready after auth")
	}
}

// replayConn feeds fakeAdbd a synthetic CNXN first (the real one was
// consumed by the auth exchange).
type replayConn struct {
	net.Conn
	first bool
	buf   []byte
}

func (r *replayConn) Read(p []byte) (int, error) {
	if r.first {
		r.first = false
		var b bytes.Buffer
		adbWrite(&b, adbCNXN, 0x01000001, 256*1024, []byte("host::\x00"))
		r.buf = b.Bytes()
	}
	if len(r.buf) > 0 {
		n := copy(p, r.buf)
		r.buf = r.buf[n:]
		return n, nil
	}
	return r.Conn.Read(p)
}

// TB_ADB_ADDR=host:port go test -run TestRealADB ./internal/agent/ checks a
// real adbd; TB_ADB_BOOTED is the expected result ("1" or "0").
func TestRealADB(t *testing.T) {
	addr := os.Getenv("TB_ADB_ADDR")
	if addr == "" {
		t.Skip("TB_ADB_ADDR not set")
	}
	want := os.Getenv("TB_ADB_BOOTED") == "1"
	var key *adbkey.Key
	if p := os.Getenv("TB_ADB_KEY"); p != "" {
		var err error
		if key, err = adbkey.LoadOrCreate(p); err != nil {
			t.Fatal(err)
		}
	}
	if got := checkReadyWith(context.Background(), "adb", addr, key); got != want {
		t.Fatalf("checkReady(adb, %s) = %v, want %v", addr, got, want)
	}
}

// TB_ADB_ADDR=host:port TB_ADB_KEY=<key> TB_ADB_EXPECT=accepted|rejected|open
// go test -run TestRealADBAuth ./internal/agent/ reports how a real adbd
// treats the key: open (no auth required), accepted or rejected.
func TestRealADBAuth(t *testing.T) {
	addr, keyPath := os.Getenv("TB_ADB_ADDR"), os.Getenv("TB_ADB_KEY")
	if addr == "" || keyPath == "" {
		t.Skip("TB_ADB_ADDR / TB_ADB_KEY not set")
	}
	key, err := adbkey.LoadOrCreate(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	adbWrite(c, adbCNXN, 0x01000001, 256*1024, []byte("host::\x00"))
	result := ""
	signed := false
	for result == "" {
		m, err := adbRead(c)
		if err != nil {
			t.Fatal(err)
		}
		switch {
		case m.cmd == adbCNXN && !signed:
			result = "open"
		case m.cmd == adbCNXN:
			result = "accepted"
		case m.cmd == adbAUTH && signed:
			result = "rejected"
		case m.cmd == adbAUTH:
			sig, _ := key.Sign(m.data)
			adbWrite(c, adbAUTH, adbAuthSignature, 0, sig)
			signed = true
		}
	}
	t.Logf("adbd at %s: %s", addr, result)
	if want := os.Getenv("TB_ADB_EXPECT"); want != "" && want != result {
		t.Fatalf("got %s, want %s", result, want)
	}
}

func TestUsageChanged(t *testing.T) {
	base := &agentv1.DeviceStatus{MemoryUsedMb: 1000, CpuUsedPercent: 10, StorageUsedMb: 500}
	for _, c := range []struct {
		st   *agentv1.DeviceStatus
		want bool
	}{
		{&agentv1.DeviceStatus{MemoryUsedMb: 1008, CpuUsedPercent: 10.5, StorageUsedMb: 500}, false},
		{&agentv1.DeviceStatus{MemoryUsedMb: 1020, CpuUsedPercent: 10, StorageUsedMb: 500}, true},
		{&agentv1.DeviceStatus{MemoryUsedMb: 1000, CpuUsedPercent: 12, StorageUsedMb: 500}, true},
		{&agentv1.DeviceStatus{MemoryUsedMb: 1000, CpuUsedPercent: 10, StorageUsedMb: 501}, true},
	} {
		if got := usageChanged(base, c.st); got != c.want {
			t.Errorf("%+v: got %v", c.st, got)
		}
	}
}
