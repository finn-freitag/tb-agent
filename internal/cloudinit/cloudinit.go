// Package cloudinit builds NoCloud "cidata" seed ISOs for the cloud-init
// (Linux) and cloudbase-init (Windows) guest profiles.
package cloudinit

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/kdomanski/iso9660"
	"gopkg.in/yaml.v3"
)

// Params are the per-device provisioning inputs.
type Params struct {
	InstanceID string // device id
	Hostname   string
	User       string // default user (template default_user)
	Password   string
	SSHKeys    []string
	ProductKey string // Windows only, may be empty
	// Packages are extra packages for Linux guests (best effort).
	Packages []string
	// NoGuestAgent skips qemu-guest-agent (containers have no virtio port;
	// enabling it there only blocks cloud-init until systemd times out).
	NoGuestAgent bool
	// Script is an optional shell script run as root once, after packages are
	// installed (template param cloudinit.script). Output goes to
	// /var/log/tossbox-setup.log.
	Script string
	// GPUForceProbe is the PCI device ID (e.g. "4692") of a passed-through
	// Intel GPU VF that the guest's xe driver only binds with force_probe
	// (platforms not officially supported by xe). Empty = nothing to do.
	GPUForceProbe string
}

// TemplateExtras reads the generic cloud-init template params:
// cloudinit.packages (space or comma separated) and cloudinit.script.
func TemplateExtras(params map[string]string) (packages []string, script string) {
	return strings.Fields(strings.ReplaceAll(params["cloudinit.packages"], ",", " ")), params["cloudinit.script"]
}

// WithTemplate returns p with the template's generic cloud-init extras added.
func (p Params) WithTemplate(params map[string]string) Params {
	pkgs, script := TemplateExtras(params)
	p.Packages = append(append([]string(nil), p.Packages...), pkgs...)
	if script != "" {
		p.Script = script
	}
	return p
}

// Seed holds the NoCloud files.
type Seed struct {
	MetaData []byte
	UserData []byte
}

// ForProfile builds the seed for a guest profile ("cloud-init" or "cloudbase-init").
func ForProfile(profile string, p Params) (Seed, error) {
	if p.User == "" {
		p.User = "tossbox"
	}
	switch profile {
	case "cloud-init", "":
		return Linux(p)
	case "cloudbase-init":
		return Windows(p), nil
	default:
		return Seed{}, fmt.Errorf("cloudinit: unsupported guest profile %q", profile)
	}
}

func metaData(p Params) []byte {
	b, _ := yaml.Marshal(map[string]string{"instance-id": p.InstanceID, "local-hostname": p.Hostname})
	return b
}

// Linux builds a cloud-init seed: hostname, default user with password and
// sudo, SSH keys, password SSH login and the QEMU guest agent.
func Linux(p Params) (Seed, error) {
	user := map[string]any{
		"name":        p.User,
		"sudo":        "ALL=(ALL) NOPASSWD:ALL",
		"shell":       "/bin/bash",
		"lock_passwd": false,
	}
	if len(p.SSHKeys) > 0 {
		user["ssh_authorized_keys"] = p.SSHKeys
	}
	var pkgs []string
	runcmd := []any{
		// Some images ship sshd drop-ins that disable password logins.
		[]string{"sh", "-c", "rm -f /etc/ssh/sshd_config.d/*-cloudimg-settings.conf; mkdir -p /etc/ssh/sshd_config.d; printf 'PasswordAuthentication yes\\n' > /etc/ssh/sshd_config.d/00-tossbox.conf; for s in ssh sshd; do systemctl enable $s 2>/dev/null; systemctl restart $s 2>/dev/null; rc-update add $s 2>/dev/null; rc-service $s restart 2>/dev/null; done; true"},
	}
	if !p.NoGuestAgent {
		pkgs = append(pkgs, "qemu-guest-agent")
		runcmd = append(runcmd, []string{"sh", "-c", "systemctl enable --now --no-block qemu-guest-agent 2>/dev/null || true"})
	}
	pkgs = append(pkgs, p.Packages...)
	var files []any
	if id := p.GPUForceProbe; id != "" {
		files = append(files, map[string]any{"path": "/etc/modprobe.d/tossbox-gpu.conf", "permissions": "0644",
			"content": "# Tossbox: SR-IOV GPU VF (xe driver, force probe)\noptions xe force_probe=" + id + "\noptions i915 force_probe=!" + id + "\n"})
		// The driver already gave up during this boot: bind it now. The
		// modprobe.d options apply on later boots (initramfs is updated too).
		runcmd = append(runcmd, []string{"sh", "-c", "modprobe -r xe 2>/dev/null; modprobe xe; command -v update-initramfs >/dev/null && update-initramfs -u >/dev/null 2>&1; true"})
	}
	if p.Script != "" {
		files = append(files, map[string]any{"path": "/var/lib/tossbox/setup.sh", "permissions": "0755", "content": p.Script})
		runcmd = append(runcmd, []string{"sh", "-c", "bash /var/lib/tossbox/setup.sh > /var/log/tossbox-setup.log 2>&1"})
	}
	cfg := map[string]any{
		"hostname":         p.Hostname,
		"manage_etc_hosts": true,
		"users":            []any{"default", user},
		"chpasswd": map[string]any{
			"expire": false,
			"users":  []any{map[string]any{"name": p.User, "password": p.Password, "type": "text"}},
		},
		"ssh_pwauth": true,
		"packages":   pkgs,
		"runcmd":     runcmd,
	}
	if len(files) > 0 {
		cfg["write_files"] = files
	}
	body, err := yaml.Marshal(cfg)
	if err != nil {
		return Seed{}, err
	}
	return Seed{MetaData: metaData(p), UserData: append([]byte("#cloud-config\n"), body...)}, nil
}

// netbios shortens a hostname to the 15 characters Windows allows.
func netbios(h string) string {
	h = strings.TrimRight(h, "-")
	if len(h) > 15 {
		h = strings.TrimRight(h[:15], "-")
	}
	return h
}

// psQuote quotes a string for PowerShell single-quoted literals.
func psQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// Windows builds a cloudbase-init seed with a PowerShell user-data script.
func Windows(p Params) Seed {
	var b strings.Builder
	b.WriteString("#ps1_sysnative\n")
	b.WriteString("$ErrorActionPreference = 'Continue'\n")
	fmt.Fprintf(&b, "$user = %s\n$pw = %s\n$hostname = %s\n", psQuote(p.User), psQuote(p.Password), psQuote(netbios(p.Hostname)))
	b.WriteString(`$sec = ConvertTo-SecureString $pw -AsPlainText -Force
if (Get-LocalUser -Name $user -ErrorAction SilentlyContinue) {
  Set-LocalUser -Name $user -Password $sec -PasswordNeverExpires $true
  Enable-LocalUser -Name $user
} else {
  New-LocalUser -Name $user -Password $sec -PasswordNeverExpires -AccountNeverExpires | Out-Null
}
# Administrators and Remote Desktop Users (SIDs are language independent)
Add-LocalGroupMember -SID 'S-1-5-32-544' -Member $user -ErrorAction SilentlyContinue
Add-LocalGroupMember -SID 'S-1-5-32-555' -Member $user -ErrorAction SilentlyContinue

# Hostname (cloudbase-init also sets it from meta-data; it reboots when needed)
if ($hostname -and $env:COMPUTERNAME -ne $hostname.ToUpper()) {
  Rename-Computer -NewName $hostname -Force -ErrorAction SilentlyContinue
}

# RDP with Network Level Authentication
Set-ItemProperty -Path 'HKLM:\System\CurrentControlSet\Control\Terminal Server' -Name fDenyTSConnections -Value 0
Set-ItemProperty -Path 'HKLM:\System\CurrentControlSet\Control\Terminal Server\WinStations\RDP-Tcp' -Name UserAuthentication -Value 1
Enable-NetFirewallRule -Group '@FirewallAPI.dll,-28752' -ErrorAction SilentlyContinue
if (-not (Get-NetFirewallRule -Name 'tossbox-rdp' -ErrorAction SilentlyContinue)) {
  New-NetFirewallRule -Name 'tossbox-rdp' -DisplayName 'Tossbox RDP' -Direction Inbound -Protocol TCP -LocalPort 3389 -Action Allow | Out-Null
}

# OpenSSH server
if (-not (Get-Service sshd -ErrorAction SilentlyContinue)) {
  Add-WindowsCapability -Online -Name OpenSSH.Server~~~~0.0.1.0 | Out-Null
}
Set-Service -Name sshd -StartupType Automatic -ErrorAction SilentlyContinue
Start-Service sshd -ErrorAction SilentlyContinue
if (-not (Get-NetFirewallRule -Name 'tossbox-sshd' -ErrorAction SilentlyContinue)) {
  New-NetFirewallRule -Name 'tossbox-sshd' -DisplayName 'Tossbox OpenSSH' -Direction Inbound -Protocol TCP -LocalPort 22 -Action Allow | Out-Null
}
New-ItemProperty -Path 'HKLM:\SOFTWARE\OpenSSH' -Name DefaultShell -Value 'C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe' -PropertyType String -Force | Out-Null
`)
	if len(p.SSHKeys) > 0 {
		quoted := make([]string, len(p.SSHKeys))
		for i, k := range p.SSHKeys {
			quoted[i] = psQuote(k)
		}
		fmt.Fprintf(&b, "$keys = @(%s)\n", strings.Join(quoted, ", "))
		b.WriteString(`$akf = 'C:\ProgramData\ssh\administrators_authorized_keys'
New-Item -ItemType Directory -Force -Path 'C:\ProgramData\ssh' | Out-Null
Set-Content -Path $akf -Value $keys -Encoding ascii
icacls.exe $akf /inheritance:r /grant 'Administrators:F' /grant 'SYSTEM:F' | Out-Null
`)
	}
	if p.ProductKey != "" {
		fmt.Fprintf(&b, "$key = %s\n", psQuote(p.ProductKey))
		b.WriteString(`cscript.exe //B //Nologo "$env:SystemRoot\System32\slmgr.vbs" /ipk $key
cscript.exe //B //Nologo "$env:SystemRoot\System32\slmgr.vbs" /ato
`)
	}
	return Seed{MetaData: metaData(p), UserData: []byte(b.String())}
}

// ISO renders the seed as an ISO 9660 image with volume label "cidata".
func (s Seed) ISO() ([]byte, error) {
	w, err := iso9660.NewWriter()
	if err != nil {
		return nil, err
	}
	defer w.Cleanup()
	if err := w.AddFile(bytes.NewReader(s.MetaData), "meta-data"); err != nil {
		return nil, err
	}
	if err := w.AddFile(bytes.NewReader(s.UserData), "user-data"); err != nil {
		return nil, err
	}
	var out bytes.Buffer
	if err := w.WriteTo(&out, "cidata"); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}
