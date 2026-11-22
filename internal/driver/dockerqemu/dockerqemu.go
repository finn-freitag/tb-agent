// Package dockerqemu implements the "docker-qemu" driver: one Docker container
// per VM, running QEMU inside the container (dockur/windows for Windows,
// qemux/qemu for Linux and other disk images).
//
// Networking: the containers use Docker's bridge (or a configured network).
// Both images NAT the guest behind the container IP and forward every port
// except their own web/VNC ports to the guest, so the agent core can reach the
// guest's services at <container IP>:<internal port>. VNC (5900) reaches
// QEMU's display server inside the container.
package dockerqemu

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/finn-freitag/tb-agent/internal/cloudinit"
	"github.com/finn-freitag/tb-agent/internal/dockerapi"
	"github.com/finn-freitag/tb-agent/internal/driver"
	agentv1 "github.com/finn-freitag/tb-proto/gen/agent/v1"
	"gopkg.in/yaml.v3"
)

const name = "docker-qemu"

// Labels put on every container created by this driver.
const (
	LabelDevice = "tossbox.device"
	LabelDriver = "tossbox.driver"
)

// Config is the driver's section in the node config.
//
//	drivers:
//	  docker-qemu:
//	    socket: /var/run/docker.sock
//	    network: bridge
//	    storage_dir: /var/lib/tb-agent/docker-qemu   # per device storage, bind mounted to /storage
//	    host_storage_dir: ""                          # same dir as seen by the Docker daemon (if the agent runs in a container with a different mount path)
//	    prepared_dir: /var/lib/tb-agent/prepared      # prepared dockur storage snapshots ("prepared_storage" template param)
//	    windows_image: dockurr/windows
//	    linux_image: qemux/qemu
//	    stop_timeout: 120s
type Config struct {
	Socket         string        `yaml:"socket"`
	Network        string        `yaml:"network"`
	StorageDir     string        `yaml:"storage_dir"`
	HostStorageDir string        `yaml:"host_storage_dir"`
	PreparedDir    string        `yaml:"prepared_dir"`
	WindowsImage   string        `yaml:"windows_image"`
	LinuxImage     string        `yaml:"linux_image"`
	StopTimeout    time.Duration `yaml:"stop_timeout"`
	// KVMDevice is checked for existence to report health (default /dev/kvm).
	KVMDevice string `yaml:"kvm_device"`
}

func (c *Config) defaults(env driver.Env) {
	if c.Socket == "" {
		c.Socket = dockerapi.DefaultSocket
	}
	if c.Network == "" {
		c.Network = "bridge"
	}
	if c.StorageDir == "" {
		c.StorageDir = filepath.Join(env.DataDir, "devices")
	}
	if c.PreparedDir == "" {
		c.PreparedDir = filepath.Join(env.DataDir, "prepared")
	}
	if c.WindowsImage == "" {
		c.WindowsImage = "dockurr/windows"
	}
	if c.LinuxImage == "" {
		c.LinuxImage = "qemux/qemu"
	}
	if c.StopTimeout == 0 {
		c.StopTimeout = 120 * time.Second
	}
	if c.KVMDevice == "" {
		c.KVMDevice = "/dev/kvm"
	}
}

// Driver is the docker-qemu driver.
type Driver struct {
	cfg    Config
	env    driver.Env
	docker *dockerapi.Client
	// seedISO builds a NoCloud seed ISO (overridable in tests).
	seedISO SeedBuilder

	mu       sync.Mutex
	messages map[string]string // last provisioning message per device
}

func init() {
	driver.Register(name, func(env driver.Env, node *yaml.Node) (driver.Driver, error) {
		var cfg Config
		if err := driver.DecodeConfig(node, &cfg); err != nil {
			return nil, fmt.Errorf("docker-qemu config: %w", err)
		}
		return New(env, cfg), nil
	})
}

// New creates the driver.
func New(env driver.Env, cfg Config) *Driver {
	cfg.defaults(env)
	return &Driver{cfg: cfg, env: env, docker: dockerapi.New(cfg.Socket), seedISO: writeSeedISO, messages: map[string]string{}}
}

// Name implements driver.Driver.
func (d *Driver) Name() string { return name }

// Capabilities implements driver.Driver.
func (d *Driver) Capabilities(ctx context.Context) *agentv1.DriverCapabilities {
	caps := &agentv1.DriverCapabilities{
		Name:     name,
		Families: []string{"windows", "linux"},
		Services: []string{"ssh", "rdp", "vnc", "http", "https"},
		Healthy:  true,
	}
	if err := d.docker.Ping(ctx); err != nil {
		caps.Healthy = false
		caps.Message = "docker daemon not reachable: " + err.Error()
		return caps
	}
	if _, err := os.Stat(d.cfg.KVMDevice); err != nil {
		caps.Healthy = false
		caps.Message = "KVM not available: " + err.Error()
	}
	return caps
}

func (d *Driver) setMessage(id, msg string) {
	d.mu.Lock()
	d.messages[id] = msg
	d.mu.Unlock()
}

func (d *Driver) deviceDir(id string) string { return filepath.Join(d.cfg.StorageDir, id) }

// hostPath translates a path in the agent's filesystem into the path the
// Docker daemon sees (only differs when the agent itself runs in a container).
func (d *Driver) hostPath(p string) string {
	if d.cfg.HostStorageDir == "" {
		return p
	}
	rel, err := filepath.Rel(d.cfg.StorageDir, p)
	if err != nil || strings.HasPrefix(rel, "..") {
		return p
	}
	return filepath.Join(d.cfg.HostStorageDir, rel)
}

// Ensure implements driver.Driver.
func (d *Driver) Ensure(ctx context.Context, spec *agentv1.DeviceSpec, report driver.Reporter) error {
	cname := driver.DomainName(spec.DeviceId)
	ci, err := d.docker.InspectContainer(ctx, cname)
	switch {
	case errors.Is(err, dockerapi.ErrNotFound):
		// Create it even if the desired power is "stopped", so the device
		// exists; applyPower below leaves it stopped.
		if err := d.create(ctx, spec, report); err != nil {
			d.setMessage(spec.DeviceId, err.Error())
			return err
		}
		ci, err = d.docker.InspectContainer(ctx, cname)
		if err != nil {
			return err
		}
	case err != nil:
		return err
	}
	return d.applyPower(ctx, ci, spec.Power)
}

func (d *Driver) applyPower(ctx context.Context, ci *dockerapi.ContainerInfo, power agentv1.PowerState) error {
	switch power {
	case agentv1.PowerState_POWER_STATE_STOPPED:
		if ci.State.Running {
			return d.docker.StopContainer(ctx, ci.ID, d.cfg.StopTimeout)
		}
	default:
		if !ci.State.Running {
			return d.docker.StartContainer(ctx, ci.ID)
		}
	}
	return nil
}

// containerPlan is the result of mapping a DeviceSpec to a container.
type containerPlan struct {
	Image  string
	Config dockerapi.ContainerConfig
	// Files to prepare in the device dir before creating the container.
	BootImage       *agentv1.ImageSpec // Linux: downloaded and copied to <dir>/boot.<ext>
	BootFile        string
	SeedISO         bool   // Linux: build <dir>/seed.iso
	PreparedStorage string // Windows: copy this prepared dir into <dir>/storage
}

// plan maps a spec to a container plan (pure, unit tested).
func (d *Driver) plan(spec *agentv1.DeviceSpec, productKey string) (*containerPlan, error) {
	tpl := spec.GetTemplate()
	if tpl == nil {
		return nil, errors.New("docker-qemu: spec has no template")
	}
	res := spec.GetResources()
	params := mergedParams(tpl.GetParams(), spec.GetParams())
	user, password := credentials(spec)
	dir := d.deviceDir(spec.DeviceId)

	image := strings.TrimPrefix(tpl.GetImage().GetUrl(), "docker://")
	if image == "" || !strings.HasPrefix(tpl.GetImage().GetUrl(), "docker://") {
		switch tpl.GetFamily() {
		case "windows":
			image = d.cfg.WindowsImage
		default:
			image = d.cfg.LinuxImage
		}
	}

	env := []string{
		"CPU_CORES=" + fmt.Sprint(max(1, res.GetVcpus())),
		"RAM_SIZE=" + memSize(res.GetRamMb()),
	}
	if res.GetStorageGb() > 0 {
		env = append(env, fmt.Sprintf("DISK_SIZE=%dG", res.GetStorageGb()))
	}
	p := &containerPlan{Image: image}
	binds := []string{d.hostPath(filepath.Join(dir, "storage")) + ":/storage"}

	switch tpl.GetFamily() {
	case "windows":
		env = append(env,
			"USERNAME="+user,
			"PASSWORD="+password,
			"HOST="+spec.GetHostname(),
		)
		if v := params["docker_qemu.version"]; v != "" {
			env = append(env, "VERSION="+v)
		}
		if v := params["docker_qemu.edition"]; v != "" {
			env = append(env, "EDITION="+v)
		}
		if v := params["docker_qemu.language"]; v != "" {
			env = append(env, "LANGUAGE="+v)
		}
		if productKey != "" {
			env = append(env, "KEY="+productKey)
		}
		if hasService(spec, "ssh") {
			env = append(env, "COMMAND="+windowsSSHCommand)
		}
		if ps := params["docker_qemu.prepared_storage"]; ps != "" {
			p.PreparedStorage = resolvePrepared(d.cfg.PreparedDir, ps)
		}
	default:
		// Linux / generic disk image: boot a per-device copy of the cloud image
		// mounted as /boot.<ext> plus a NoCloud seed ISO.
		bootURL := params["docker_qemu.boot_image"]
		if bootURL == "" && !strings.HasPrefix(tpl.GetImage().GetUrl(), "docker://") {
			bootURL = tpl.GetImage().GetUrl()
		}
		if bootURL == "" {
			if v := params["docker_qemu.boot"]; v != "" {
				env = append(env, "BOOT="+v) // qemux built-in OS name, no provisioning
			} else {
				return nil, errors.New("docker-qemu: template has no boot image (param docker_qemu.boot_image)")
			}
		} else {
			ext := imageExt(bootURL, tpl.GetImage().GetFormat())
			p.BootImage = &agentv1.ImageSpec{Url: bootURL, Format: strings.TrimPrefix(ext, "."), Sha256: params["docker_qemu.boot_image_sha256"]}
			p.BootFile = filepath.Join(dir, "boot"+ext)
			binds = append(binds, d.hostPath(p.BootFile)+":/boot"+ext)
			if tpl.GetGuestProfile() == "cloud-init" {
				p.SeedISO = true
				binds = append(binds, d.hostPath(filepath.Join(dir, "seed.iso"))+":/seed.iso:ro")
				env = append(env, "ARGUMENTS=-drive file=/seed.iso,format=raw,if=virtio,readonly=on")
			}
		}
	}

	hc := dockerapi.HostConfig{
		Binds: binds,
		Devices: []dockerapi.DeviceMapping{
			{PathOnHost: "/dev/kvm", PathInContainer: "/dev/kvm", CgroupPermissions: "rwm"},
			{PathOnHost: "/dev/net/tun", PathInContainer: "/dev/net/tun", CgroupPermissions: "rwm"},
		},
		CapAdd:        []string{"NET_ADMIN"},
		RestartPolicy: dockerapi.RestartPolicy{Name: "unless-stopped"},
		NetworkMode:   d.cfg.Network,
	}
	if capPct := res.GetCpuCapPercent(); capPct > 0 {
		hc.NanoCPUs = int64(runtime.NumCPU()) * int64(capPct) * 1e7 // cores * pct/100 * 1e9
	}
	stop := int(d.cfg.StopTimeout.Seconds())
	p.Config = dockerapi.ContainerConfig{
		Image:       image,
		Hostname:    spec.GetHostname(),
		Env:         env,
		StopTimeout: &stop,
		Labels: map[string]string{
			LabelDevice:        spec.DeviceId,
			LabelDriver:        name,
			"tossbox.template": tpl.GetId(),
		},
		HostConfig: hc,
	}
	return p, nil
}

// windowsSSHCommand is run by dockur at the end of the unattended install to
// enable the OpenSSH server.
const windowsSSHCommand = `powershell -NoProfile -ExecutionPolicy Bypass -Command "Add-WindowsCapability -Online -Name OpenSSH.Server~~~~0.0.1.0; Set-Service -Name sshd -StartupType Automatic; Start-Service sshd; New-NetFirewallRule -Name sshd -DisplayName 'OpenSSH Server' -Enabled True -Direction Inbound -Protocol TCP -Action Allow -LocalPort 22 -ErrorAction SilentlyContinue"`

func (d *Driver) create(ctx context.Context, spec *agentv1.DeviceSpec, report driver.Reporter) error {
	id := spec.DeviceId
	var key string
	if spec.GetTemplate().GetFamily() == "windows" && d.env.Licenses != nil {
		k, err := d.env.Licenses.Resolve(id, spec.GetLicense())
		if err != nil {
			return fmt.Errorf("license: %w", err)
		}
		key = k
	}
	p, err := d.plan(spec, key)
	if err != nil {
		return err
	}
	dir := d.deviceDir(id)
	if err := os.MkdirAll(filepath.Join(dir, "storage"), 0o750); err != nil {
		return err
	}

	report(agentv1.DeviceState_DEVICE_STATE_PROVISIONING, "Pulling "+p.Image)
	if ok, _ := d.docker.ImageExists(ctx, p.Image); !ok {
		err := d.docker.PullImage(ctx, p.Image, func(done, total int64, _ string) {
			if total > 0 {
				report(agentv1.DeviceState_DEVICE_STATE_PROVISIONING, fmt.Sprintf("Pulling %s (%d%%)", p.Image, done*100/total))
			}
		})
		if err != nil {
			return err
		}
	}

	if p.PreparedStorage != "" {
		report(agentv1.DeviceState_DEVICE_STATE_PROVISIONING, "Copying prepared disk")
		if err := copyTree(p.PreparedStorage, filepath.Join(dir, "storage")); err != nil {
			return fmt.Errorf("copy prepared storage: %w", err)
		}
	}
	if p.BootImage != nil {
		if d.env.Images == nil {
			return errors.New("docker-qemu: no image cache available")
		}
		src, err := d.env.Images.Fetch(ctx, p.BootImage, func(done, total int64) {
			if total > 0 {
				report(agentv1.DeviceState_DEVICE_STATE_PROVISIONING, fmt.Sprintf("Downloading image (%d%%)", done*100/total))
			}
		})
		if err != nil {
			return err
		}
		report(agentv1.DeviceState_DEVICE_STATE_PROVISIONING, "Creating disk")
		if err := copyFile(src, p.BootFile); err != nil {
			return err
		}
		resizeDisk(ctx, p.BootFile, spec.GetResources().GetStorageGb())
	}
	if p.SeedISO {
		user, password := credentials(spec)
		seed, err := cloudinit.Linux(cloudinit.Params{
			InstanceID: id, Hostname: spec.GetHostname(), User: user, Password: password,
			SSHKeys: spec.GetSshAuthorizedKeys(),
		})
		if err != nil {
			return err
		}
		if err := d.seedISO(ctx, filepath.Join(dir, "seed.iso"), seed); err != nil {
			return fmt.Errorf("build seed iso: %w", err)
		}
	}

	report(agentv1.DeviceState_DEVICE_STATE_PROVISIONING, "Creating container")
	if _, err := d.docker.CreateContainer(ctx, driver.DomainName(id), p.Config); err != nil {
		return err
	}
	msg := "Booting"
	if spec.GetTemplate().GetFamily() == "windows" && p.PreparedStorage == "" {
		msg = "Installing Windows (this takes 10-20 minutes on first boot)"
	}
	d.setMessage(id, msg)
	report(agentv1.DeviceState_DEVICE_STATE_PROVISIONING, msg)
	return nil
}

// Destroy implements driver.Driver.
func (d *Driver) Destroy(ctx context.Context, deviceID string) error {
	if err := d.docker.RemoveContainer(ctx, driver.DomainName(deviceID)); err != nil {
		return err
	}
	if d.env.Licenses != nil {
		d.env.Licenses.Release(deviceID)
	}
	d.mu.Lock()
	delete(d.messages, deviceID)
	d.mu.Unlock()
	return os.RemoveAll(d.deviceDir(deviceID))
}

// List implements driver.Driver.
func (d *Driver) List(ctx context.Context) ([]*agentv1.DeviceStatus, error) {
	cs, err := d.docker.ListContainers(ctx, LabelDriver+"="+name)
	if err != nil {
		return nil, err
	}
	out := make([]*agentv1.DeviceStatus, 0, len(cs))
	for _, c := range cs {
		id := c.Labels[LabelDevice]
		if id == "" {
			continue
		}
		st, err := d.Status(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, nil
}

// Status implements driver.Driver.
func (d *Driver) Status(ctx context.Context, deviceID string) (*agentv1.DeviceStatus, error) {
	st := &agentv1.DeviceStatus{DeviceId: deviceID, UpdatedUnix: time.Now().Unix()}
	ci, err := d.docker.InspectContainer(ctx, driver.DomainName(deviceID))
	if errors.Is(err, dockerapi.ErrNotFound) {
		st.State = agentv1.DeviceState_DEVICE_STATE_DESTROYED
		return st, nil
	}
	if err != nil {
		return nil, err
	}
	st.State = containerState(ci.State.Status)
	if ci.State.Running {
		st.InternalIp = ci.IP(d.cfg.Network)
		if mb, err := d.docker.ContainerMemoryMB(ctx, ci.ID); err == nil {
			st.MemoryUsedMb = mb
		}
	}
	d.mu.Lock()
	st.Message = d.messages[deviceID]
	d.mu.Unlock()
	if ci.State.Error != "" {
		st.Message = ci.State.Error
	}
	st.StorageUsedMb = dirUsage(d.deviceDir(deviceID)) >> 20
	return st, nil
}

// containerState maps a Docker container status to a device state.
func containerState(s string) agentv1.DeviceState {
	switch s {
	case "running", "restarting":
		return agentv1.DeviceState_DEVICE_STATE_RUNNING
	case "created":
		return agentv1.DeviceState_DEVICE_STATE_PENDING
	case "exited", "paused":
		return agentv1.DeviceState_DEVICE_STATE_STOPPED
	case "removing":
		return agentv1.DeviceState_DEVICE_STATE_DESTROYING
	case "dead":
		return agentv1.DeviceState_DEVICE_STATE_FAILED
	}
	return agentv1.DeviceState_DEVICE_STATE_UNSPECIFIED
}

// ---------------------------------------------------------------- helpers

func credentials(spec *agentv1.DeviceSpec) (user, password string) {
	user = spec.GetTemplate().GetDefaultUser()
	for _, s := range spec.GetServices() {
		if s.GetPassword() != "" {
			password = s.GetPassword()
			if s.GetUsername() != "" {
				user = s.GetUsername()
			}
			break
		}
	}
	if user == "" {
		user = "tossbox"
	}
	return user, password
}

func hasService(spec *agentv1.DeviceSpec, svc string) bool {
	for _, s := range spec.GetServices() {
		if s.GetName() == svc {
			return true
		}
	}
	return false
}

func mergedParams(a, b map[string]string) map[string]string {
	out := make(map[string]string, len(a)+len(b))
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}

// memSize formats MiB for RAM_SIZE ("4G" or "1536M").
func memSize(mb int64) string {
	if mb <= 0 {
		mb = 2048
	}
	if mb%1024 == 0 {
		return fmt.Sprintf("%dG", mb/1024)
	}
	return fmt.Sprintf("%dM", mb)
}

func imageExt(url, format string) string {
	switch format {
	case "qcow2", "raw", "img", "iso":
		if format == "raw" {
			return ".img"
		}
		return "." + format
	}
	u := strings.ToLower(url)
	for _, e := range []string{".qcow2", ".img", ".raw", ".iso"} {
		if strings.HasSuffix(u, e) {
			if e == ".raw" {
				return ".img"
			}
			return e
		}
	}
	return ".qcow2"
}

func resolvePrepared(dir, ref string) string {
	ref = strings.TrimPrefix(ref, "local:")
	if filepath.IsAbs(ref) {
		return ref
	}
	return filepath.Join(dir, filepath.Clean("/"+ref))
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, de os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		if de.IsDir() {
			return os.MkdirAll(target, 0o750)
		}
		if !de.Type().IsRegular() {
			return nil
		}
		return copyFile(p, target)
	})
}
