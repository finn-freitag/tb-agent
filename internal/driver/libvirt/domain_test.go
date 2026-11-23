package libvirt

import (
	"context"
	"path/filepath"

	"encoding/xml"
	golibvirt "github.com/digitalocean/go-libvirt"
	"github.com/finn-freitag/tb-agent/internal/driver"
	"strings"
	"testing"
)

func TestRenderDomainPortIsolation(t *testing.T) {
	p := domainParams{Name: "tb-d1", DeviceID: "d1", MemoryMB: 1024, VCPUs: 1, Machine: "q35", CPUMode: "host-passthrough",
		DiskPath: "/x.qcow2", DiskBus: "virtio", NetModel: "virtio", Video: "virtio", Network: "default", VNCListen: "127.0.0.1"}
	for _, iso := range []bool{true, false} {
		p.IsolatePorts = iso
		out, err := renderDomain(p)
		if err != nil {
			t.Fatal(err)
		}
		if err := xml.Unmarshal([]byte(out), new(struct{})); err != nil {
			t.Fatalf("invalid XML: %v\n%s", err, out)
		}
		if got := strings.Contains(out, "<port isolated='yes'/>"); got != iso {
			t.Errorf("isolate=%v: port element present=%v", iso, got)
		}
	}
}

func TestRenderDomainGPUAndEFI(t *testing.T) {
	p := domainParams{Name: "tb-d1", DeviceID: "d1", MemoryMB: 2048, VCPUs: 2, Machine: "q35", CPUMode: "host-passthrough",
		DiskPath: "/x.qcow2", DiskBus: "sata", NetModel: "e1000e", Video: "virtio", Network: "tossbox-offline", VNCListen: "127.0.0.1",
		EFI: true, VirGL: true, RenderNode: "/dev/dri/renderD128"}
	out, err := renderDomain(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := xml.Unmarshal([]byte(out), new(struct{})); err != nil {
		t.Fatalf("invalid XML: %v\n%s", err, out)
	}
	for _, want := range []string{"<os firmware='efi'>", "name='secure-boot'", "<graphics type='egl-headless'>", "rendernode='/dev/dri/renderD128'",
		"accel3d='yes'", "<source network='tossbox-offline'/>", "<model type='e1000e'/>"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(out, "device='cdrom'") {
		t.Error("no seed ISO expected")
	}

	p.VirGL, p.HostdevPCI, p.EFI = false, "0000:00:02.3", false
	out, _ = renderDomain(p)
	if !strings.Contains(out, "<address domain='0x0000' bus='0x00' slot='0x02' function='0x3'/>") || strings.Contains(out, "egl-headless") {
		t.Errorf("hostdev not rendered:\n%s", out)
	}
}

func TestOfflineNetworkXML(t *testing.T) {
	out, err := offlineNetworkXML("tossbox-offline", "192.168.251.0/24")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "<forward") || !strings.Contains(out, "<dns enable='no'/>") || !strings.Contains(out, "range start='192.168.251.2' end='192.168.251.254'") {
		t.Fatalf("bad network:\n%s", out)
	}
	if _, err := offlineNetworkXML("x", "10.0.0.0/16"); err == nil {
		t.Fatal("non-/24 subnet accepted")
	}
}

func TestIsNoNetwork(t *testing.T) {
	if !isNoNetwork(golibvirt.Error{Code: uint32(golibvirt.ErrNoNetwork)}) || isNoNetwork(golibvirt.Error{Code: uint32(golibvirt.ErrNoDomain)}) {
		t.Fatal("isNoNetwork")
	}
}

type fakeGPU struct {
	node string
	vfs  []string
}

func (f fakeGPU) RenderNode() string             { return f.node }
func (f fakeGPU) VFs() []string                  { return f.vfs }
func (f fakeGPU) Acquire(string) (string, error) { return "", nil }
func (f fakeGPU) Release(string)                 {}

func TestCapabilitiesGPU(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "none.sock") // no libvirt: unhealthy, GPU fields still reported
	cases := []struct {
		gpu   string
		env   fakeGPU
		mode  string
		slots int32
		note  string
	}{
		{"sriov", fakeGPU{vfs: []string{"0000:00:02.1", "0000:00:02.2"}}, "sriov", 2, ""},
		{"sriov", fakeGPU{}, "", 0, "no SR-IOV virtual functions"},
		{"virgl", fakeGPU{node: "/dev/dri/renderD128"}, "virgl", -1, ""},
		{"virgl", fakeGPU{}, "", 0, "no render node"},
		{"", fakeGPU{node: "/dev/dri/renderD128"}, "", 0, ""},
	}
	for _, c := range cases {
		d, err := New(driver.Env{GPU: c.env}, Config{Socket: sock, GPU: c.gpu})
		if err != nil {
			t.Fatal(err)
		}
		caps := d.Capabilities(context.Background())
		if caps.GpuMode != c.mode || caps.GpuSlots != c.slots || (c.note != "") != strings.Contains(caps.Message, "gpu:") || !strings.Contains(caps.Message, c.note) {
			t.Errorf("%s %+v: mode=%q slots=%d msg=%q", c.gpu, c.env, caps.GpuMode, caps.GpuSlots, caps.Message)
		}
	}
	if _, err := New(driver.Env{}, Config{GPU: "cuda"}); err == nil {
		t.Error("unknown gpu mode accepted")
	}
}
