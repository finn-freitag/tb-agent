package cloudinit

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/kdomanski/iso9660"
	"gopkg.in/yaml.v3"
)

func TestLinuxSeedISO(t *testing.T) {
	s, err := ForProfile("cloud-init", Params{InstanceID: "id1", Hostname: "box", Password: "pw-1", SSHKeys: []string{"ssh-ed25519 AAA x"}})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(s.UserData, []byte("#cloud-config\n")) {
		t.Fatal("missing header")
	}
	var cfg map[string]any
	if err := yaml.Unmarshal(s.UserData, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg["hostname"] != "box" || cfg["ssh_pwauth"] != true {
		t.Fatalf("cfg %v", cfg)
	}
	iso, err := s.ISO()
	if err != nil {
		t.Fatal(err)
	}
	img, err := iso9660.OpenImage(bytes.NewReader(iso))
	if err != nil {
		t.Fatal(err)
	}
	label, _ := img.Label()
	if !strings.EqualFold(label, "cidata") {
		t.Fatalf("label %q", label)
	}
	root, _ := img.RootDir()
	kids, _ := root.GetChildren()
	names := map[string]string{}
	for _, k := range kids {
		b, _ := io.ReadAll(k.Reader())
		names[k.Name()] = string(b)
	}
	if !strings.Contains(names["user-data"], "#cloud-config") || !strings.Contains(names["meta-data"], "instance-id: id1") {
		t.Fatalf("files %v", names)
	}
}

func TestWindowsSeedQuoting(t *testing.T) {
	s, _ := ForProfile("cloudbase-init", Params{InstanceID: "i", Hostname: "win", Password: "a'b", ProductKey: "AAAAA-BBBBB", SSHKeys: []string{"k"}})
	u := string(s.UserData)
	for _, want := range []string{"#ps1_sysnative", "$pw = 'a''b'", "/ipk $key", "administrators_authorized_keys", "UserAuthentication -Value 1"} {
		if !strings.Contains(u, want) {
			t.Errorf("missing %q", want)
		}
	}
	if _, err := ForProfile("tart", Params{}); err == nil {
		t.Error("expected unsupported profile error")
	}
}

func TestLinuxTemplateExtras(t *testing.T) {
	p := Params{InstanceID: "x", Hostname: "h", User: "u", Password: "p", Packages: []string{"openssh-server"}}.
		WithTemplate(map[string]string{"cloudinit.packages": "xrdp, task-gnome-desktop", "cloudinit.script": "echo hi\n"})
	s, err := Linux(p)
	if err != nil {
		t.Fatal(err)
	}
	ud := string(s.UserData)
	for _, want := range []string{"- openssh-server", "- xrdp", "- task-gnome-desktop", "/var/lib/tossbox/setup.sh", "echo hi", "tossbox-setup.log"} {
		if !strings.Contains(ud, want) {
			t.Errorf("user-data lacks %q:\n%s", want, ud)
		}
	}
}

func TestLinuxGPUForceProbe(t *testing.T) {
	seed, err := Linux(Params{InstanceID: "d1", Hostname: "h", User: "u", Password: "p", GPUForceProbe: "4692"})
	if err != nil {
		t.Fatal(err)
	}
	ud := string(seed.UserData)
	for _, want := range []string{"/etc/modprobe.d/tossbox-gpu.conf", "options xe force_probe=4692", "options i915 force_probe=!4692", "modprobe xe"} {
		if !strings.Contains(ud, want) {
			t.Errorf("missing %q in\n%s", want, ud)
		}
	}
	if seed, _ := Linux(Params{InstanceID: "d1", Hostname: "h", User: "u", Password: "p"}); strings.Contains(string(seed.UserData), "force_probe") {
		t.Error("force_probe without GPU")
	}
}
