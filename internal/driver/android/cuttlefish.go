package android

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/finn-freitag/tb-agent/internal/driver"
	agentv1 "github.com/finn-freitag/tb-proto/gen/agent/v1"
)

// CuttlefishConfig configures the Cuttlefish backend.
type CuttlefishConfig struct {
	Enabled bool `yaml:"enabled"`
	// HostPackageDir is an extracted cvd-host_package (contains bin/launch_cvd).
	HostPackageDir string `yaml:"host_package_dir"`
	// ImagesDir holds the device images (aosp_cf_*-img-*.zip extracted).
	// A template image "local:<path>" is resolved relative to it.
	ImagesDir string `yaml:"images_dir"`
	// RuntimeDir holds one HOME directory per device.
	RuntimeDir   string   `yaml:"runtime_dir"`
	MaxInstances int      `yaml:"max_instances"`
	ExtraArgs    []string `yaml:"extra_args"`
}

// Cuttlefish base ports for instance 1 (instance N = base + N - 1).
const (
	cfADBBase = 6520
	cfVNCBase = 6444
)

type runFunc func(ctx context.Context, dir string, env []string, bin string, args ...string) error

type cuttlefish struct {
	cfg CuttlefishConfig
	env driver.Env
	run runFunc
	mu  sync.Mutex // serializes instance number allocation
}

func newCuttlefish(env driver.Env, cfg CuttlefishConfig) (*cuttlefish, error) {
	if cfg.HostPackageDir == "" {
		return nil, errors.New("android: cuttlefish.host_package_dir is required")
	}
	if cfg.RuntimeDir == "" {
		cfg.RuntimeDir = filepath.Join(env.DataDir, "cuttlefish")
	}
	if cfg.MaxInstances <= 0 {
		cfg.MaxInstances = 4
	}
	return &cuttlefish{cfg: cfg, env: env, run: execRun}, nil
}

func execRun(ctx context.Context, dir string, env []string, bin string, args ...string) error {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		tail := string(out)
		if len(tail) > 2000 {
			tail = tail[len(tail)-2000:]
		}
		return fmt.Errorf("%s: %v: %s", filepath.Base(bin), err, strings.TrimSpace(tail))
	}
	return nil
}

func (c *cuttlefish) name() string       { return "cuttlefish" }
func (c *cuttlefish) services() []string { return []string{"adb", "vnc"} }

func (c *cuttlefish) health(context.Context) error {
	if runtime.GOOS != "linux" {
		return errors.New("cuttlefish needs a Linux host")
	}
	if _, err := os.Stat(filepath.Join(c.cfg.HostPackageDir, "bin", "launch_cvd")); err != nil {
		return fmt.Errorf("launch_cvd not found in host package: %w", err)
	}
	if _, err := os.Stat("/dev/kvm"); err != nil {
		return fmt.Errorf("KVM not available: %w", err)
	}
	return nil
}

func (c *cuttlefish) home(id string) string { return filepath.Join(c.cfg.RuntimeDir, id) }

func (c *cuttlefish) instanceNum(id string) (int, error) {
	b, err := os.ReadFile(filepath.Join(c.home(id), "instance"))
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(b)))
}

// serviceEndpoint maps adb / vnc to instance N's host ports
// (127.0.0.1:6520+N-1 / 6444+N-1). Other services aren't served.
func (c *cuttlefish) serviceEndpoint(_ context.Context, id, service string) (string, bool) {
	n, err := c.instanceNum(id)
	if err != nil {
		return "", false
	}
	if !c.running(id) {
		return "", true
	}
	switch service {
	case "adb":
		return net.JoinHostPort("127.0.0.1", strconv.Itoa(cfADBBase+n-1)), true
	case "vnc":
		return net.JoinHostPort("127.0.0.1", strconv.Itoa(cfVNCBase+n-1)), true
	}
	return "", true
}

func (c *cuttlefish) running(id string) bool {
	_, err := os.Stat(filepath.Join(c.home(id), "running"))
	return err == nil
}

// allocate creates the device's HOME dir with a free instance number.
func (c *cuttlefish) allocate(id string) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if n, err := c.instanceNum(id); err == nil {
		return n, nil
	}
	used := map[int]bool{}
	ids, _ := c.list(context.Background())
	for _, other := range ids {
		if n, err := c.instanceNum(other); err == nil {
			used[n] = true
		}
	}
	n := 0
	for i := 1; i <= c.cfg.MaxInstances; i++ {
		if !used[i] {
			n = i
			break
		}
	}
	if n == 0 {
		return 0, fmt.Errorf("android: all %d Cuttlefish instances are in use", c.cfg.MaxInstances)
	}
	home := c.home(id)
	if err := os.MkdirAll(home, 0o750); err != nil {
		return 0, err
	}
	// Cuttlefish expects the host package in $HOME; link its entries.
	entries, err := os.ReadDir(c.cfg.HostPackageDir)
	if err != nil {
		return 0, err
	}
	for _, e := range entries {
		dst := filepath.Join(home, e.Name())
		if _, err := os.Lstat(dst); err == nil {
			continue
		}
		if err := os.Symlink(filepath.Join(c.cfg.HostPackageDir, e.Name()), dst); err != nil {
			return 0, err
		}
	}
	if err := os.WriteFile(filepath.Join(home, "instance"), []byte(strconv.Itoa(n)), 0o640); err != nil {
		return 0, err
	}
	return n, nil
}

// launchArgs builds the launch_cvd arguments (pure, unit tested).
func (c *cuttlefish) launchArgs(spec *agentv1.DeviceSpec, n int, first bool) []string {
	res := spec.GetResources()
	images := c.cfg.ImagesDir
	if u := spec.GetTemplate().GetImage().GetUrl(); strings.HasPrefix(u, "local:") {
		p := strings.TrimPrefix(u, "local:")
		if filepath.IsAbs(p) {
			images = p
		} else {
			images = filepath.Join(c.cfg.ImagesDir, filepath.Clean("/"+p))
		}
	}
	args := []string{
		"--daemon",
		"--base_instance_num=" + strconv.Itoa(n),
		"--num_instances=1",
		"--report_anonymous_usage_stats=n",
	}
	if images != "" {
		args = append(args, "--system_image_dir="+images)
	}
	if v := res.GetVcpus(); v > 0 {
		args = append(args, "--cpus="+strconv.Itoa(int(v)))
	}
	if m := res.GetRamMb(); m > 0 {
		args = append(args, "--memory_mb="+strconv.FormatInt(m, 10))
	}
	if first {
		args = append(args, "--data_policy=always_create")
		if s := res.GetStorageGb(); s > 0 {
			args = append(args, "--blank_data_image_mb="+strconv.FormatInt(s*1024, 10))
		}
	} else {
		args = append(args, "--data_policy=use_existing")
	}
	if hasService(spec, "vnc") {
		args = append(args, "--start_vnc_server=true")
	}
	return append(args, c.cfg.ExtraArgs...)
}

func (c *cuttlefish) ensure(ctx context.Context, spec *agentv1.DeviceSpec, report driver.Reporter) error {
	id := spec.DeviceId
	first := false
	n, err := c.instanceNum(id)
	if err != nil {
		if n, err = c.allocate(id); err != nil {
			return err
		}
		first = true
	}
	if _, err := os.Stat(filepath.Join(c.home(id), "created")); err != nil {
		first = true
	}
	home := c.home(id)
	env := []string{"HOME=" + home}
	switch spec.Power {
	case agentv1.PowerState_POWER_STATE_STOPPED:
		if c.running(id) {
			if err := c.run(ctx, home, env, filepath.Join(home, "bin", "stop_cvd")); err != nil {
				return err
			}
			os.Remove(filepath.Join(home, "running"))
		}
		return nil
	default:
		if c.running(id) {
			return nil
		}
		report(agentv1.DeviceState_DEVICE_STATE_PROVISIONING, "Booting Cuttlefish device")
		if err := c.run(ctx, home, env, filepath.Join(home, "bin", "launch_cvd"), c.launchArgs(spec, n, first)...); err != nil {
			return err
		}
		os.WriteFile(filepath.Join(home, "created"), nil, 0o640)
		os.WriteFile(filepath.Join(home, "running"), nil, 0o640)
		return nil
	}
}

func (c *cuttlefish) destroy(ctx context.Context, id string) error {
	home := c.home(id)
	if _, err := os.Stat(home); err != nil {
		return nil
	}
	if c.running(id) {
		// Best effort; the directory is removed anyway.
		_ = c.run(ctx, home, []string{"HOME=" + home}, filepath.Join(home, "bin", "stop_cvd"))
	}
	return os.RemoveAll(home)
}

func (c *cuttlefish) list(context.Context) ([]string, error) {
	entries, err := os.ReadDir(c.cfg.RuntimeDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(c.cfg.RuntimeDir, e.Name(), "instance")); err == nil {
			ids = append(ids, e.Name())
		}
	}
	return ids, nil
}

func (c *cuttlefish) status(_ context.Context, id string) (*agentv1.DeviceStatus, error) {
	if _, err := c.instanceNum(id); err != nil {
		return nil, nil
	}
	st := &agentv1.DeviceStatus{DeviceId: id, UpdatedUnix: time.Now().Unix(), StorageUsedMb: dirUsage(c.home(id)) >> 20}
	if c.running(id) {
		st.State = agentv1.DeviceState_DEVICE_STATE_RUNNING
		st.InternalIp = "127.0.0.1" // services resolve via serviceEndpoint
	} else {
		st.State = agentv1.DeviceState_DEVICE_STATE_STOPPED
	}
	return st, nil
}

// dirUsage returns allocated bytes below dir, not following symlinks.
func dirUsage(dir string) int64 {
	var total int64
	_ = filepath.WalkDir(dir, func(_ string, de os.DirEntry, err error) error {
		if err != nil || de.IsDir() || de.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if info, err := de.Info(); err == nil {
			if st, ok := info.Sys().(*syscall.Stat_t); ok {
				total += int64(st.Blocks) * 512
			} else {
				total += info.Size()
			}
		}
		return nil
	})
	return total
}
