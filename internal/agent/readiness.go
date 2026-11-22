package agent

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
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
	case "adb":
		return adbBooted(c)
	default:
		return true // vnc, http(s): TCP connect is enough
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

// adb wire protocol (system/core/adb/protocol.txt).
const (
	adbCNXN = 0x4e584e43
	adbAUTH = 0x48545541
	adbSTLS = 0x534c5453
	adbOPEN = 0x4e45504f
	adbOKAY = 0x59414b4f
	adbWRTE = 0x45545257
	adbCLSE = 0x45534c43
)

type adbMsg struct {
	cmd, arg0, arg1 uint32
	data            []byte
}

func adbWrite(w io.Writer, cmd, arg0, arg1 uint32, data []byte) error {
	var sum uint32
	for _, b := range data {
		sum += uint32(b)
	}
	h := make([]byte, 24, 24+len(data))
	for i, v := range []uint32{cmd, arg0, arg1, uint32(len(data)), sum, cmd ^ 0xffffffff} {
		binary.LittleEndian.PutUint32(h[i*4:], v)
	}
	_, err := w.Write(append(h, data...))
	return err
}

func adbRead(r io.Reader) (adbMsg, error) {
	var h [24]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return adbMsg{}, err
	}
	u := func(i int) uint32 { return binary.LittleEndian.Uint32(h[i*4:]) }
	m := adbMsg{cmd: u(0), arg0: u(1), arg1: u(2)}
	if u(5) != m.cmd^0xffffffff || u(3) > 1<<20 {
		return adbMsg{}, errors.New("adb: bad header")
	}
	m.data = make([]byte, u(3))
	_, err := io.ReadFull(r, m.data)
	return m, err
}

// adbBooted reports whether Android behind an adb endpoint finished booting
// (sys.boot_completed=1). adbd starts early in boot and answers even when
// the framework crash-loops, so a TCP connect is not enough. If adbd
// requires authentication (or TLS) the boot state can't be queried and the
// endpoint counts as ready.
func adbBooted(c net.Conn) bool {
	if adbWrite(c, adbCNXN, 0x01000001, 256*1024, []byte("host::\x00")) != nil {
		return false
	}
	for {
		m, err := adbRead(c)
		if err != nil {
			return false
		}
		if m.cmd == adbAUTH || m.cmd == adbSTLS {
			return true
		}
		if m.cmd == adbCNXN {
			break
		}
	}
	const local = 1
	if adbWrite(c, adbOPEN, local, 0, []byte("shell:getprop sys.boot_completed\x00")) != nil {
		return false
	}
	var out []byte
	for {
		m, err := adbRead(c)
		if err != nil {
			return strings.TrimSpace(string(out)) == "1"
		}
		switch m.cmd {
		case adbWRTE:
			out = append(out, m.data...)
			if adbWrite(c, adbOKAY, local, m.arg0, nil) != nil {
				return false
			}
		case adbCLSE:
			return strings.TrimSpace(string(out)) == "1"
		}
	}
}
