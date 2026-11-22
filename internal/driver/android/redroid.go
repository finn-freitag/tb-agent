package android

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/finn-freitag/tb-agent/internal/dockerapi"
	"github.com/finn-freitag/tb-agent/internal/driver"
	agentv1 "github.com/finn-freitag/tb-proto/gen/agent/v1"
)

// RedroidConfig configures the redroid backend.
type RedroidConfig struct {
	Enabled bool   `yaml:"enabled"`
	Socket  string `yaml:"socket"`
	Network string `yaml:"network"`
	Image   string `yaml:"image"`
	// DataDir holds each device's /data partition (bind mounted).
	DataDir string `yaml:"data_dir"`
	// HostDataDir is DataDir as seen by the Docker daemon (agent in a container).
	HostDataDir string `yaml:"host_data_dir"`
	Width       int    `yaml:"width"`
	Height      int    `yaml:"height"`
	DPI         int    `yaml:"dpi"`
	GPUMode     string `yaml:"gpu_mode"`
	// UseMemfd passes androidboot.use_memfd=true (no ashmem module needed). Default true.
	UseMemfd *bool `yaml:"use_memfd"`
}

type redroid struct {
	cfg    RedroidConfig
	env    driver.Env
	docker *dockerapi.Client
	// binderCheck verifies kernel binder support (overridable in tests).
	binderCheck func() error
	// ashmemCheck reports whether the kernel has ashmem (overridable in tests).
	ashmemCheck func() bool
}

func newRedroid(env driver.Env, cfg RedroidConfig) *redroid {
	if cfg.Network == "" {
		cfg.Network = "bridge"
	}
	if cfg.Image == "" {
		cfg.Image = "redroid/redroid:15.0.0_64only-latest"
	}
	if cfg.DataDir == "" {
		cfg.DataDir = filepath.Join(env.DataDir, "redroid")
	}
	if cfg.Width == 0 {
		cfg.Width = 720
	}
	if cfg.Height == 0 {
		cfg.Height = 1280
	}
	if cfg.DPI == 0 {
		cfg.DPI = 320
	}
	if cfg.GPUMode == "" {
		cfg.GPUMode = "guest"
	}
	if cfg.UseMemfd == nil {
		t := true
		cfg.UseMemfd = &t
	}
	return &redroid{cfg: cfg, env: env, docker: dockerapi.New(cfg.Socket), binderCheck: checkBinder, ashmemCheck: hasAshmem}
}

func (r *redroid) name() string       { return "redroid" }
func (r *redroid) services() []string { return []string{"adb"} }

func (r *redroid) serviceEndpoint(context.Context, string, string) (string, bool) { return "", false }

// hasAshmem reports whether the kernel provides the ashmem misc device.
// /proc/misc is kernel-wide, so this also works inside the agent container.
// Linux removed ashmem in 5.18; some distributions ship it as a module.
func hasAshmem() bool {
	b, err := os.ReadFile("/proc/misc")
	return err == nil && strings.Contains(string(b), " ashmem\n")
}

// checkBinder looks for binder support: binderfs in /proc/filesystems or a
// /dev/binder device (binder_linux module loaded).
func checkBinder() error {
	if runtime.GOOS != "linux" {
		return errors.New("redroid needs a Linux host")
	}
	if b, err := os.ReadFile("/proc/filesystems"); err == nil && strings.Contains(string(b), "binder") {
		return nil
	}
	if _, err := os.Stat("/dev/binder"); err == nil {
		return nil
	}
	return errors.New(`kernel binder support missing (load it: modprobe binder_linux devices="binder,hwbinder,vndbinder")`)
}

func (r *redroid) health(ctx context.Context) error {
	if err := r.docker.Ping(ctx); err != nil {
		return fmt.Errorf("docker daemon not reachable: %w", err)
	}
	return r.binderCheck()
}

func (r *redroid) dataDir(id string) string { return filepath.Join(r.cfg.DataDir, id) }

func (r *redroid) hostPath(p string) string {
	if r.cfg.HostDataDir == "" {
		return p
	}
	rel, err := filepath.Rel(r.cfg.DataDir, p)
	if err != nil || strings.HasPrefix(rel, "..") {
		return p
	}
	return filepath.Join(r.cfg.HostDataDir, rel)
}

// containerConfig maps a spec to the redroid container (pure, unit tested).
func (r *redroid) containerConfig(spec *agentv1.DeviceSpec) dockerapi.ContainerConfig {
	tpl := spec.GetTemplate()
	image := r.cfg.Image
	if u := tpl.GetImage().GetUrl(); strings.HasPrefix(u, "docker://") {
		image = strings.TrimPrefix(u, "docker://")
	}
	p := tpl.GetParams()
	width, height, dpi := orInt(p["redroid.width"], r.cfg.Width), orInt(p["redroid.height"], r.cfg.Height), orInt(p["redroid.dpi"], r.cfg.DPI)
	cmd := []string{
		fmt.Sprintf("androidboot.redroid_width=%d", width),
		fmt.Sprintf("androidboot.redroid_height=%d", height),
		fmt.Sprintf("androidboot.redroid_dpi=%d", dpi),
		"androidboot.redroid_gpu_mode=" + r.cfg.GPUMode,
	}
	if *r.cfg.UseMemfd {
		cmd = append(cmd, "androidboot.use_memfd=true")
	}
	res := spec.GetResources()
	hc := dockerapi.HostConfig{
		Privileged:    true,
		Binds:         []string{r.hostPath(r.dataDir(spec.DeviceId)) + ":/data"},
		RestartPolicy: dockerapi.RestartPolicy{Name: "unless-stopped"},
		NetworkMode:   r.cfg.Network,
	}
	if m := res.GetRamMb(); m > 0 {
		hc.Memory = m << 20
	}
	switch {
	case res.GetCpuCapPercent() > 0:
		hc.NanoCPUs = int64(runtime.NumCPU()) * int64(res.GetCpuCapPercent()) * 1e7
	case res.GetVcpus() > 0:
		hc.NanoCPUs = int64(res.GetVcpus()) * 1e9
	}
	return dockerapi.ContainerConfig{
		Image:    image,
		Hostname: spec.GetHostname(),
		Cmd:      cmd,
		Labels: map[string]string{
			labelDevice:  spec.DeviceId,
			labelDriver:  name,
			labelBackend: "redroid",
		},
		HostConfig: hc,
	}
}

func (r *redroid) ensure(ctx context.Context, spec *agentv1.DeviceSpec, report driver.Reporter) error {
	// Some images (e.g. redroid 16) still allocate graphics buffers via
	// ashmem even with use_memfd; without it SurfaceFlinger crash-loops and
	// Android never finishes booting.
	if spec.GetTemplate().GetParams()["redroid.requires_ashmem"] == "true" && !r.ashmemCheck() {
		return errors.New("this Android image needs the ashmem kernel driver, which this node's kernel does not provide (removed in Linux 5.18); use the Android 15 template or a node with ashmem")
	}
	cname := driver.DomainName(spec.DeviceId)
	ci, err := r.docker.InspectContainer(ctx, cname)
	if errors.Is(err, dockerapi.ErrNotFound) {
		cfg := r.containerConfig(spec)
		if err := os.MkdirAll(r.dataDir(spec.DeviceId), 0o750); err != nil {
			return err
		}
		if ok, _ := r.docker.ImageExists(ctx, cfg.Image); !ok {
			report(agentv1.DeviceState_DEVICE_STATE_PROVISIONING, "Pulling "+cfg.Image)
			err := r.docker.PullImage(ctx, cfg.Image, func(done, total int64, _ string) {
				if total > 0 {
					report(agentv1.DeviceState_DEVICE_STATE_PROVISIONING, fmt.Sprintf("Pulling %s (%d%%)", cfg.Image, done*100/total))
				}
			})
			if err != nil {
				return err
			}
		}
		report(agentv1.DeviceState_DEVICE_STATE_PROVISIONING, "Creating Android container")
		if _, err := r.docker.CreateContainer(ctx, cname, cfg); err != nil {
			return err
		}
		if ci, err = r.docker.InspectContainer(ctx, cname); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	if spec.Power == agentv1.PowerState_POWER_STATE_STOPPED {
		if ci.State.Running {
			return r.docker.StopContainer(ctx, ci.ID, 30*time.Second)
		}
		return nil
	}
	if !ci.State.Running {
		report(agentv1.DeviceState_DEVICE_STATE_PROVISIONING, "Booting Android")
		return r.docker.StartContainer(ctx, ci.ID)
	}
	return nil
}

func (r *redroid) destroy(ctx context.Context, id string) error {
	if err := r.docker.RemoveContainer(ctx, driver.DomainName(id)); err != nil {
		return err
	}
	return os.RemoveAll(r.dataDir(id))
}

func (r *redroid) list(ctx context.Context) ([]string, error) {
	cs, err := r.docker.ListContainers(ctx, labelDriver+"="+name, labelBackend+"=redroid")
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, c := range cs {
		if id := c.Labels[labelDevice]; id != "" {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

func (r *redroid) status(ctx context.Context, id string) (*agentv1.DeviceStatus, error) {
	ci, err := r.docker.InspectContainer(ctx, driver.DomainName(id))
	if errors.Is(err, dockerapi.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if ci.Config.Labels[labelBackend] != "redroid" {
		return nil, nil
	}
	st := &agentv1.DeviceStatus{DeviceId: id, UpdatedUnix: time.Now().Unix()}
	switch ci.State.Status {
	case "running", "restarting":
		st.State = agentv1.DeviceState_DEVICE_STATE_RUNNING
		st.InternalIp = ci.IP(r.cfg.Network)
		if mb, err := r.docker.ContainerMemoryMB(ctx, ci.ID); err == nil {
			st.MemoryUsedMb = mb
		}
	case "created":
		st.State = agentv1.DeviceState_DEVICE_STATE_PENDING
	case "exited", "paused":
		st.State = agentv1.DeviceState_DEVICE_STATE_STOPPED
	case "dead":
		st.State = agentv1.DeviceState_DEVICE_STATE_FAILED
		st.Message = ci.State.Error
	default:
		st.State = agentv1.DeviceState_DEVICE_STATE_PROVISIONING
	}
	return st, nil
}

func orInt(s string, def int) int {
	var v int
	if _, err := fmt.Sscan(s, &v); err == nil && v > 0 {
		return v
	}
	return def
}
