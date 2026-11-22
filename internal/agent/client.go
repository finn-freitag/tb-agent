package agent

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/finn-freitag/tb-proto/enroll"
	agentv1 "github.com/finn-freitag/tb-proto/gen/agent/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/status"

	"github.com/finn-freitag/tb-agent/internal/version"
)

// identity is the persisted enrollment result (<data_dir>/node.json + PEMs).
type identity struct {
	NodeID    string `json:"node_id"`
	NodeName  string `json:"node_name"`
	TokenHash string `json:"token_hash"` // sha256 of the token used to enroll
	Endpoint  string `json:"endpoint"`
}

func (a *Agent) certPaths() (ca, cert, key, meta string) {
	d := a.cfg.DataDir
	return filepath.Join(d, "ca.pem"), filepath.Join(d, "client.pem"), filepath.Join(d, "client-key.pem"), filepath.Join(d, "node.json")
}

func tokenHash(tok string) string {
	if tok == "" {
		return ""
	}
	h := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(h[:])
}

// loadIdentity returns the enrolled identity, or nil if (re-)enrollment is needed.
func (a *Agent) loadIdentity() (*identity, *tls.Config, error) {
	caP, certP, keyP, metaP := a.certPaths()
	b, err := os.ReadFile(metaP)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	var id identity
	if err := json.Unmarshal(b, &id); err != nil {
		return nil, nil, err
	}
	// A different token in the config means the admin issued a new one.
	if a.cfg.Dashboard.Token != "" && tokenHash(a.cfg.Dashboard.Token) != id.TokenHash {
		a.log.Info("enrollment token changed in config; re-enrolling")
		return nil, nil, nil
	}
	cert, err := tls.LoadX509KeyPair(certP, keyP)
	if err != nil {
		return nil, nil, err
	}
	caPEM, err := os.ReadFile(caP)
	if err != nil {
		return nil, nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, nil, errors.New("agent: invalid ca.pem")
	}
	host, _, _ := net.SplitHostPort(a.cfg.Dashboard.Endpoint)
	return &id, &tls.Config{
		MinVersion:   tls.VersionTLS12,
		RootCAs:      pool,
		Certificates: []tls.Certificate{cert},
		ServerName:   host,
	}, nil
}

// enroll exchanges the one-time token for a client certificate.
func (a *Agent) enroll(ctx context.Context) error {
	if a.cfg.Dashboard.Token == "" {
		return errors.New("agent: not enrolled and no dashboard.token configured (create the node in the dashboard to get one)")
	}
	tok, err := enroll.ParseToken(a.cfg.Dashboard.Token)
	if err != nil {
		return err
	}
	host, _, _ := net.SplitHostPort(a.cfg.Dashboard.Endpoint)
	tlsCfg := &tls.Config{
		MinVersion:            tls.VersionTLS12,
		ServerName:            host,
		InsecureSkipVerify:    true, // verified by the pinned CA fingerprint below
		VerifyPeerCertificate: enroll.VerifyPinnedChain(tok.CAFingerprint),
	}
	conn, err := grpc.NewClient(a.cfg.Dashboard.Endpoint, grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg)))
	if err != nil {
		return err
	}
	defer conn.Close()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: a.cfg.Name}}, key)
	if err != nil {
		return err
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	resp, err := agentv1.NewEnrollmentClient(conn).Enroll(cctx, &agentv1.EnrollRequest{Token: tok.Secret, CsrDer: csr, AgentVersion: version.Version})
	if err != nil {
		return fmt.Errorf("agent: enrollment failed: %w", err)
	}
	if enroll.Fingerprint(resp.CaCertDer) != tok.CAFingerprint {
		return errors.New("agent: dashboard returned a CA that does not match the token")
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	caP, certP, keyP, metaP := a.certPaths()
	if err := os.MkdirAll(a.cfg.DataDir, 0o700); err != nil {
		return err
	}
	files := []struct {
		path string
		data []byte
		mode os.FileMode
	}{
		{keyP, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600},
		{certP, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: resp.ClientCertDer}), 0o644},
		{caP, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: resp.CaCertDer}), 0o644},
	}
	meta, _ := json.MarshalIndent(identity{NodeID: resp.NodeId, NodeName: resp.NodeName, TokenHash: tokenHash(a.cfg.Dashboard.Token), Endpoint: a.cfg.Dashboard.Endpoint}, "", "  ")
	files = append(files, struct {
		path string
		data []byte
		mode os.FileMode
	}{metaP, meta, 0o644})
	for _, f := range files {
		if err := os.WriteFile(f.path, f.data, f.mode); err != nil {
			return err
		}
	}
	a.log.Info("enrolled", "node_id", resp.NodeId, "node_name", resp.NodeName)
	return nil
}

// runClient keeps a control stream to the dashboard open until ctx ends.
func (a *Agent) runClient(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		start := time.Now()
		err := a.session(ctx)
		if ctx.Err() != nil {
			return
		}
		if time.Since(start) > time.Minute {
			backoff = time.Second
		}
		a.log.Warn("dashboard connection lost", "err", err, "retry_in", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

func (a *Agent) session(ctx context.Context) error {
	id, tlsCfg, err := a.loadIdentity()
	if err != nil {
		return err
	}
	if id == nil {
		if err := a.enroll(ctx); err != nil {
			return err
		}
		if id, tlsCfg, err = a.loadIdentity(); err != nil || id == nil {
			return fmt.Errorf("agent: identity after enrollment: %v", err)
		}
	}
	conn, err := grpc.NewClient(a.cfg.Dashboard.Endpoint,
		grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg)),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{Time: 30 * time.Second, Timeout: 20 * time.Second, PermitWithoutStream: true}),
	)
	if err != nil {
		return err
	}
	defer conn.Close()
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream, err := agentv1.NewNodeControlClient(conn).Connect(sctx)
	if err != nil {
		return err
	}
	// Drop stale events; Hello carries the full current state.
	for drained := false; !drained; {
		select {
		case <-a.mgr.Events():
		default:
			drained = true
		}
	}
	caps := a.capabilities(ctx)
	if err := stream.Send(&agentv1.AgentMessage{Msg: &agentv1.AgentMessage_Hello{Hello: &agentv1.Hello{
		AgentVersion: version.Version, Capabilities: caps, Devices: a.mgr.Statuses(),
	}}}); err != nil {
		return err
	}

	sendCh := make(chan *agentv1.AgentMessage, 64)
	errCh := make(chan error, 2)
	go func() { // receiver
		for {
			msg, err := stream.Recv()
			if err != nil {
				errCh <- err
				return
			}
			a.handle(msg, sendCh)
		}
	}()

	hb := time.NewTicker(15 * time.Second)
	defer hb.Stop()
	capT := time.NewTicker(60 * time.Second)
	defer capT.Stop()
	for {
		var out *agentv1.AgentMessage
		select {
		case <-ctx.Done():
			_ = stream.CloseSend()
			return ctx.Err()
		case err := <-errCh:
			if s, ok := status.FromError(err); ok && (s.Code() == codes.Unauthenticated || s.Code() == codes.PermissionDenied) {
				a.log.Error("dashboard rejected this node; put a new enrollment token into the config if the node was re-created", "err", err)
			}
			return err
		case st := <-a.mgr.Events():
			out = &agentv1.AgentMessage{Msg: &agentv1.AgentMessage_DeviceStatus{DeviceStatus: st}}
		case m := <-sendCh:
			out = m
		case <-hb.C:
			ram, storage := a.mgr.Usage()
			out = &agentv1.AgentMessage{Msg: &agentv1.AgentMessage_Heartbeat{Heartbeat: &agentv1.Heartbeat{Usage: &agentv1.ResourceUsage{
				RamUsedMb: ram, CpuUsedPercent: a.cpuPercent(), StorageUsedGb: storage / 1024,
			}}}}
		case <-capT.C:
			if c := a.capabilities(ctx); !protoEqual(c, caps) {
				caps = c
				out = &agentv1.AgentMessage{Msg: &agentv1.AgentMessage_CapabilitiesChanged{CapabilitiesChanged: &agentv1.CapabilitiesChanged{Capabilities: c}}}
			}
		}
		if out != nil {
			if err := stream.Send(out); err != nil {
				return err
			}
		}
	}
}

func (a *Agent) handle(msg *agentv1.DashboardMessage, send chan<- *agentv1.AgentMessage) {
	switch m := msg.Msg.(type) {
	case *agentv1.DashboardMessage_Welcome:
		a.log.Info("connected to dashboard", "node_id", m.Welcome.NodeId, "node_name", m.Welcome.NodeName, "devices", len(m.Welcome.Devices))
		a.mgr.SetDesired(m.Welcome.Devices)
	case *agentv1.DashboardMessage_ApplyDevice:
		if m.ApplyDevice.Device != nil {
			a.mgr.Apply(m.ApplyDevice.Device)
		}
	case *agentv1.DashboardMessage_DeleteDevice:
		a.mgr.Delete(m.DeleteDevice.DeviceId)
	case *agentv1.DashboardMessage_Ping:
		select {
		case send <- &agentv1.AgentMessage{Msg: &agentv1.AgentMessage_Pong{Pong: &agentv1.Pong{Nonce: m.Ping.Nonce}}}:
		default:
		}
	}
}
