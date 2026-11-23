// Package libvirt implements the "libvirt" driver: KVM virtual machines
// managed through the host's libvirt daemon (Linux and Windows guests).
//
// Storage: all disks live in a libvirt storage pool and are created through
// the libvirt API (images are uploaded into the pool once and used as qcow2
// backing files for per-device linked clones). The agent therefore does not
// need filesystem access to the pool, which keeps it container friendly.
//
// Pool volumes:
//
//	tb-base-<hash>.img        uploaded base image (shared, cached)
//	tb-<device>.qcow2         per-device overlay disk
//	tb-<device>-cidata.iso    NoCloud seed (cloud-init / cloudbase-init)
package libvirt

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	golibvirt "github.com/digitalocean/go-libvirt"
	"github.com/digitalocean/go-libvirt/socket/dialers"
	agentv1 "github.com/finn-freitag/tb-proto/gen/agent/v1"
	"gopkg.in/yaml.v3"

	"github.com/finn-freitag/tb-agent/internal/cloudinit"
	"github.com/finn-freitag/tb-agent/internal/driver"
	"github.com/finn-freitag/tb-agent/internal/hostinfo"
)

// Config is the drivers.libvirt section of node.yaml.
type Config struct {
	// URI: qemu:///system (default) or qemu:///session.
	URI string `yaml:"uri"`
	// Socket is the libvirt unix socket (default /var/run/libvirt/libvirt-sock).
	Socket string `yaml:"socket"`
	// Network is the libvirt network devices attach to (default "default").
	Network string `yaml:"network"`
	// Bridge attaches devices to a host bridge instead of a libvirt network.
	// The agent must then be able to discover IPs via the guest agent.
	Bridge string `yaml:"bridge"`
	// IsolatePorts marks each device's bridge port isolated, so devices on
	// the same network can't reach each other (only the host/uplink).
	// Default true.
	IsolatePorts *bool `yaml:"isolate_ports"`
	// OfflineNetwork is the libvirt network for devices with the "offline"
	// policy: no forwarding, no DNS, only DHCP; the agent defines it if
	// missing (default "tossbox-offline", bridge tb-virt-off).
	OfflineNetwork string `yaml:"offline_network"`
	// OfflineSubnet is its IPv4 /24 (default 192.168.251.0/24).
	OfflineSubnet string `yaml:"offline_subnet"`
	// GPU: how devices that request a GPU get one: "" (not offered),
	// "virgl" (shared paravirtual 3D via the node's render node; Linux and
	// ChromeOS guests) or "sriov" (one SR-IOV virtual function per device,
	// see the node's gpu.sriov section).
	GPU string `yaml:"gpu"`
	// StoragePool holds all device disks (default "default").
	StoragePool string `yaml:"storage_pool"`
	// Families this node offers (default [linux, windows, chromeos]).
	Families []string `yaml:"families"`
	Machine  string   `yaml:"machine"`  // default q35
	CPUMode  string   `yaml:"cpu_mode"` // default host-passthrough
	// VNCListen is where the VNC console of each VM listens on the host
	// (default 127.0.0.1); the agent forwards the vnc service there.
	VNCListen string        `yaml:"vnc_listen"`
	Windows   WindowsConfig `yaml:"windows"`
}

// WindowsConfig tunes Windows guests.
type WindowsConfig struct {
	// Loader / NVRAMTemplate: explicit OVMF paths. Empty = libvirt firmware
	// auto-selection (<os firmware='efi'>).
	Loader        string `yaml:"loader"`
	NVRAMTemplate string `yaml:"nvram_template"`
	SecureBoot    *bool  `yaml:"secure_boot"` // default true
	TPM           *bool  `yaml:"tpm"`         // default true (needs swtpm on the host)
	DiskBus       string `yaml:"disk_bus"`    // default virtio (golden image must contain VirtIO drivers)
	NetModel      string `yaml:"net_model"`   // default virtio
	Video         string `yaml:"video"`       // default vga
}

func boolOr(p *bool, def bool) bool {
	if p == nil {
		return def
	}
	return *p
}

func init() {
	driver.Register("libvirt", func(env driver.Env, cfg *yaml.Node) (driver.Driver, error) {
		var c Config
		if err := driver.DecodeConfig(cfg, &c); err != nil {
			return nil, fmt.Errorf("libvirt: config: %w", err)
		}
		return New(env, c)
	})
}

// Driver is the libvirt driver.
type Driver struct {
	env driver.Env
	cfg Config
	log *slog.Logger

	mu sync.Mutex
	lv *golibvirt.Libvirt

	baseMu sync.Mutex // serializes base image uploads
}

// New creates the driver (connects lazily).
func New(env driver.Env, c Config) (*Driver, error) {
	if c.URI == "" {
		c.URI = string(golibvirt.QEMUSystem)
	}
	if c.Socket == "" {
		c.Socket = "/var/run/libvirt/libvirt-sock"
	}
	if c.Network == "" {
		c.Network = "default"
	}
	if c.StoragePool == "" {
		c.StoragePool = "default"
	}
	if len(c.Families) == 0 {
		c.Families = []string{"linux", "windows", "chromeos"}
	}
	if c.Machine == "" {
		c.Machine = "q35"
	}
	if c.CPUMode == "" {
		c.CPUMode = "host-passthrough"
	}
	if c.VNCListen == "" {
		c.VNCListen = "127.0.0.1"
	}
	if c.OfflineNetwork == "" {
		c.OfflineNetwork = "tossbox-offline"
	}
	if c.OfflineSubnet == "" {
		c.OfflineSubnet = "192.168.251.0/24"
	}
	switch c.GPU {
	case "", "virgl", "sriov":
	default:
		return nil, fmt.Errorf("libvirt: gpu must be virgl, sriov or empty, got %q", c.GPU)
	}
	if c.Windows.DiskBus == "" {
		c.Windows.DiskBus = "virtio"
	}
	if c.Windows.NetModel == "" {
		c.Windows.NetModel = "virtio"
	}
	if c.Windows.Video == "" {
		c.Windows.Video = "vga"
	}
	log := env.Log
	if log == nil {
		log = slog.Default()
	}
	return &Driver{env: env, cfg: c, log: log.With("driver", "libvirt")}, nil
}

// Name implements driver.Driver.
func (d *Driver) Name() string { return "libvirt" }

func (d *Driver) conn() (*golibvirt.Libvirt, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.lv != nil && d.lv.IsConnected() {
		return d.lv, nil
	}
	if d.lv != nil {
		_ = d.lv.Disconnect()
	}
	lv := golibvirt.NewWithDialer(dialers.NewLocal(dialers.WithSocket(d.cfg.Socket), dialers.WithLocalTimeout(5*time.Second)))
	if err := lv.ConnectToURI(golibvirt.ConnectURI(d.cfg.URI)); err != nil {
		return nil, fmt.Errorf("libvirt: connect %s via %s: %w", d.cfg.URI, d.cfg.Socket, err)
	}
	d.lv = lv
	return lv, nil
}

// Capabilities implements driver.Driver.
func (d *Driver) Capabilities(ctx context.Context) *agentv1.DriverCapabilities {
	caps := &agentv1.DriverCapabilities{
		Name:     "libvirt",
		Families: d.cfg.Families,
		Services: []string{"ssh", "rdp", "vnc", "http", "https"},
		Healthy:  true,
	}
	lv, err := d.conn()
	if err == nil {
		_, err = lv.StoragePoolLookupByName(d.cfg.StoragePool)
		if err != nil {
			err = fmt.Errorf("storage pool %q: %w", d.cfg.StoragePool, err)
		}
	}
	if err == nil && d.cfg.Bridge == "" {
		if _, e := lv.NetworkLookupByName(d.cfg.Network); e != nil {
			err = fmt.Errorf("network %q: %w", d.cfg.Network, e)
		}
	}
	if err != nil {
		caps.Healthy = false
		caps.Message = err.Error()
	}
	caps.NetworkPolicies = []string{driver.PolicyInternet}
	if d.cfg.Bridge == "" {
		caps.NetworkPolicies = append(caps.NetworkPolicies, driver.PolicyOffline)
	}
	// A configured but unavailable GPU only disables GPU devices; the
	// message tells the admin why (Nodes page).
	gpuNote := ""
	switch d.cfg.GPU {
	case "virgl":
		if d.env.GPU != nil && d.env.GPU.RenderNode() != "" {
			caps.GpuMode, caps.GpuSlots = "virgl", -1
		} else {
			gpuNote = "gpu: virgl configured, but no render node found (mount /dev/dri into the agent container)"
		}
	case "sriov":
		if d.env.GPU != nil && len(d.env.GPU.VFs()) > 0 {
			caps.GpuMode, caps.GpuSlots = "sriov", int32(len(d.env.GPU.VFs()))
		} else {
			gpuNote = "gpu: sriov configured, but no SR-IOV virtual functions found (gpu.sriov.pf/vfs, see deploy/host-gpu.sh)"
		}
	}
	if gpuNote != "" {
		if caps.Message != "" {
			caps.Message += "; "
		}
		caps.Message += gpuNote
	}
	return caps
}

// offlineNetworkXML is an isolated libvirt network: no <forward> (libvirt
// rejects forwarding to and from it), no DNS, DHCP only.
func offlineNetworkXML(name, subnet string) (string, error) {
	p, err := netip.ParsePrefix(subnet)
	if err != nil || !p.Addr().Is4() || p.Bits() != 24 {
		return "", fmt.Errorf("libvirt: offline_subnet must be an IPv4 /24, got %q", subnet)
	}
	b := p.Masked().Addr().As4()
	ip := func(last byte) string { return netip.AddrFrom4([4]byte{b[0], b[1], b[2], last}).String() }
	return fmt.Sprintf(`<network>
  <name>%s</name>
  <bridge name='tb-virt-off' stp='on' delay='0'/>
  <dns enable='no'/>
  <ip address='%s' netmask='255.255.255.0'>
    <dhcp>
      <range start='%s' end='%s'/>
    </dhcp>
  </ip>
</network>`, x(name), ip(1), ip(2), ip(254)), nil
}

// ensureOfflineNetwork defines and starts the offline network if needed.
func (d *Driver) ensureOfflineNetwork(lv *golibvirt.Libvirt) error {
	net, err := lv.NetworkLookupByName(d.cfg.OfflineNetwork)
	if err != nil {
		if !isNoNetwork(err) {
			return err
		}
		xml, err := offlineNetworkXML(d.cfg.OfflineNetwork, d.cfg.OfflineSubnet)
		if err != nil {
			return err
		}
		if net, err = lv.NetworkDefineXML(xml); err != nil {
			return fmt.Errorf("libvirt: define offline network: %w", err)
		}
		d.log.Info("defined offline network", "network", d.cfg.OfflineNetwork, "subnet", d.cfg.OfflineSubnet)
	}
	if active, err := lv.NetworkIsActive(net); err == nil && active == 0 {
		if err := lv.NetworkCreate(net); err != nil {
			return fmt.Errorf("libvirt: start offline network: %w", err)
		}
	}
	_ = lv.NetworkSetAutostart(net, 1)
	return nil
}

// isNoNetwork: golibvirt.IsNotFound only matches missing domains.
func isNoNetwork(err error) bool {
	var e golibvirt.Error
	return errors.As(err, &e) && e.Code == uint32(golibvirt.ErrNoNetwork)
}

func isNoVol(err error) bool {
	var e golibvirt.Error
	return errors.As(err, &e) && e.Code == uint32(golibvirt.ErrNoStorageVol)
}

func diskName(id string) string { return driver.DomainName(id) + ".qcow2" }
func isoName(id string) string  { return driver.DomainName(id) + "-cidata.iso" }

// Ensure implements driver.Driver.
func (d *Driver) Ensure(ctx context.Context, spec *agentv1.DeviceSpec, report driver.Reporter) error {
	lv, err := d.conn()
	if err != nil {
		return err
	}
	name := driver.DomainName(spec.DeviceId)
	dom, err := lv.DomainLookupByName(name)
	if err != nil {
		if !golibvirt.IsNotFound(err) {
			return fmt.Errorf("libvirt: lookup %s: %w", name, err)
		}
		if dom, err = d.create(ctx, lv, spec, report); err != nil {
			return err
		}
	}
	state, _, err := lv.DomainGetState(dom, 0)
	if err != nil {
		return fmt.Errorf("libvirt: state: %w", err)
	}
	running := golibvirt.DomainState(state) == golibvirt.DomainRunning || golibvirt.DomainState(state) == golibvirt.DomainBlocked ||
		golibvirt.DomainState(state) == golibvirt.DomainPaused || golibvirt.DomainState(state) == golibvirt.DomainShutdown
	switch spec.Power {
	case agentv1.PowerState_POWER_STATE_STOPPED:
		if running {
			report(agentv1.DeviceState_DEVICE_STATE_STOPPING, "Shutting down")
			return d.shutdown(ctx, lv, dom)
		}
	default:
		if golibvirt.DomainState(state) == golibvirt.DomainPaused {
			return lv.DomainResume(dom)
		}
		if !running {
			report(agentv1.DeviceState_DEVICE_STATE_PROVISIONING, "Booting")
			if err := lv.DomainCreate(dom); err != nil {
				return fmt.Errorf("libvirt: start %s: %w", name, err)
			}
		}
	}
	return nil
}

func (d *Driver) shutdown(ctx context.Context, lv *golibvirt.Libvirt, dom golibvirt.Domain) error {
	_ = lv.DomainShutdown(dom)
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		st, _, err := lv.DomainGetState(dom, 0)
		if err != nil || golibvirt.DomainState(st) == golibvirt.DomainShutoff {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	d.log.Info("guest did not shut down in time, forcing power off", "domain", dom.Name)
	if err := lv.DomainDestroy(dom); err != nil && !golibvirt.IsNotFound(err) {
		return err
	}
	return nil
}

func (d *Driver) create(ctx context.Context, lv *golibvirt.Libvirt, spec *agentv1.DeviceSpec, report driver.Reporter) (golibvirt.Domain, error) {
	tpl := spec.GetTemplate()
	family := tpl.GetFamily()
	if !contains(d.cfg.Families, family) {
		return golibvirt.Domain{}, fmt.Errorf("libvirt: family %q not enabled on this node", family)
	}
	policy, err := driver.NetworkPolicy(spec)
	if err != nil {
		return golibvirt.Domain{}, err
	}
	network := d.cfg.Network
	if policy == driver.PolicyOffline {
		if d.cfg.Bridge != "" {
			return golibvirt.Domain{}, errors.New("libvirt: the offline network policy needs a libvirt network (not bridge mode)")
		}
		if err := d.ensureOfflineNetwork(lv); err != nil {
			return golibvirt.Domain{}, err
		}
		network = d.cfg.OfflineNetwork
	}
	var virgl bool
	var renderNode, vf string
	if driver.WantsGPU(spec) {
		switch d.cfg.GPU {
		case "virgl":
			if d.env.GPU != nil {
				renderNode = d.env.GPU.RenderNode()
			}
			if renderNode == "" {
				return golibvirt.Domain{}, errors.New("libvirt: no GPU render node on this node")
			}
			virgl = true
		case "sriov":
			if d.env.GPU == nil {
				return golibvirt.Domain{}, errors.New("libvirt: GPU sharing is not configured")
			}
			if vf, err = d.env.GPU.Acquire(spec.DeviceId); err != nil {
				return golibvirt.Domain{}, err
			}
		default:
			return golibvirt.Domain{}, errors.New("libvirt: this node offers no GPU for VMs (drivers.libvirt.gpu)")
		}
	}
	pool, err := lv.StoragePoolLookupByName(d.cfg.StoragePool)
	if err != nil {
		return golibvirt.Domain{}, fmt.Errorf("libvirt: storage pool %q: %w", d.cfg.StoragePool, err)
	}

	// 1. Base image.
	report(agentv1.DeviceState_DEVICE_STATE_PROVISIONING, "Fetching image")
	local, err := d.env.Images.Fetch(ctx, tpl.GetImage(), func(done, total int64) {
		if total > 0 {
			report(agentv1.DeviceState_DEVICE_STATE_PROVISIONING, fmt.Sprintf("Downloading image (%d%%)", done*100/total))
		} else {
			report(agentv1.DeviceState_DEVICE_STATE_PROVISIONING, fmt.Sprintf("Downloading image (%d MB)", done>>20))
		}
	})
	if err != nil {
		return golibvirt.Domain{}, err
	}
	basePath, baseFormat, virtSize, err := d.ensureBase(lv, pool, tpl.GetImage(), local, report)
	if err != nil {
		return golibvirt.Domain{}, err
	}

	// 2. Overlay disk (linked clone).
	report(agentv1.DeviceState_DEVICE_STATE_PROVISIONING, "Creating disk")
	capacity := uint64(spec.GetResources().GetStorageGb()) << 30
	if capacity < virtSize {
		capacity = virtSize
	}
	if old, err := lv.StorageVolLookupByName(pool, diskName(spec.DeviceId)); err == nil {
		_ = lv.StorageVolDelete(old, 0) // leftover of an interrupted create
	}
	disk, err := lv.StorageVolCreateXML(pool, volumeXML(diskName(spec.DeviceId), capacity, "qcow2", basePath, baseFormat), 0)
	if err != nil {
		return golibvirt.Domain{}, fmt.Errorf("libvirt: create disk: %w", err)
	}
	diskPath, err := lv.StorageVolGetPath(disk)
	if err != nil {
		return golibvirt.Domain{}, err
	}

	// 3. Seed ISO.
	password := ""
	for _, s := range spec.GetServices() {
		if s.Password != "" {
			password = s.Password
			break
		}
	}
	productKey := ""
	if family == "windows" {
		if productKey, err = d.env.Licenses.Resolve(spec.DeviceId, spec.GetLicense()); err != nil {
			return golibvirt.Domain{}, err
		}
	}
	isoPath := ""
	if tpl.GetGuestProfile() != "none" { // "none": the guest isn't provisioned (e.g. ChromeOS)
		seed, err := cloudinit.ForProfile(tpl.GetGuestProfile(), cloudinit.Params{
			InstanceID: spec.DeviceId,
			Hostname:   spec.Hostname,
			User:       tpl.GetDefaultUser(),
			Password:   password,
			SSHKeys:    spec.GetSshAuthorizedKeys(),
			ProductKey: productKey,
		}.WithTemplate(tpl.GetParams()))
		if err != nil {
			return golibvirt.Domain{}, err
		}
		iso, err := seed.ISO()
		if err != nil {
			return golibvirt.Domain{}, err
		}
		if isoPath, err = d.uploadVolume(lv, pool, isoName(spec.DeviceId), "raw", int64(len(iso)), strings.NewReader(string(iso)), nil); err != nil {
			return golibvirt.Domain{}, err
		}
	}

	// 4. Domain.
	res := spec.GetResources()
	vcpus := res.GetVcpus()
	if vcpus <= 0 {
		vcpus = 1
	}
	mem := res.GetRamMb()
	if mem <= 0 {
		mem = 1024
	}
	var quota int64
	if cap := res.GetCpuCapPercent(); cap > 0 && cap < 100 {
		quota = int64(cap) * int64(hostinfo.Cores()) * 1000 // of a 100ms period
		if quota < 1000 {
			quota = 1000
		}
	}
	vncPW := password
	if len(vncPW) > 8 {
		vncPW = vncPW[:8] // VNC auth only uses 8 characters
	}
	win := family == "windows"
	p := domainParams{
		Name: driver.DomainName(spec.DeviceId), Title: spec.Hostname, DeviceID: spec.DeviceId,
		Hostname: spec.Hostname, TemplateID: tpl.GetId(), Family: family,
		MemoryMB: mem, VCPUs: vcpus, GlobalQuota: quota,
		Machine: d.cfg.Machine, CPUMode: d.cfg.CPUMode,
		DiskPath: diskPath, ISOPath: isoPath,
		DiskBus: "virtio", NetModel: "virtio", Video: "virtio",
		Network: network, Bridge: d.cfg.Bridge, IsolatePorts: boolOr(d.cfg.IsolatePorts, true),
		VirGL: virgl, RenderNode: renderNode, HostdevPCI: vf,
		EFI:       tpl.GetParams()["firmware"] == "efi",
		VNCListen: d.cfg.VNCListen, VNCPassword: vncPW,
		Windows: win,
	}
	if win {
		p.DiskBus, p.NetModel, p.Video = d.cfg.Windows.DiskBus, d.cfg.Windows.NetModel, d.cfg.Windows.Video
		p.SecureBoot = boolOr(d.cfg.Windows.SecureBoot, true)
		p.TPM = boolOr(d.cfg.Windows.TPM, true)
		p.Loader, p.NVRAMTemplate = d.cfg.Windows.Loader, d.cfg.Windows.NVRAMTemplate
	}
	if win {
		// Template params override the node defaults.
		if v := tpl.GetParams()["tpm"]; v != "" {
			p.TPM = v == "true"
		}
		if v := tpl.GetParams()["secure_boot"]; v != "" {
			p.SecureBoot = v == "true"
		}
	}
	if v := tpl.GetParams()["disk_bus"]; v != "" {
		p.DiskBus = v
	}
	if v := tpl.GetParams()["video"]; v != "" {
		p.Video = v
	}
	if v := tpl.GetParams()["net_model"]; v != "" {
		p.NetModel = v
	}
	domXML, err := renderDomain(p)
	if err != nil {
		return golibvirt.Domain{}, err
	}
	dom, err := lv.DomainDefineXML(domXML)
	if err != nil {
		return golibvirt.Domain{}, fmt.Errorf("libvirt: define domain: %w", err)
	}
	d.log.Info("device defined", "device", spec.DeviceId, "hostname", spec.Hostname, "template", tpl.GetId())
	return dom, nil
}

// ensureBase uploads the base image into the pool once and returns its path,
// format and virtual size.
func (d *Driver) ensureBase(lv *golibvirt.Libvirt, pool golibvirt.StoragePool, img *agentv1.ImageSpec, local string, report driver.Reporter) (string, string, uint64, error) {
	format, virt, err := probeImage(local)
	if err != nil {
		return "", "", 0, err
	}
	h := sha256.Sum256([]byte(img.GetUrl() + "|" + img.GetSha256()))
	name := "tb-base-" + hex.EncodeToString(h[:8]) + ".img"

	d.baseMu.Lock()
	defer d.baseMu.Unlock()
	if vol, err := lv.StorageVolLookupByName(pool, name); err == nil {
		p, err := lv.StorageVolGetPath(vol)
		return p, format, virt, err
	} else if !isNoVol(err) {
		return "", "", 0, err
	}
	fi, err := os.Stat(local)
	if err != nil {
		return "", "", 0, err
	}
	f, err := os.Open(local)
	if err != nil {
		return "", "", 0, err
	}
	defer f.Close()
	p, err := d.uploadVolume(lv, pool, name, "raw", fi.Size(), f, func(done int64) {
		report(agentv1.DeviceState_DEVICE_STATE_PROVISIONING, fmt.Sprintf("Importing image into storage pool (%d%%)", done*100/max(fi.Size(), 1)))
	})
	return p, format, virt, err
}

// uploadVolume creates a volume of exactly size bytes and streams r into it.
// On failure the partial volume is removed.
func (d *Driver) uploadVolume(lv *golibvirt.Libvirt, pool golibvirt.StoragePool, name, format string, size int64, r io.Reader, progress func(done int64)) (string, error) {
	if old, err := lv.StorageVolLookupByName(pool, name); err == nil {
		_ = lv.StorageVolDelete(old, 0)
	}
	vol, err := lv.StorageVolCreateXML(pool, volumeXML(name, uint64(size), format, "", ""), 0)
	if err != nil {
		return "", fmt.Errorf("libvirt: create volume %s: %w", name, err)
	}
	if progress != nil {
		r = &countingReader{r: r, fn: progress}
	}
	if err := lv.StorageVolUpload(vol, r, 0, uint64(size), 0); err != nil {
		_ = lv.StorageVolDelete(vol, 0)
		return "", fmt.Errorf("libvirt: upload %s: %w", name, err)
	}
	return lv.StorageVolGetPath(vol)
}

type countingReader struct {
	r    io.Reader
	n    int64
	last time.Time
	fn   func(int64)
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	if time.Since(c.last) > time.Second {
		c.last = time.Now()
		c.fn(c.n)
	}
	return n, err
}

// probeImage detects qcow2 vs raw and returns the virtual disk size.
func probeImage(p string) (string, uint64, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	hdr := make([]byte, 32)
	if _, err := io.ReadFull(f, hdr); err == nil && string(hdr[:4]) == "QFI\xfb" {
		return "qcow2", binary.BigEndian.Uint64(hdr[24:32]), nil
	}
	fi, err := f.Stat()
	if err != nil {
		return "", 0, err
	}
	return "raw", uint64(fi.Size()), nil
}

// Destroy implements driver.Driver.
func (d *Driver) Destroy(ctx context.Context, deviceID string) error {
	lv, err := d.conn()
	if err != nil {
		return err
	}
	name := driver.DomainName(deviceID)
	if dom, err := lv.DomainLookupByName(name); err == nil {
		if st, _, err := lv.DomainGetState(dom, 0); err == nil && golibvirt.DomainState(st) != golibvirt.DomainShutoff {
			if err := lv.DomainDestroy(dom); err != nil && !golibvirt.IsNotFound(err) {
				return fmt.Errorf("libvirt: power off %s: %w", name, err)
			}
		}
		flags := golibvirt.DomainUndefineManagedSave | golibvirt.DomainUndefineSnapshotsMetadata | golibvirt.DomainUndefineNvram | golibvirt.DomainUndefineTpm
		if err := lv.DomainUndefineFlags(dom, flags); err != nil && !golibvirt.IsNotFound(err) {
			// Older libvirt without TPM flag support.
			if err2 := lv.DomainUndefineFlags(dom, flags&^golibvirt.DomainUndefineTpm); err2 != nil && !golibvirt.IsNotFound(err2) {
				return fmt.Errorf("libvirt: undefine %s: %w", name, err)
			}
		}
	} else if !golibvirt.IsNotFound(err) {
		return err
	}
	pool, err := lv.StoragePoolLookupByName(d.cfg.StoragePool)
	if err != nil {
		return err
	}
	for _, v := range []string{diskName(deviceID), isoName(deviceID)} {
		vol, err := lv.StorageVolLookupByName(pool, v)
		if isNoVol(err) {
			continue
		}
		if err != nil {
			return err
		}
		if err := lv.StorageVolDelete(vol, 0); err != nil && !isNoVol(err) {
			return fmt.Errorf("libvirt: delete volume %s: %w", v, err)
		}
	}
	d.env.Licenses.Release(deviceID)
	if d.env.GPU != nil {
		d.env.GPU.Release(deviceID)
	}
	return nil
}

// List implements driver.Driver.
func (d *Driver) List(ctx context.Context) ([]*agentv1.DeviceStatus, error) {
	lv, err := d.conn()
	if err != nil {
		return nil, err
	}
	doms, _, err := lv.ConnectListAllDomains(1, golibvirt.ConnectListDomainsActive|golibvirt.ConnectListDomainsInactive)
	if err != nil {
		return nil, err
	}
	var out []*agentv1.DeviceStatus
	for _, dom := range doms {
		if !strings.HasPrefix(dom.Name, "tb-") {
			continue
		}
		xmlDesc, err := lv.DomainGetXMLDesc(dom, 0)
		if err != nil {
			continue
		}
		ld, err := parseDomain(xmlDesc)
		if err != nil || ld.Metadata.Device == nil {
			continue // not created by Tossbox
		}
		st, err := d.status(lv, dom, ld.Metadata.Device.ID)
		if err == nil {
			out = append(out, st)
		}
	}
	return out, nil
}

// Status implements driver.Driver.
func (d *Driver) Status(ctx context.Context, deviceID string) (*agentv1.DeviceStatus, error) {
	lv, err := d.conn()
	if err != nil {
		return nil, err
	}
	dom, err := lv.DomainLookupByName(driver.DomainName(deviceID))
	if golibvirt.IsNotFound(err) {
		return &agentv1.DeviceStatus{DeviceId: deviceID, State: agentv1.DeviceState_DEVICE_STATE_DESTROYED, UpdatedUnix: time.Now().Unix()}, nil
	}
	if err != nil {
		return nil, err
	}
	return d.status(lv, dom, deviceID)
}

func (d *Driver) status(lv *golibvirt.Libvirt, dom golibvirt.Domain, deviceID string) (*agentv1.DeviceStatus, error) {
	st, reason, err := lv.DomainGetState(dom, 0)
	if err != nil {
		return nil, err
	}
	s := &agentv1.DeviceStatus{DeviceId: deviceID, UpdatedUnix: time.Now().Unix()}
	switch golibvirt.DomainState(st) {
	case golibvirt.DomainRunning, golibvirt.DomainBlocked:
		s.State = agentv1.DeviceState_DEVICE_STATE_RUNNING
	case golibvirt.DomainPaused, golibvirt.DomainPmsuspended:
		s.State = agentv1.DeviceState_DEVICE_STATE_STOPPED
		s.Message = "paused"
	case golibvirt.DomainShutdown:
		s.State = agentv1.DeviceState_DEVICE_STATE_STOPPING
	case golibvirt.DomainShutoff:
		s.State = agentv1.DeviceState_DEVICE_STATE_STOPPED
	case golibvirt.DomainCrashed:
		s.State = agentv1.DeviceState_DEVICE_STATE_FAILED
		s.Message = "guest crashed (reason " + strconv.Itoa(int(reason)) + ")"
	default:
		s.State = agentv1.DeviceState_DEVICE_STATE_PENDING
	}
	if s.State == agentv1.DeviceState_DEVICE_STATE_RUNNING {
		s.InternalIp = d.ipOf(lv, dom)
		// RSS of the QEMU process: what the VM really takes from the host.
		if stats, err := lv.DomainMemoryStats(dom, uint32(golibvirt.DomainMemoryStatNr), 0); err == nil {
			for _, m := range stats {
				if m.Tag == int32(golibvirt.DomainMemoryStatRss) {
					s.MemoryUsedMb = int64(m.Val >> 10) // KiB
				}
			}
		}
	}
	if pool, err := lv.StoragePoolLookupByName(d.cfg.StoragePool); err == nil {
		if vol, err := lv.StorageVolLookupByName(pool, diskName(deviceID)); err == nil {
			if _, _, alloc, err := lv.StorageVolGetInfo(vol); err == nil {
				s.StorageUsedMb = int64(alloc >> 20)
			}
		}
	}
	return s, nil
}

func (d *Driver) ipOf(lv *golibvirt.Libvirt, dom golibvirt.Domain) string {
	for _, src := range []golibvirt.DomainInterfaceAddressesSource{golibvirt.DomainInterfaceAddressesSrcLease, golibvirt.DomainInterfaceAddressesSrcAgent} {
		ifaces, err := lv.DomainInterfaceAddresses(dom, uint32(src), 0)
		if err != nil {
			continue
		}
		for _, ifc := range ifaces {
			if ifc.Name == "lo" {
				continue
			}
			for _, a := range ifc.Addrs {
				ip := net.ParseIP(a.Addr)
				if a.Type == int32(golibvirt.IPAddrTypeIpv4) && ip != nil && !ip.IsLoopback() && !ip.IsLinkLocalUnicast() {
					return a.Addr
				}
			}
		}
	}
	return ""
}

// ServiceEndpoint lets the agent core forward services that are not served
// at internal_ip:internal_port. For libvirt this is the VNC console, which is
// provided by QEMU on the host (VNCListen:<autoport>) rather than by the guest.
func (d *Driver) ServiceEndpoint(ctx context.Context, deviceID, service string) (string, bool) {
	if service != "vnc" {
		return "", false
	}
	lv, err := d.conn()
	if err != nil {
		return "", true
	}
	dom, err := lv.DomainLookupByName(driver.DomainName(deviceID))
	if err != nil {
		return "", true
	}
	xmlDesc, err := lv.DomainGetXMLDesc(dom, 0)
	if err != nil {
		return "", true
	}
	ld, err := parseDomain(xmlDesc)
	if err != nil {
		return "", true
	}
	for _, g := range ld.Devices.Graphics {
		if g.Type == "vnc" && g.Port > 0 {
			return net.JoinHostPort(d.cfg.VNCListen, strconv.Itoa(g.Port)), true
		}
	}
	return "", true
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
