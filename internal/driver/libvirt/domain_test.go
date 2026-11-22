package libvirt

import (
	"encoding/xml"
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
