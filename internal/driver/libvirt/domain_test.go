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
