package usbphone

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/finn-freitag/tb-agent/internal/driver"
	agentv1 "github.com/finn-freitag/tb-proto/gen/agent/v1"
)

const adbOut = `List of devices attached
* daemon started successfully
R58M123ABC             device usb:1-1 product:panther model:Pixel_7 device:panther transport_id:1
UNAUTH01               unauthorized usb:1-2 transport_id:2
192.168.1.5:5555       device product:x model:Net transport_id:3
`

func TestParseADB(t *testing.T) {
	ps := parseADBDevices(adbOut)
	if len(ps) != 2 || ps[0].Serial != "R58M123ABC" || ps[0].Model != "Pixel 7" || ps[1].State != "unauthorized" {
		t.Fatalf("%+v", ps)
	}
}

func TestPick(t *testing.T) {
	ps := []phone{{Serial: "A", Family: "android", State: "device"}, {Serial: "B", Family: "android", State: "unauthorized"}, {Serial: "I", Family: "ios", State: "device"}}
	if p, err := pick(ps, nil, "android", "", false); err != nil || p.Serial != "A" {
		t.Fatal(p, err)
	}
	if _, err := pick(ps, []*lease{{Serial: "A"}}, "android", "", false); err == nil {
		t.Error("A is leased")
	}
	if _, err := pick(ps, nil, "android", "B", false); err == nil || !strings.Contains(err.Error(), "unauthorized") {
		t.Error(err)
	}
	if _, err := pick(ps, nil, "ios", "", false); err == nil {
		t.Error("ios needs reset command")
	}
	if p, err := pick(ps, nil, "ios", "", true); err != nil || p.Serial != "I" {
		t.Error(p, err)
	}
}

type fakeRunner struct {
	mu    sync.Mutex
	calls []string
}

func (f *fakeRunner) run(ctx context.Context, bin string, args ...string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cmd := bin + " " + strings.Join(args, " ")
	f.calls = append(f.calls, cmd)
	switch {
	case cmd == "adb devices -l":
		return adbOut, nil
	case bin == "idevice_id":
		return "", errors.New(`exec: "idevice_id": executable file not found in $PATH`)
	case strings.HasSuffix(cmd, "pm list packages -3"):
		return "package:com.example.app\npackage:com.keep.me\n", nil
	}
	return "", nil
}

func (f *fakeRunner) has(sub string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if strings.Contains(c, sub) {
			return true
		}
	}
	return false
}

func TestLifecycle(t *testing.T) {
	fr := &fakeRunner{}
	d := New(driver.Env{DataDir: t.TempDir()}, Config{ForwardPortBase: 39100, Android: AndroidFlow{KeepPackages: []string{"com.keep.me"}}})
	d.run = fr.run
	ctx := context.Background()

	caps := d.Capabilities(ctx)
	if caps.FixedSlots != 2 || len(caps.PhysicalDevices) != 2 || !caps.PhysicalDevices[0].Available || caps.Families[0] != "android" || !caps.Healthy {
		t.Fatalf("%+v", caps)
	}
	spec := &agentv1.DeviceSpec{DeviceId: "p1", Power: agentv1.PowerState_POWER_STATE_RUNNING,
		Template: &agentv1.TemplateSpec{Family: "android", GuestProfile: "phone"},
		Services: []*agentv1.ServiceSpec{{Name: "adb", InternalPort: 5555}}}
	if err := d.Ensure(ctx, spec, func(agentv1.DeviceState, string) {}); err != nil {
		t.Fatal(err)
	}
	if !fr.has("adb -s R58M123ABC tcpip 5555") || !fr.has("forward tcp:39101 tcp:5555") {
		t.Fatal(fr.calls)
	}
	st, _ := d.Status(ctx, "p1")
	if st.State != agentv1.DeviceState_DEVICE_STATE_RUNNING {
		t.Fatal(st)
	}
	if ep, ok := d.ServiceEndpoint(ctx, "p1", "adb"); !ok || ep != "127.0.0.1:39101" {
		t.Fatal("adb endpoint", ep, ok)
	}
	if _, ok := d.ServiceEndpoint(ctx, "nope", "adb"); ok {
		t.Fatal("unknown device handled")
	}
	if c := d.Capabilities(ctx); c.PhysicalDevices[0].Available {
		t.Error("leased phone reported available")
	}
	spec2 := &agentv1.DeviceSpec{DeviceId: "p2", Template: &agentv1.TemplateSpec{Family: "android"}}
	if err := d.Ensure(ctx, spec2, func(agentv1.DeviceState, string) {}); err == nil {
		t.Error("no second free phone expected")
	}
	spec.Power = agentv1.PowerState_POWER_STATE_STOPPED
	d.Ensure(ctx, spec, func(agentv1.DeviceState, string) {})
	if st, _ := d.Status(ctx, "p1"); st.State != agentv1.DeviceState_DEVICE_STATE_STOPPED {
		t.Fatal(st)
	}
	if ep, ok := d.ServiceEndpoint(ctx, "p1", "adb"); !ok || ep != "" {
		t.Fatal("stopped phone must be unreachable", ep, ok)
	}
	if !fr.has("forward --remove tcp:39101") {
		t.Fatal("forward not removed on stop")
	}
	if l, _ := d.List(ctx); len(l) != 1 {
		t.Fatal(l)
	}
	if err := d.Destroy(ctx, "p1"); err != nil {
		t.Fatal(err)
	}
	if !fr.has("uninstall --user 0 com.example.app") || fr.has("uninstall --user 0 com.keep.me") {
		t.Fatal(fr.calls)
	}
	if st, _ := d.Status(ctx, "p1"); st.State != agentv1.DeviceState_DEVICE_STATE_DESTROYED {
		t.Fatal(st)
	}
	if err := d.Destroy(ctx, "p1"); err != nil {
		t.Fatal(err)
	}
}
