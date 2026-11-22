// Package usbphone implements the experimental "usb-phone" driver: physical
// Android phones and iPhones attached to the node over USB are leased as
// devices instead of being created.
//
//   - Detection: `adb devices -l` (Android) and `idevice_id -l`
//     (libimobiledevice, iOS) if installed.
//   - Ensure = lease a free phone (param "serial", or any free phone of the
//     template family), run the optional setup command and expose adb.
//   - Destroy = reset the phone (Android: remove third-party apps and user
//     files, or an opt-in fastboot wipe; iOS: a configured reset command, e.g.
//     a cfgutil/pymobiledevice3 script for supervised devices) and release it.
//   - Power: phones are never switched off; "stopped" closes the adb forward
//     and turns the screen off.
//
// adb exposure: `adb tcpip 5555` makes adbd listen on TCP inside the phone,
// `adb forward tcp:<p> tcp:5555` tunnels the local port 127.0.0.1:<p> over
// USB, and the driver reports that endpoint via driver.EndpointResolver.
package usbphone

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/finn-freitag/tb-agent/internal/driver"
	agentv1 "github.com/finn-freitag/tb-proto/gen/agent/v1"
	"gopkg.in/yaml.v3"
)

const name = "usb-phone"

// Config is the driver's node config section.
//
//	drivers:
//	  usb-phone:
//	    adb: adb
//	    fastboot: fastboot
//	    idevice_id: idevice_id
//	    forward_port_base: 15000
//	    android:
//	      fastboot_wipe: false        # DANGEROUS: full wipe via fastboot -w (unlocked bootloader; adb must be enabled by the image afterwards)
//	      keep_packages: []           # third-party packages not removed on reset
//	      setup_command: ""           # run after leasing: <cmd> <serial>
//	      reset_command: ""           # replaces the built-in reset: <cmd> <serial>
//	    ios:
//	      setup_command: ""           # <cmd> <udid>
//	      reset_command: ""           # required to lease iPhones: <cmd> <udid>
type Config struct {
	ADB             string        `yaml:"adb"`
	Fastboot        string        `yaml:"fastboot"`
	IDeviceID       string        `yaml:"idevice_id"`
	ForwardPortBase int           `yaml:"forward_port_base"`
	Android         AndroidFlow   `yaml:"android"`
	IOS             IOSFlow       `yaml:"ios"`
	CommandTimeout  time.Duration `yaml:"command_timeout"`
}

// AndroidFlow configures Android reset/setup.
type AndroidFlow struct {
	FastbootWipe bool     `yaml:"fastboot_wipe"`
	KeepPackages []string `yaml:"keep_packages"`
	SetupCommand string   `yaml:"setup_command"`
	ResetCommand string   `yaml:"reset_command"`
}

// IOSFlow configures iPhone reset/setup.
type IOSFlow struct {
	SetupCommand string `yaml:"setup_command"`
	ResetCommand string `yaml:"reset_command"`
}

func (c *Config) defaults() {
	if c.ADB == "" {
		c.ADB = "adb"
	}
	if c.Fastboot == "" {
		c.Fastboot = "fastboot"
	}
	if c.IDeviceID == "" {
		c.IDeviceID = "idevice_id"
	}
	if c.ForwardPortBase == 0 {
		c.ForwardPortBase = 15000
	}
	if c.CommandTimeout == 0 {
		c.CommandTimeout = 15 * time.Minute
	}
}

// lease is persisted per device in <DataDir>/leases/<id>.json.
type lease struct {
	DeviceID string `json:"device_id"`
	Serial   string `json:"serial"`
	Family   string `json:"family"`
	Slot     int    `json:"slot"` // forward port index, 1..254
	Stopped  bool   `json:"stopped"`
	ADB      bool   `json:"adb"`
}

type runFunc func(ctx context.Context, bin string, args ...string) (string, error)

// Driver is the usb-phone driver.
type Driver struct {
	cfg Config
	env driver.Env
	run runFunc

	mu       sync.Mutex
	forwards map[string]bool // device id -> adb forward active
}

func init() {
	driver.Register(name, func(env driver.Env, node *yaml.Node) (driver.Driver, error) {
		var cfg Config
		if err := driver.DecodeConfig(node, &cfg); err != nil {
			return nil, fmt.Errorf("usb-phone config: %w", err)
		}
		return New(env, cfg), nil
	})
}

// New creates the driver.
func New(env driver.Env, cfg Config) *Driver {
	cfg.defaults()
	return &Driver{cfg: cfg, env: env, run: execRun, forwards: map[string]bool{}}
}

func execRun(ctx context.Context, bin string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, bin, args...).CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s %s: %v: %s", bin, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// Name implements driver.Driver.
func (d *Driver) Name() string { return name }

func (d *Driver) leaseDir() string { return filepath.Join(d.env.DataDir, "leases") }

func (d *Driver) loadLease(id string) (*lease, error) {
	b, err := os.ReadFile(filepath.Join(d.leaseDir(), id+".json"))
	if err != nil {
		return nil, err
	}
	var l lease
	return &l, json.Unmarshal(b, &l)
}

func (d *Driver) saveLease(l *lease) error {
	if err := os.MkdirAll(d.leaseDir(), 0o750); err != nil {
		return err
	}
	b, _ := json.Marshal(l)
	return os.WriteFile(filepath.Join(d.leaseDir(), l.DeviceID+".json"), b, 0o640)
}

func (d *Driver) leases() []*lease {
	entries, _ := os.ReadDir(d.leaseDir())
	var out []*lease
	for _, e := range entries {
		if id, ok := strings.CutSuffix(e.Name(), ".json"); ok {
			if l, err := d.loadLease(id); err == nil {
				out = append(out, l)
			}
		}
	}
	return out
}

// phone is an attached physical device.
type phone struct {
	Serial string
	Family string
	Model  string
	State  string // adb state (device, unauthorized, offline) or "device" for iOS
}

// parseADBDevices parses `adb devices -l` output.
func parseADBDevices(out string) []phone {
	var ps []phone
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 || strings.HasPrefix(line, "List of devices") || strings.HasPrefix(line, "*") {
			continue
		}
		p := phone{Serial: f[0], Family: "android", State: f[1]}
		for _, kv := range f[2:] {
			if m, ok := strings.CutPrefix(kv, "model:"); ok {
				p.Model = strings.ReplaceAll(m, "_", " ")
			}
		}
		// Skip network (adb connect) devices: only USB phones.
		if strings.Contains(p.Serial, ":") {
			continue
		}
		ps = append(ps, p)
	}
	return ps
}

func (d *Driver) attached(ctx context.Context) ([]phone, []string) {
	var ps []phone
	var problems []string
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if out, err := d.run(ctx, d.cfg.ADB, "devices", "-l"); err == nil {
		ps = append(ps, parseADBDevices(out)...)
	} else {
		problems = append(problems, "adb: "+firstLine(err.Error()))
	}
	if out, err := d.run(ctx, d.cfg.IDeviceID, "-l"); err == nil {
		for _, u := range strings.Fields(out) {
			ps = append(ps, phone{Serial: u, Family: "ios", Model: "iPhone", State: "device"})
		}
	} else if !errors.Is(err, exec.ErrNotFound) && !strings.Contains(err.Error(), "executable file not found") {
		problems = append(problems, "idevice_id: "+firstLine(err.Error()))
	}
	sort.Slice(ps, func(i, j int) bool { return ps[i].Serial < ps[j].Serial })
	return ps, problems
}

// Capabilities implements driver.Driver.
func (d *Driver) Capabilities(ctx context.Context) *agentv1.DriverCapabilities {
	ps, problems := d.attached(ctx)
	leased := map[string]bool{}
	for _, l := range d.leases() {
		leased[l.Serial] = true
	}
	caps := &agentv1.DriverCapabilities{Name: name, Services: []string{"adb"}}
	fam := map[string]bool{}
	for _, p := range ps {
		caps.PhysicalDevices = append(caps.PhysicalDevices, &agentv1.PhysicalDevice{
			Serial: p.Serial, Family: p.Family, Model: p.Model,
			Available: !leased[p.Serial] && p.State == "device" && (p.Family != "ios" || d.cfg.IOS.ResetCommand != ""),
		})
		fam[p.Family] = true
	}
	for f := range fam {
		caps.Families = append(caps.Families, f)
	}
	sort.Strings(caps.Families)
	caps.FixedSlots = int32(len(ps))
	caps.Healthy = len(problems) == 0 || len(ps) > 0
	caps.Message = strings.Join(problems, "; ")
	if len(ps) == 0 && caps.Message == "" {
		caps.Message = "no phones attached"
	}
	return caps
}

// pick chooses the phone for a new lease.
func pick(ps []phone, leases []*lease, family, serial string, iosAllowed bool) (phone, error) {
	used := map[string]bool{}
	for _, l := range leases {
		used[l.Serial] = true
	}
	for _, p := range ps {
		if serial != "" && p.Serial != serial {
			continue
		}
		if p.Family != family || used[p.Serial] {
			continue
		}
		if p.State != "device" {
			if serial != "" {
				return phone{}, fmt.Errorf("usb-phone: %s is %s (authorize USB debugging on the phone)", serial, p.State)
			}
			continue
		}
		if p.Family == "ios" && !iosAllowed {
			return phone{}, errors.New("usb-phone: iPhones need ios.reset_command in the node config")
		}
		return p, nil
	}
	if serial != "" {
		return phone{}, fmt.Errorf("usb-phone: phone %s is not attached or already in use", serial)
	}
	return phone{}, fmt.Errorf("usb-phone: no free %s phone attached", family)
}

func (d *Driver) freeSlot(leases []*lease) int {
	used := map[int]bool{}
	for _, l := range leases {
		used[l.Slot] = true
	}
	for i := 1; i < 255; i++ {
		if !used[i] {
			return i
		}
	}
	return 0
}

// Ensure implements driver.Driver.
func (d *Driver) Ensure(ctx context.Context, spec *agentv1.DeviceSpec, report driver.Reporter) error {
	id := spec.DeviceId
	l, err := d.loadLease(id)
	if err != nil {
		d.mu.Lock()
		ps, _ := d.attached(ctx)
		leases := d.leases()
		family := spec.GetTemplate().GetFamily()
		p, perr := pick(ps, leases, family, spec.GetParams()["serial"], d.cfg.IOS.ResetCommand != "")
		if perr != nil {
			d.mu.Unlock()
			return perr
		}
		l = &lease{DeviceID: id, Serial: p.Serial, Family: p.Family, Slot: d.freeSlot(leases), ADB: hasService(spec, "adb") && p.Family == "android"}
		err := d.saveLease(l)
		d.mu.Unlock()
		if err != nil {
			return err
		}
		report(agentv1.DeviceState_DEVICE_STATE_PROVISIONING, "Setting up phone "+p.Model+" ("+p.Serial+")")
		if err := d.setup(ctx, l); err != nil {
			return err
		}
	}
	stopped := spec.Power == agentv1.PowerState_POWER_STATE_STOPPED
	if stopped != l.Stopped {
		l.Stopped = stopped
		if err := d.saveLease(l); err != nil {
			return err
		}
		if stopped && l.Family == "android" {
			_, _ = d.run(ctx, d.cfg.ADB, "-s", l.Serial, "shell", "input", "keyevent", "KEYCODE_SLEEP")
		}
	}
	if stopped {
		d.closeForward(ctx, l)
		return nil
	}
	return d.openForward(ctx, l)
}

func (d *Driver) setup(ctx context.Context, l *lease) error {
	ctx, cancel := context.WithTimeout(ctx, d.cfg.CommandTimeout)
	defer cancel()
	switch l.Family {
	case "android":
		if l.ADB {
			if _, err := d.run(ctx, d.cfg.ADB, "-s", l.Serial, "tcpip", "5555"); err != nil {
				return err
			}
			_, _ = d.run(ctx, d.cfg.ADB, "-s", l.Serial, "wait-for-device")
		}
		if c := d.cfg.Android.SetupCommand; c != "" {
			if _, err := d.run(ctx, c, l.Serial); err != nil {
				return err
			}
		}
	case "ios":
		if c := d.cfg.IOS.SetupCommand; c != "" {
			if _, err := d.run(ctx, c, l.Serial); err != nil {
				return err
			}
		}
	}
	return nil
}

func (d *Driver) forwardPort(l *lease) int { return d.cfg.ForwardPortBase + l.Slot }

// openForward sets up `adb forward tcp:<port> tcp:5555` once per agent run.
func (d *Driver) openForward(ctx context.Context, l *lease) error {
	if !l.ADB {
		return nil
	}
	d.mu.Lock()
	active := d.forwards[l.DeviceID]
	d.mu.Unlock()
	if active {
		return nil
	}
	if _, err := d.run(ctx, d.cfg.ADB, "-s", l.Serial, "forward", "tcp:"+strconv.Itoa(d.forwardPort(l)), "tcp:5555"); err != nil {
		return err
	}
	d.mu.Lock()
	d.forwards[l.DeviceID] = true
	d.mu.Unlock()
	return nil
}

func (d *Driver) closeForward(ctx context.Context, l *lease) {
	d.mu.Lock()
	active := d.forwards[l.DeviceID]
	delete(d.forwards, l.DeviceID)
	d.mu.Unlock()
	if active {
		_, _ = d.run(ctx, d.cfg.ADB, "-s", l.Serial, "forward", "--remove", "tcp:"+strconv.Itoa(d.forwardPort(l)))
	}
}

// ServiceEndpoint implements driver.EndpointResolver: adb is reachable at the
// local end of the adb forward while the device is "on".
func (d *Driver) ServiceEndpoint(ctx context.Context, id, service string) (string, bool) {
	l, err := d.loadLease(id)
	if err != nil {
		return "", false
	}
	if service != "adb" || !l.ADB || l.Stopped {
		return "", true
	}
	if err := d.openForward(ctx, l); err != nil {
		return "", true
	}
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(d.forwardPort(l))), true
}

// Destroy implements driver.Driver: reset the phone and release the lease.
func (d *Driver) Destroy(ctx context.Context, id string) error {
	l, err := d.loadLease(id)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	d.closeForward(ctx, l)
	if err := d.reset(ctx, l); err != nil {
		return fmt.Errorf("reset %s: %w", l.Serial, err)
	}
	return os.Remove(filepath.Join(d.leaseDir(), id+".json"))
}

func (d *Driver) reset(ctx context.Context, l *lease) error {
	ctx, cancel := context.WithTimeout(ctx, d.cfg.CommandTimeout)
	defer cancel()
	adb := func(args ...string) (string, error) {
		return d.run(ctx, d.cfg.ADB, append([]string{"-s", l.Serial}, args...)...)
	}
	switch l.Family {
	case "ios":
		if d.cfg.IOS.ResetCommand == "" {
			return errors.New("ios.reset_command not configured")
		}
		_, err := d.run(ctx, d.cfg.IOS.ResetCommand, l.Serial)
		return err
	case "android":
		if c := d.cfg.Android.ResetCommand; c != "" {
			_, err := d.run(ctx, c, l.Serial)
			return err
		}
		if d.cfg.Android.FastbootWipe {
			if _, err := adb("reboot", "bootloader"); err != nil {
				return err
			}
			if _, err := d.run(ctx, d.cfg.Fastboot, "-s", l.Serial, "-w"); err != nil {
				return err
			}
			_, err := d.run(ctx, d.cfg.Fastboot, "-s", l.Serial, "reboot")
			return err
		}
		// Built-in cleanup: uninstall third-party apps, wipe user files, back to USB adb.
		out, err := adb("shell", "pm", "list", "packages", "-3")
		if err != nil {
			return err
		}
		keep := map[string]bool{}
		for _, k := range d.cfg.Android.KeepPackages {
			keep[k] = true
		}
		for _, line := range strings.Split(out, "\n") {
			pkg, ok := strings.CutPrefix(strings.TrimSpace(line), "package:")
			if !ok || pkg == "" || keep[pkg] {
				continue
			}
			_, _ = adb("uninstall", "--user", "0", pkg)
		}
		for _, dir := range []string{"Download", "DCIM", "Pictures", "Movies", "Music", "Documents"} {
			_, _ = adb("shell", "rm", "-rf", "/sdcard/"+dir+"/*")
		}
		_, _ = adb("usb")
		return nil
	}
	return nil
}

// List implements driver.Driver.
func (d *Driver) List(ctx context.Context) ([]*agentv1.DeviceStatus, error) {
	ps, _ := d.attached(ctx)
	var out []*agentv1.DeviceStatus
	for _, l := range d.leases() {
		out = append(out, d.statusOf(ctx, l, ps))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].DeviceId < out[j].DeviceId })
	return out, nil
}

// Status implements driver.Driver.
func (d *Driver) Status(ctx context.Context, id string) (*agentv1.DeviceStatus, error) {
	l, err := d.loadLease(id)
	if errors.Is(err, os.ErrNotExist) {
		return &agentv1.DeviceStatus{DeviceId: id, State: agentv1.DeviceState_DEVICE_STATE_DESTROYED, UpdatedUnix: time.Now().Unix()}, nil
	}
	if err != nil {
		return nil, err
	}
	ps, _ := d.attached(ctx)
	return d.statusOf(ctx, l, ps), nil
}

func (d *Driver) statusOf(ctx context.Context, l *lease, ps []phone) *agentv1.DeviceStatus {
	st := &agentv1.DeviceStatus{DeviceId: l.DeviceID, UpdatedUnix: time.Now().Unix()}
	attached := false
	for _, p := range ps {
		if p.Serial == l.Serial && p.State == "device" {
			attached = true
		}
	}
	switch {
	case !attached:
		st.State = agentv1.DeviceState_DEVICE_STATE_FAILED
		st.Message = "phone " + l.Serial + " is disconnected"
	case l.Stopped:
		st.State = agentv1.DeviceState_DEVICE_STATE_STOPPED
	default:
		st.State = agentv1.DeviceState_DEVICE_STATE_RUNNING
		if l.ADB {
			st.InternalIp = "127.0.0.1" // adb resolves via ServiceEndpoint
			_ = d.openForward(ctx, l)   // restore after agent restart
		}
		st.Message = l.Family + " phone " + l.Serial
	}
	return st
}

func hasService(spec *agentv1.DeviceSpec, svc string) bool {
	for _, s := range spec.GetServices() {
		if s.GetName() == svc {
			return true
		}
	}
	return false
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
