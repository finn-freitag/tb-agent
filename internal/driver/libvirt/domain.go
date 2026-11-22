package libvirt

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"strings"
	"text/template"
)

// metadataNS identifies Tossbox-owned domains.
const metadataNS = "https://github.com/finn-freitag/tossbox/xmlns/device/v1"

type domainParams struct {
	Name, Title, DeviceID, Hostname, TemplateID, Family string
	MemoryMB                                            int64
	VCPUs                                               int32
	GlobalQuota                                         int64 // µs per 100ms period, 0 = no cap
	Machine, CPUMode                                    string
	DiskPath, ISOPath                                   string
	DiskBus, NetModel, Video                            string
	Network, Bridge                                     string
	VNCListen, VNCPassword                              string
	Windows                                             bool
	SecureBoot, TPM                                     bool
	Loader, NVRAMTemplate                               string
}

func x(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return strings.ReplaceAll(b.String(), "'", "&apos;")
}

var domainTmpl = template.Must(template.New("domain").Funcs(template.FuncMap{"x": x}).Parse(`<domain type='kvm'>
  <name>{{x .Name}}</name>
  <title>{{x .Title}}</title>
  <description>Tossbox device {{x .DeviceID}} ({{x .TemplateID}})</description>
  <metadata>
    <tossbox:device xmlns:tossbox='` + metadataNS + `' id='{{x .DeviceID}}' hostname='{{x .Hostname}}' template='{{x .TemplateID}}' family='{{x .Family}}'/>
  </metadata>
  <memory unit='MiB'>{{.MemoryMB}}</memory>
  <currentMemory unit='MiB'>{{.MemoryMB}}</currentMemory>
  <vcpu placement='static'>{{.VCPUs}}</vcpu>
{{- if gt .GlobalQuota 0}}
  <cputune>
    <global_period>100000</global_period>
    <global_quota>{{.GlobalQuota}}</global_quota>
  </cputune>
{{- end}}
{{- if .Windows}}
{{- if .Loader}}
  <os>
    <type arch='x86_64' machine='{{x .Machine}}'>hvm</type>
    <loader readonly='yes' secure='{{if .SecureBoot}}yes{{else}}no{{end}}' type='pflash'>{{x .Loader}}</loader>
{{- if .NVRAMTemplate}}
    <nvram template='{{x .NVRAMTemplate}}'/>
{{- end}}
    <boot dev='hd'/>
  </os>
{{- else}}
  <os firmware='efi'>
    <type arch='x86_64' machine='{{x .Machine}}'>hvm</type>
    <firmware>
      <feature enabled='{{if .SecureBoot}}yes{{else}}no{{end}}' name='secure-boot'/>
{{- if .SecureBoot}}
      <feature enabled='yes' name='enrolled-keys'/>
{{- end}}
    </firmware>
{{- if .SecureBoot}}
    <loader secure='yes'/>
{{- end}}
    <boot dev='hd'/>
  </os>
{{- end}}
  <features>
    <acpi/>
    <apic/>
    <hyperv mode='custom'>
      <relaxed state='on'/>
      <vapic state='on'/>
      <spinlocks state='on' retries='8191'/>
      <vpindex state='on'/>
      <synic state='on'/>
      <stimer state='on'/>
    </hyperv>
{{- if .SecureBoot}}
    <smm state='on'/>
{{- end}}
  </features>
  <clock offset='localtime'>
    <timer name='rtc' tickpolicy='catchup'/>
    <timer name='pit' tickpolicy='delay'/>
    <timer name='hpet' present='no'/>
    <timer name='hypervclock' present='yes'/>
  </clock>
{{- else}}
  <os>
    <type arch='x86_64' machine='{{x .Machine}}'>hvm</type>
    <boot dev='hd'/>
  </os>
  <features>
    <acpi/>
    <apic/>
  </features>
  <clock offset='utc'/>
{{- end}}
  <cpu mode='{{x .CPUMode}}'/>
  <on_poweroff>destroy</on_poweroff>
  <on_reboot>restart</on_reboot>
  <on_crash>destroy</on_crash>
  <devices>
    <disk type='file' device='disk'>
      <driver name='qemu' type='qcow2' discard='unmap'/>
      <source file='{{x .DiskPath}}'/>
      <target dev='{{if eq .DiskBus "virtio"}}vda{{else}}sda{{end}}' bus='{{x .DiskBus}}'/>
    </disk>
    <disk type='file' device='cdrom'>
      <driver name='qemu' type='raw'/>
      <source file='{{x .ISOPath}}'/>
      <target dev='sdz' bus='sata'/>
      <readonly/>
    </disk>
{{- if .Bridge}}
    <interface type='bridge'>
      <source bridge='{{x .Bridge}}'/>
      <model type='{{x .NetModel}}'/>
    </interface>
{{- else}}
    <interface type='network'>
      <source network='{{x .Network}}'/>
      <model type='{{x .NetModel}}'/>
    </interface>
{{- end}}
    <serial type='pty'>
      <target port='0'/>
    </serial>
    <console type='pty'/>
    <channel type='unix'>
      <target type='virtio' name='org.qemu.guest_agent.0'/>
    </channel>
    <input type='tablet' bus='usb'/>
    <graphics type='vnc' autoport='yes' listen='{{x .VNCListen}}' passwd='{{x .VNCPassword}}'>
      <listen type='address' address='{{x .VNCListen}}'/>
    </graphics>
    <video>
      <model type='{{x .Video}}'/>
    </video>
{{- if .TPM}}
    <tpm model='tpm-crb'>
      <backend type='emulator' version='2.0'/>
    </tpm>
{{- end}}
    <rng model='virtio'>
      <backend model='random'>/dev/urandom</backend>
    </rng>
    <memballoon model='virtio'/>
  </devices>
</domain>
`))

func renderDomain(p domainParams) (string, error) {
	var b bytes.Buffer
	if err := domainTmpl.Execute(&b, p); err != nil {
		return "", err
	}
	return b.String(), nil
}

// liveDomain is the subset of the domain XML we read back.
type liveDomain struct {
	Metadata struct {
		Device *struct {
			ID       string `xml:"id,attr"`
			Hostname string `xml:"hostname,attr"`
			Template string `xml:"template,attr"`
			Family   string `xml:"family,attr"`
		} `xml:"https://github.com/finn-freitag/tossbox/xmlns/device/v1 device"`
	} `xml:"metadata"`
	Devices struct {
		Graphics []struct {
			Type string `xml:"type,attr"`
			Port int    `xml:"port,attr"`
		} `xml:"graphics"`
		Interfaces []struct {
			MAC struct {
				Address string `xml:"address,attr"`
			} `xml:"mac"`
		} `xml:"interface"`
	} `xml:"devices"`
}

func parseDomain(s string) (*liveDomain, error) {
	var d liveDomain
	if err := xml.Unmarshal([]byte(s), &d); err != nil {
		return nil, fmt.Errorf("libvirt: parse domain xml: %w", err)
	}
	return &d, nil
}

func volumeXML(name string, capacity uint64, format, backingPath, backingFormat string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "<volume><name>%s</name><capacity unit='bytes'>%d</capacity>", x(name), capacity)
	fmt.Fprintf(&b, "<target><format type='%s'/></target>", x(format))
	if backingPath != "" {
		fmt.Fprintf(&b, "<backingStore><path>%s</path><format type='%s'/></backingStore>", x(backingPath), x(backingFormat))
	}
	b.WriteString("</volume>")
	return b.String()
}
