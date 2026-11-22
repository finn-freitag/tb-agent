// Package agent is the node agent core: it enrolls with the dashboard, keeps
// the control stream open, reconciles desired device state with the enabled
// drivers, runs readiness checks and forwards service ports.
package agent

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	agentv1 "github.com/finn-freitag/tb-proto/gen/agent/v1"
	"google.golang.org/protobuf/proto"

	"github.com/finn-freitag/tb-agent/internal/config"
	"github.com/finn-freitag/tb-agent/internal/driver"
	"github.com/finn-freitag/tb-agent/internal/hostinfo"
	"github.com/finn-freitag/tb-agent/internal/images"
	"github.com/finn-freitag/tb-agent/internal/licenses"
)

// Agent wires everything together.
type Agent struct {
	cfg      *config.Config
	log      *slog.Logger
	drivers  map[string]driver.Driver
	licenses *licenses.Manager
	images   *images.Store
	mgr      *Manager
	fwd      *Forwarder
}

// New builds the agent and instantiates all enabled drivers.
func New(cfg *config.Config, log *slog.Logger) (*Agent, error) {
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return nil, fmt.Errorf("agent: data dir: %w", err)
	}
	img, err := images.New(filepath.Join(cfg.DataDir, "images"), log.With("component", "images"))
	if err != nil {
		return nil, err
	}
	lic, err := licenses.New(cfg.LicensePools, cfg.DataDir)
	if err != nil {
		return nil, err
	}
	a := &Agent{cfg: cfg, log: log, drivers: map[string]driver.Driver{}, licenses: lic, images: img}
	for _, name := range cfg.DriverNames() {
		dir := filepath.Join(cfg.DataDir, "drivers", name)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
		drv, err := driver.New(name, driver.Env{
			NodeName: cfg.Name,
			DataDir:  dir,
			Images:   img,
			Licenses: lic,
			Log:      log.With("driver", name),
		}, cfg.DriverConfig(name))
		if err != nil {
			return nil, fmt.Errorf("agent: driver %s: %w", name, err)
		}
		a.drivers[name] = drv
	}
	a.fwd = NewForwarder(cfg.ListenAddress, log.With("component", "forward"))
	a.mgr = NewManager(log.With("component", "reconciler"), a.drivers, lic, newStore(cfg.DataDir), a.fwd)
	return a, nil
}

// Manager exposes the reconciler (used by tests and tools).
func (a *Agent) Manager() *Manager { return a.mgr }

// Run starts the reconciler and the dashboard connection; blocks until ctx ends.
func (a *Agent) Run(ctx context.Context) error {
	if err := a.mgr.Start(ctx); err != nil {
		return err
	}
	a.runClient(ctx)
	return nil
}

// Capabilities reports what this node can do (sent in Hello).
func (a *Agent) Capabilities(ctx context.Context) *agentv1.Capabilities { return a.capabilities(ctx) }

func (a *Agent) capabilities(ctx context.Context) *agentv1.Capabilities {
	total, _ := hostinfo.StorageGB(a.cfg.DataDir)
	c := &agentv1.Capabilities{
		Arch:          hostinfo.Arch(),
		Os:            hostinfo.OS(),
		HostCores:     hostinfo.Cores(),
		HostRamMb:     hostinfo.RAMMB(),
		HostStorageGb: total,
		Limits: &agentv1.NodeLimits{
			RamMb:      a.cfg.RAMLimitMB,
			CpuPercent: a.cfg.CPULimitPct,
			StorageGb:  a.cfg.StorageLimitGB,
		},
		LicensePools: a.licenses.Pools(),
	}
	for _, name := range a.cfg.DriverNames() {
		dc := a.drivers[name].Capabilities(ctx)
		if dc == nil {
			dc = &agentv1.DriverCapabilities{Name: name}
		}
		c.Drivers = append(c.Drivers, dc)
	}
	return c
}

func (a *Agent) cpuPercent() float64 { return hostinfo.CPUPercent() }

func protoEqual(a, b proto.Message) bool { return proto.Equal(a, b) }
