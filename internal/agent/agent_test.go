package agent

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/finn-freitag/tb-proto/enroll"
	agentv1 "github.com/finn-freitag/tb-proto/gen/agent/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"gopkg.in/yaml.v3"

	"github.com/finn-freitag/tb-agent/internal/config"
	"github.com/finn-freitag/tb-agent/internal/driver"
)

// ---- fake driver ----

type fakeDriver struct {
	mu      sync.Mutex
	devs    map[string]agentv1.DeviceState
	created int
}

func (f *fakeDriver) Name() string { return "fake" }
func (f *fakeDriver) Capabilities(context.Context) *agentv1.DriverCapabilities {
	return &agentv1.DriverCapabilities{Name: "fake", Families: []string{"linux"}, Services: []string{"ssh"}, Healthy: true}
}
func (f *fakeDriver) Ensure(_ context.Context, s *agentv1.DeviceSpec, r driver.Reporter) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.devs[s.DeviceId]; !ok {
		r(agentv1.DeviceState_DEVICE_STATE_PROVISIONING, "Creating")
		f.created++
	}
	if s.Power == agentv1.PowerState_POWER_STATE_STOPPED {
		f.devs[s.DeviceId] = agentv1.DeviceState_DEVICE_STATE_STOPPED
	} else {
		f.devs[s.DeviceId] = agentv1.DeviceState_DEVICE_STATE_RUNNING
	}
	return nil
}
func (f *fakeDriver) Destroy(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.devs, id)
	return nil
}
func (f *fakeDriver) List(context.Context) ([]*agentv1.DeviceStatus, error) { return nil, nil }
func (f *fakeDriver) Status(_ context.Context, id string) (*agentv1.DeviceStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	st, ok := f.devs[id]
	if !ok {
		return &agentv1.DeviceStatus{DeviceId: id, State: agentv1.DeviceState_DEVICE_STATE_DESTROYED}, nil
	}
	s := &agentv1.DeviceStatus{DeviceId: id, State: st}
	if st == agentv1.DeviceState_DEVICE_STATE_RUNNING {
		s.InternalIp = "127.0.0.1"
	}
	return s, nil
}

var theFake = &fakeDriver{devs: map[string]agentv1.DeviceState{}}

func init() {
	driver.Register("fake", func(driver.Env, *yaml.Node) (driver.Driver, error) { return theFake, nil })
}

// ---- fake dashboard ----

type fakeDash struct {
	agentv1.UnimplementedEnrollmentServer
	agentv1.UnimplementedNodeControlServer
	ca       *x509.Certificate
	caKey    *ecdsa.PrivateKey
	secret   string
	welcome  []*agentv1.DeviceSpec
	statuses chan *agentv1.DeviceStatus
	hello    chan *agentv1.Hello
	toAgent  chan *agentv1.DashboardMessage
}

func (d *fakeDash) Enroll(_ context.Context, r *agentv1.EnrollRequest) (*agentv1.EnrollResponse, error) {
	if r.Token != d.secret {
		return nil, status.Error(codes.PermissionDenied, "bad token")
	}
	csr, err := x509.ParseCertificateRequest(r.CsrDer)
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(42), Subject: pkix.Name{CommonName: "node-1"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, KeyUsage: x509.KeyUsageDigitalSignature}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, d.ca, csr.PublicKey, d.caKey)
	if err != nil {
		return nil, err
	}
	return &agentv1.EnrollResponse{NodeId: "node-1", NodeName: "test", ClientCertDer: der, CaCertDer: d.ca.Raw}, nil
}

func (d *fakeDash) Connect(s grpc.BidiStreamingServer[agentv1.AgentMessage, agentv1.DashboardMessage]) error {
	p, _ := peer.FromContext(s.Context())
	ti, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok || len(ti.State.VerifiedChains) == 0 || ti.State.VerifiedChains[0][0].Subject.CommonName != "node-1" {
		return status.Error(codes.Unauthenticated, "client certificate required")
	}
	first, err := s.Recv()
	if err != nil {
		return err
	}
	d.hello <- first.GetHello()
	if err := s.Send(&agentv1.DashboardMessage{Msg: &agentv1.DashboardMessage_Welcome{Welcome: &agentv1.Welcome{NodeId: "node-1", NodeName: "test", Devices: d.welcome}}}); err != nil {
		return err
	}
	errc := make(chan error, 1)
	go func() {
		for {
			m, err := s.Recv()
			if err != nil {
				errc <- err
				return
			}
			if st := m.GetDeviceStatus(); st != nil {
				d.statuses <- st
			}
		}
	}()
	for {
		select {
		case err := <-errc:
			return err
		case m := <-d.toAgent:
			if err := s.Send(m); err != nil {
				return err
			}
		}
	}
}

func newCert(t *testing.T, tmpl *x509.Certificate, parent *x509.Certificate, parentKey *ecdsa.PrivateKey) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if parent == nil {
		parent, parentKey = tmpl, key
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, &key.PublicKey, parentKey)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := x509.ParseCertificate(der)
	return c, key
}

// sshBanner serves an SSH-like banner (stands in for a guest sshd).
func sshBanner(t *testing.T) int {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			fmt.Fprintf(c, "SSH-2.0-fake\r\n")
			c.Close()
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port
}

func freePort(t *testing.T) int32 {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()
	return int32(ln.Addr().(*net.TCPAddr).Port)
}

func TestEnrollConnectReconcileForward(t *testing.T) {
	ca, caKey := newCert(t, &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Tossbox CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}, nil, nil)
	leaf, leafKey := newCert(t, &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "dashboard"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, KeyUsage: x509.KeyUsageDigitalSignature}, ca, caKey)
	pool := x509.NewCertPool()
	pool.AddCert(ca)

	bannerPort := sshBanner(t)
	pub := freePort(t)
	spec := &agentv1.DeviceSpec{DeviceId: "dev-1", Hostname: "box", Driver: "fake", Power: agentv1.PowerState_POWER_STATE_RUNNING, Revision: 1,
		Template: &agentv1.TemplateSpec{Id: "t", Readiness: "ssh"},
		Services: []*agentv1.ServiceSpec{{Name: "ssh", InternalPort: int32(bannerPort), PublicPort: pub, Username: "u", Password: "p"}}}

	dash := &fakeDash{ca: ca, caKey: caKey, secret: "s3cret", welcome: []*agentv1.DeviceSpec{spec},
		statuses: make(chan *agentv1.DeviceStatus, 100), hello: make(chan *agentv1.Hello, 2), toAgent: make(chan *agentv1.DashboardMessage, 4)}
	srv := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{leaf.Raw, ca.Raw}, PrivateKey: leafKey}},
		ClientAuth:   tls.VerifyClientCertIfGiven, ClientCAs: pool,
	})))
	agentv1.RegisterEnrollmentServer(srv, dash)
	agentv1.RegisterNodeControlServer(srv, dash)
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	go srv.Serve(ln)
	defer srv.Stop()

	tok := enroll.Token{CAFingerprint: enroll.Fingerprint(ca.Raw), Secret: "s3cret"}
	cfg, err := config.Parse([]byte(fmt.Sprintf("name: test\ndashboard: {endpoint: %q, token: %q}\ndata_dir: %q\nlisten_address: 127.0.0.1\ndrivers: {fake: }\n",
		ln.Addr().String(), tok.String(), t.TempDir())))
	if err != nil {
		t.Fatal(err)
	}
	a, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.Run(ctx)

	select {
	case h := <-dash.hello:
		if len(h.Capabilities.GetDrivers()) != 1 || h.Capabilities.Drivers[0].Name != "fake" {
			t.Fatalf("hello caps %+v", h.Capabilities)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no hello")
	}

	waitFor(t, dash.statuses, func(s *agentv1.DeviceStatus) bool {
		return s.DeviceId == "dev-1" && s.State == agentv1.DeviceState_DEVICE_STATE_RUNNING && s.Ready
	})

	// Port forward reaches the "guest".
	c, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(int(pub)), 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	line, _ := bufio.NewReader(c).ReadString('\n')
	c.Close()
	if !strings.HasPrefix(line, "SSH-2.0-fake") {
		t.Fatalf("forwarded banner %q", line)
	}

	// Power off: forward goes away.
	stopped := proto.Clone(spec).(*agentv1.DeviceSpec)
	stopped.Power, stopped.Revision = agentv1.PowerState_POWER_STATE_STOPPED, 2
	dash.toAgent <- &agentv1.DashboardMessage{Msg: &agentv1.DashboardMessage_ApplyDevice{ApplyDevice: &agentv1.ApplyDevice{Device: stopped}}}
	waitFor(t, dash.statuses, func(s *agentv1.DeviceStatus) bool { return s.State == agentv1.DeviceState_DEVICE_STATE_STOPPED })
	if p := a.fwd.Ports("dev-1"); len(p) != 0 {
		t.Fatalf("ports still open after stop: %v", p)
	}

	dash.toAgent <- &agentv1.DashboardMessage{Msg: &agentv1.DashboardMessage_DeleteDevice{DeleteDevice: &agentv1.DeleteDevice{DeviceId: "dev-1"}}}
	waitFor(t, dash.statuses, func(s *agentv1.DeviceStatus) bool { return s.State == agentv1.DeviceState_DEVICE_STATE_DESTROYED })
	if theFake.created != 1 {
		t.Fatalf("created %d times", theFake.created)
	}
	// State persisted: no devices left.
	saved, err := a.mgr.store.load()
	if err != nil || len(saved) != 0 {
		t.Fatalf("saved state %v %v", saved, err)
	}
}

func waitFor(t *testing.T, ch chan *agentv1.DeviceStatus, ok func(*agentv1.DeviceStatus) bool) {
	t.Helper()
	deadline := time.After(20 * time.Second)
	for {
		select {
		case s := <-ch:
			if ok(s) {
				return
			}
		case <-deadline:
			t.Fatal(errors.New("timeout waiting for status"))
		}
	}
}

func TestWrongTokenRejected(t *testing.T) {
	if _, err := enroll.ParseToken("tb1.zz.s"); err == nil {
		t.Fatal("expected parse error")
	}
}
