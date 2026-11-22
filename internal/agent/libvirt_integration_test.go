//go:build integration

// Real end-to-end test of the reconciler + libvirt driver + port forwarding.
//
//	TB_IT_DATA_DIR=/some/dir go test -tags integration -run TestLibvirt -v -timeout 30m ./internal/agent
//
// Creates a Debian 13 VM (768 MB RAM) via qemu:///system, logs in over SSH
// through the agent's port forward, checks VNC, stops/starts and destroys it.
package agent

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	golibvirt "github.com/digitalocean/go-libvirt"
	"github.com/digitalocean/go-libvirt/socket/dialers"
	agentv1 "github.com/finn-freitag/tb-proto/gen/agent/v1"
	"golang.org/x/crypto/ssh"
	"google.golang.org/protobuf/proto"

	"github.com/finn-freitag/tb-agent/internal/config"
	"github.com/finn-freitag/tb-agent/internal/driver"
	_ "github.com/finn-freitag/tb-agent/internal/driver/libvirt"
	"github.com/finn-freitag/tb-agent/internal/images"
	"github.com/finn-freitag/tb-agent/internal/licenses"
)

func TestLibvirtDebian(t *testing.T) {
	dataDir := os.Getenv("TB_IT_DATA_DIR")
	if dataDir == "" {
		t.Skip("TB_IT_DATA_DIR not set")
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	img, err := images.New(dataDir+"/images", log)
	if err != nil {
		t.Fatal(err)
	}
	lic, _ := licenses.New(map[string]config.LicensePool{}, dataDir)
	drv, err := driver.New("libvirt", driver.Env{NodeName: "it", DataDir: dataDir, Images: img, Licenses: lic, Log: log}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if c := drv.Capabilities(context.Background()); !c.Healthy {
		t.Fatalf("driver unhealthy: %s", c.Message)
	}
	fwd := NewForwarder("127.0.0.1", log)
	m := NewManager(log, map[string]driver.Driver{"libvirt": drv}, lic, newStore(t.TempDir()), fwd)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}

	b := make([]byte, 6)
	rand.Read(b)
	id := "it-" + hex.EncodeToString(b)
	pwb := make([]byte, 12)
	rand.Read(pwb)
	password := hex.EncodeToString(pwb)
	sshPort, vncPort := freePort(t), freePort(t)
	spec := &agentv1.DeviceSpec{
		DeviceId: id, Hostname: "tb-it-debian", Driver: "libvirt", Revision: 1, Power: agentv1.PowerState_POWER_STATE_RUNNING,
		Template: &agentv1.TemplateSpec{Id: "debian-13", Family: "linux", GuestProfile: "cloud-init", Readiness: "ssh", DefaultUser: "tossbox",
			Image: &agentv1.ImageSpec{Url: "https://cloud.debian.org/images/cloud/trixie/latest/debian-13-genericcloud-amd64.qcow2", Format: "qcow2"}},
		Resources: &agentv1.Resources{Vcpus: 1, CpuCapPercent: 25, RamMb: 768, StorageGb: 8},
		Services: []*agentv1.ServiceSpec{
			{Name: "ssh", InternalPort: 22, PublicPort: sshPort, Username: "tossbox", Password: password},
			{Name: "vnc", InternalPort: 5900, PublicPort: vncPort, Password: password},
		},
	}
	t.Cleanup(func() {
		// Safety net: make sure nothing is left behind even if the test fails.
		_ = drv.Destroy(context.Background(), id)
	})

	start := time.Now()
	m.Apply(spec)
	waitState(t, m, id, 15*time.Minute, func(s *agentv1.DeviceStatus) bool {
		return s.State == agentv1.DeviceState_DEVICE_STATE_RUNNING && s.Ready
	})
	t.Logf("device ready after %s", time.Since(start).Round(time.Second))

	// SSH through the forward with the generated password. cloud-init may
	// still be applying the password right after sshd comes up: retry.
	var out string
	deadline := time.Now().Add(5 * time.Minute)
	for {
		out, err = sshRun(fmt.Sprintf("127.0.0.1:%d", sshPort), "tossbox", password, "hostname && sudo -n true && echo sudo-ok")
		if err == nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Second)
	}
	if err != nil {
		t.Fatalf("ssh: %v", err)
	}
	if !strings.Contains(out, "tb-it-debian") || !strings.Contains(out, "sudo-ok") {
		t.Fatalf("unexpected ssh output %q", out)
	}
	t.Logf("ssh via forward ok: %q", strings.TrimSpace(out))

	// VNC console (served by QEMU on the host) through the forward.
	c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", vncPort), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	banner, _ := bufio.NewReader(c).ReadString('\n')
	c.Close()
	if !strings.HasPrefix(banner, "RFB ") {
		t.Fatalf("vnc banner %q", banner)
	}
	t.Logf("vnc via forward ok: %q", strings.TrimSpace(banner))

	// Stop.
	stopped := proto.Clone(spec).(*agentv1.DeviceSpec)
	stopped.Power, stopped.Revision = agentv1.PowerState_POWER_STATE_STOPPED, 2
	m.Apply(stopped)
	waitState(t, m, id, 3*time.Minute, func(s *agentv1.DeviceStatus) bool { return s.State == agentv1.DeviceState_DEVICE_STATE_STOPPED })
	if p := fwd.Ports(id); len(p) != 0 {
		t.Fatalf("forwards still open while stopped: %v", p)
	}
	t.Log("stopped ok")

	// Start again.
	started := proto.Clone(spec).(*agentv1.DeviceSpec)
	started.Revision = 3
	m.Apply(started)
	waitState(t, m, id, 5*time.Minute, func(s *agentv1.DeviceStatus) bool {
		return s.State == agentv1.DeviceState_DEVICE_STATE_RUNNING && s.Ready
	})
	if out, err := sshRun(fmt.Sprintf("127.0.0.1:%d", sshPort), "tossbox", password, "uptime"); err != nil {
		t.Fatalf("ssh after restart: %v", err)
	} else {
		t.Logf("restart ok: %s", strings.TrimSpace(out))
	}

	// Destroy.
	m.Delete(id)
	waitGone(t, m, id, 3*time.Minute)
	assertNothingLeft(t, id)
	t.Log("destroyed, nothing left behind")
}

func waitState(t *testing.T, m *Manager, id string, timeout time.Duration, ok func(*agentv1.DeviceStatus) bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	last := ""
	for time.Now().Before(deadline) {
		for _, s := range m.Statuses() {
			if s.DeviceId != id {
				continue
			}
			if cur := fmt.Sprintf("%s ready=%v ip=%s %q", s.State, s.Ready, s.InternalIp, s.Message); cur != last {
				t.Logf("status: %s", cur)
				last = cur
			}
			if ok(s) {
				return
			}
			if s.State == agentv1.DeviceState_DEVICE_STATE_FAILED {
				t.Fatalf("device failed: %s", s.Message)
			}
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("timeout; last status %s", last)
}

func waitGone(t *testing.T, m *Manager, id string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		found := false
		for _, s := range m.Statuses() {
			if s.DeviceId == id {
				found = true
			}
		}
		if !found {
			return
		}
		time.Sleep(time.Second)
	}
	t.Fatal("device not destroyed in time")
}

func assertNothingLeft(t *testing.T, id string) {
	lv := golibvirt.NewWithDialer(dialers.NewLocal())
	if err := lv.ConnectToURI(golibvirt.QEMUSystem); err != nil {
		t.Fatal(err)
	}
	defer lv.Disconnect()
	if _, err := lv.DomainLookupByName(driver.DomainName(id)); err == nil {
		t.Fatal("domain still exists")
	}
	pools, _, _ := lv.ConnectListAllStoragePools(1, 0)
	for _, p := range pools {
		vols, _, _ := lv.StoragePoolListAllVolumes(p, 1, 0)
		for _, v := range vols {
			if strings.HasPrefix(v.Name, driver.DomainName(id)) {
				t.Fatalf("volume left behind: %s/%s", p.Name, v.Name)
			}
		}
	}
}

func sshRun(addr, user, password, cmd string) (string, error) {
	cl, err := ssh.Dial("tcp", addr, &ssh.ClientConfig{
		User: user, Auth: []ssh.AuthMethod{ssh.Password(password)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: 10 * time.Second,
	})
	if err != nil {
		return "", err
	}
	defer cl.Close()
	s, err := cl.NewSession()
	if err != nil {
		return "", err
	}
	defer s.Close()
	b, err := s.CombinedOutput(cmd)
	return string(b), err
}
