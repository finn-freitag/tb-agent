package agent

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"time"

	agentv1 "github.com/finn-freitag/tb-proto/gen/agent/v1"
	"google.golang.org/protobuf/proto"

	"github.com/finn-freitag/tb-agent/internal/driver"
)

// EndpointResolver: see driver.EndpointResolver.
type EndpointResolver = driver.EndpointResolver

const (
	pollInterval   = 5 * time.Second
	resyncInterval = 30 * time.Second
	readyRecheck   = time.Minute
)

type device struct {
	id       string
	driver   string
	spec     *agentv1.DeviceSpec // nil for discovered devices without desired state
	deleting bool

	kick chan struct{}
	busy bool // Ensure/Destroy in progress

	appliedRev int64
	ensured    bool
	failures   int
	retryAt    time.Time

	status    *agentv1.DeviceStatus
	readyAt   time.Time
	destroyed bool
}

// Manager reconciles desired device specs with the drivers.
type Manager struct {
	log      *slog.Logger
	drivers  map[string]driver.Driver
	licenses driver.Licenses
	store    *Store
	fwd      *Forwarder

	mu      sync.Mutex
	devices map[string]*device
	ctx     context.Context
	events  chan *agentv1.DeviceStatus
}

// NewManager creates a manager. Call Start before use.
func NewManager(log *slog.Logger, drivers map[string]driver.Driver, lic driver.Licenses, store *Store, fwd *Forwarder) *Manager {
	return &Manager{
		log: log, drivers: drivers, licenses: lic, store: store, fwd: fwd,
		devices: map[string]*device{},
		events:  make(chan *agentv1.DeviceStatus, 512),
	}
}

// Events delivers status changes (to be sent to the dashboard).
func (m *Manager) Events() <-chan *agentv1.DeviceStatus { return m.events }

// Start loads persisted state, discovers existing devices and starts workers.
func (m *Manager) Start(ctx context.Context) error {
	m.mu.Lock()
	m.ctx = ctx
	m.mu.Unlock()
	saved, err := m.store.load()
	if err != nil {
		return fmt.Errorf("agent: load state: %w", err)
	}
	m.mu.Lock()
	for id, p := range saved {
		d := m.newDevice(id, p.Driver)
		d.spec, d.deleting = p.Spec, p.Deleting
	}
	m.mu.Unlock()
	for name, drv := range m.drivers {
		list, err := drv.List(ctx)
		if err != nil {
			m.log.Warn("cannot list devices", "driver", name, "err", err)
			continue
		}
		m.mu.Lock()
		for _, st := range list {
			d, ok := m.devices[st.DeviceId]
			if !ok {
				m.log.Info("found device without desired state; waiting for dashboard", "device", st.DeviceId, "driver", name)
				d = m.newDevice(st.DeviceId, name)
			}
			d.status = st
		}
		m.mu.Unlock()
	}
	m.mu.Lock()
	for _, d := range m.devices {
		go m.worker(d)
		m.kickLocked(d)
	}
	m.mu.Unlock()
	go m.poller(ctx)
	return nil
}

func (m *Manager) newDevice(id, drv string) *device {
	d := &device{id: id, driver: drv, kick: make(chan struct{}, 1)}
	m.devices[id] = d
	return d
}

func (m *Manager) kickLocked(d *device) {
	select {
	case d.kick <- struct{}{}:
	default:
	}
}

func (m *Manager) persistLocked() {
	out := map[string]persisted{}
	for id, d := range m.devices {
		if d.spec == nil && !d.deleting {
			continue
		}
		out[id] = persisted{Driver: d.driver, Spec: d.spec, Deleting: d.deleting}
	}
	if err := m.store.save(out); err != nil {
		m.log.Error("cannot persist state", "err", err)
	}
}

// Statuses returns the last known status of every device.
func (m *Manager) Statuses() []*agentv1.DeviceStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*agentv1.DeviceStatus, 0, len(m.devices))
	for _, d := range m.devices {
		out = append(out, m.statusLocked(d))
	}
	return out
}

func (m *Manager) statusLocked(d *device) *agentv1.DeviceStatus {
	if d.status == nil {
		st := agentv1.DeviceState_DEVICE_STATE_PENDING
		if d.deleting {
			st = agentv1.DeviceState_DEVICE_STATE_DESTROYING
		}
		return &agentv1.DeviceStatus{DeviceId: d.id, State: st, UpdatedUnix: time.Now().Unix()}
	}
	return proto.Clone(d.status).(*agentv1.DeviceStatus)
}

// Usage sums resources of running devices.
func (m *Manager) Usage() (ramMB, storageMB int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, d := range m.devices {
		if d.status != nil {
			storageMB += d.status.StorageUsedMb
			if d.status.State == agentv1.DeviceState_DEVICE_STATE_RUNNING && d.spec != nil {
				ramMB += d.spec.GetResources().GetRamMb()
			}
		}
	}
	return
}

// SetDesired replaces the complete desired state (Welcome). Devices not in
// specs are destroyed.
func (m *Manager) SetDesired(specs []*agentv1.DeviceSpec) {
	want := map[string]bool{}
	for _, s := range specs {
		want[s.DeviceId] = true
		m.Apply(s)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, d := range m.devices {
		if !want[id] && !d.deleting {
			m.log.Info("device not in desired state, destroying", "device", id)
			d.deleting = true
			m.kickLocked(d)
		}
	}
	m.persistLocked()
}

// Apply sets the desired spec of one device.
func (m *Manager) Apply(spec *agentv1.DeviceSpec) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.devices[spec.DeviceId]
	if ok && d.deleting {
		m.log.Warn("ignoring spec for device being destroyed", "device", spec.DeviceId)
		return
	}
	if ok && d.spec != nil && spec.Revision != 0 && spec.Revision < d.spec.Revision {
		return // stale
	}
	if !ok {
		d = m.newDevice(spec.DeviceId, spec.Driver)
		go m.worker(d)
	}
	if d.driver == "" {
		d.driver = spec.Driver
	}
	d.spec = proto.Clone(spec).(*agentv1.DeviceSpec)
	d.failures, d.retryAt = 0, time.Time{}
	m.persistLocked()
	m.kickLocked(d)
}

// Delete marks a device for destruction.
func (m *Manager) Delete(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.devices[id]
	if !ok {
		// Unknown: confirm so the dashboard can forget it.
		m.emitLocked(&agentv1.DeviceStatus{DeviceId: id, State: agentv1.DeviceState_DEVICE_STATE_DESTROYED, UpdatedUnix: time.Now().Unix()})
		return
	}
	d.deleting = true
	m.persistLocked()
	m.kickLocked(d)
}

func (m *Manager) emitLocked(st *agentv1.DeviceStatus) {
	select {
	case m.events <- proto.Clone(st).(*agentv1.DeviceStatus):
	default:
		// Dashboard not draining (disconnected): drop; Hello carries full state.
	}
}

// setStatusLocked stores and emits a status if it changed meaningfully.
func (m *Manager) setStatusLocked(d *device, st *agentv1.DeviceStatus) {
	old := d.status
	st.UpdatedUnix = time.Now().Unix()
	d.status = st
	if old == nil || old.State != st.State || old.Ready != st.Ready || old.Message != st.Message ||
		old.InternalIp != st.InternalIp || abs(old.StorageUsedMb-st.StorageUsedMb) >= 256 {
		m.emitLocked(st)
	}
}

func abs(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

func (m *Manager) worker(d *device) {
	ctx := m.ctx
	t := time.NewTicker(resyncInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-d.kick:
		case <-t.C:
		}
		if m.step(ctx, d) {
			return
		}
	}
}

// step performs one reconcile step; returns true when the device is gone.
func (m *Manager) step(ctx context.Context, d *device) bool {
	m.mu.Lock()
	drv := m.drivers[d.driver]
	deleting := d.deleting
	var spec *agentv1.DeviceSpec
	if d.spec != nil {
		spec = proto.Clone(d.spec).(*agentv1.DeviceSpec)
	}
	if !d.retryAt.IsZero() && time.Now().Before(d.retryAt) {
		m.mu.Unlock()
		return false
	}
	if drv == nil {
		if deleting || spec != nil {
			m.setStatusLocked(d, &agentv1.DeviceStatus{DeviceId: d.id, State: agentv1.DeviceState_DEVICE_STATE_FAILED,
				Message: fmt.Sprintf("driver %q is not enabled on this node", d.driver)})
		}
		if deleting { // nothing we can do; forget it
			delete(m.devices, d.id)
			m.persistLocked()
			m.emitLocked(&agentv1.DeviceStatus{DeviceId: d.id, State: agentv1.DeviceState_DEVICE_STATE_DESTROYED})
			m.mu.Unlock()
			return true
		}
		m.mu.Unlock()
		return false
	}
	needEnsure := false
	if !deleting && spec != nil {
		needEnsure = !d.ensured || d.appliedRev != spec.Revision || powerMismatch(spec, d.status)
	}
	if deleting || needEnsure {
		d.busy = true
	}
	fresh := d.status == nil
	m.mu.Unlock()

	switch {
	case deleting:
		m.mu.Lock()
		m.setStatusLocked(d, &agentv1.DeviceStatus{DeviceId: d.id, State: agentv1.DeviceState_DEVICE_STATE_DESTROYING, Message: "Destroying"})
		m.mu.Unlock()
		m.fwd.Remove(d.id)
		err := drv.Destroy(ctx, d.id)
		m.mu.Lock()
		defer m.mu.Unlock()
		d.busy = false
		if err != nil {
			m.failLocked(d, "destroy failed: "+err.Error())
			return false
		}
		m.licenses.Release(d.id)
		delete(m.devices, d.id)
		m.persistLocked()
		m.setStatusLocked(d, &agentv1.DeviceStatus{DeviceId: d.id, State: agentv1.DeviceState_DEVICE_STATE_DESTROYED})
		m.log.Info("device destroyed", "device", d.id)
		return true

	case needEnsure:
		report := func(state agentv1.DeviceState, msg string) {
			m.mu.Lock()
			defer m.mu.Unlock()
			st := m.statusLocked(d)
			st.State, st.Message = state, msg
			m.setStatusLocked(d, st)
		}
		if fresh {
			report(agentv1.DeviceState_DEVICE_STATE_PROVISIONING, "Provisioning")
		}
		err := drv.Ensure(ctx, spec, report)
		m.mu.Lock()
		d.busy = false
		if err != nil {
			m.log.Error("ensure failed", "device", d.id, "err", err)
			m.failLocked(d, err.Error())
			m.mu.Unlock()
			return false
		}
		d.ensured, d.appliedRev, d.failures, d.retryAt = true, spec.Revision, 0, time.Time{}
		m.mu.Unlock()
	}
	m.refresh(ctx, d)
	return false
}

func (m *Manager) failLocked(d *device, msg string) {
	d.failures++
	backoff := time.Duration(1<<min(d.failures, 8)) * 5 * time.Second
	if backoff > 10*time.Minute {
		backoff = 10 * time.Minute
	}
	d.retryAt = time.Now().Add(backoff)
	st := m.statusLocked(d)
	st.State, st.Ready, st.Message = agentv1.DeviceState_DEVICE_STATE_FAILED, false, msg
	m.setStatusLocked(d, st)
	time.AfterFunc(backoff, func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		m.kickLocked(d)
	})
}

func powerMismatch(spec *agentv1.DeviceSpec, st *agentv1.DeviceStatus) bool {
	if st == nil {
		return true
	}
	switch st.State {
	case agentv1.DeviceState_DEVICE_STATE_RUNNING:
		return spec.Power == agentv1.PowerState_POWER_STATE_STOPPED
	case agentv1.DeviceState_DEVICE_STATE_STOPPED:
		return spec.Power != agentv1.PowerState_POWER_STATE_STOPPED
	case agentv1.DeviceState_DEVICE_STATE_DESTROYED:
		return true // vanished: recreate
	}
	return false
}

func (m *Manager) poller(ctx context.Context) {
	t := time.NewTicker(pollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			m.fwd.Close()
			return
		case <-t.C:
		}
		m.mu.Lock()
		var ds []*device
		for _, d := range m.devices {
			if !d.busy && !d.deleting {
				ds = append(ds, d)
			}
		}
		m.mu.Unlock()
		for _, d := range ds {
			m.refresh(ctx, d)
		}
	}
}

// refresh polls the driver, runs readiness checks and syncs port forwards.
func (m *Manager) refresh(ctx context.Context, d *device) {
	m.mu.Lock()
	drv := m.drivers[d.driver]
	var spec *agentv1.DeviceSpec
	if d.spec != nil {
		spec = proto.Clone(d.spec).(*agentv1.DeviceSpec)
	}
	prev := m.statusLocked(d)
	readyAt := d.readyAt
	failed := d.status != nil && d.status.State == agentv1.DeviceState_DEVICE_STATE_FAILED && !d.retryAt.IsZero()
	m.mu.Unlock()
	if drv == nil || failed {
		return
	}
	st, err := drv.Status(ctx, d.id)
	if err != nil {
		m.log.Warn("status failed", "device", d.id, "err", err)
		return
	}
	// Keep a provisioning message while the guest boots for the first time.
	if st.Message == "" && prev.State == agentv1.DeviceState_DEVICE_STATE_PROVISIONING && st.State == agentv1.DeviceState_DEVICE_STATE_RUNNING && !prev.Ready {
		st.Message = "Booting"
	}

	var rules []Rule
	if st.State == agentv1.DeviceState_DEVICE_STATE_RUNNING && spec != nil {
		for _, svc := range spec.Services {
			rules = append(rules, Rule{PublicPort: svc.PublicPort, Service: svc.Name, Target: m.endpoint(ctx, drv, d.id, st.InternalIp, svc.Name, svc.InternalPort)})
		}
		// Readiness.
		kind := spec.GetTemplate().GetReadiness()
		if prev.Ready && time.Since(readyAt) < readyRecheck && prev.InternalIp == st.InternalIp {
			st.Ready = true
		} else {
			port := defaultPorts[kind]
			for _, svc := range spec.Services {
				if svc.Name == kind && svc.InternalPort > 0 {
					port = svc.InternalPort
				}
			}
			ep := ""
			if kind == "none" || kind == "" {
				ep = "-"
			} else {
				ep = m.endpoint(ctx, drv, d.id, st.InternalIp, kind, port)
			}
			st.Ready = checkReady(ctx, kind, ep)
			if st.Ready {
				readyAt = time.Now()
			}
		}
		if st.Ready && st.Message == "Booting" {
			st.Message = ""
		} else if !st.Ready && st.Message == "" {
			st.Message = "Booting"
		}
	}
	m.fwd.Sync(d.id, rules)

	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.devices[d.id]; !ok || d.busy || d.deleting {
		return
	}
	d.readyAt = readyAt
	m.setStatusLocked(d, st)
	if spec != nil && d.ensured && powerMismatch(spec, st) {
		m.kickLocked(d)
	}
}

func (m *Manager) endpoint(ctx context.Context, drv driver.Driver, id, ip, service string, port int32) string {
	if r, ok := drv.(EndpointResolver); ok {
		if ep, handled := r.ServiceEndpoint(ctx, id, service); handled {
			return ep
		}
	}
	if ip == "" {
		return ""
	}
	if port == 0 {
		port = defaultPorts[service]
	}
	return net.JoinHostPort(ip, strconv.Itoa(int(port)))
}
