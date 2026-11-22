package dockerqemu

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"github.com/finn-freitag/tb-agent/internal/cloudinit"
)

// SeedBuilder writes a NoCloud seed ISO to path (overridable in tests).
type SeedBuilder func(ctx context.Context, path string, seed cloudinit.Seed) error

func writeSeedISO(_ context.Context, path string, seed cloudinit.Seed) error {
	b, err := seed.ISO()
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o640)
}

func have(bin string) bool { _, err := exec.LookPath(bin); return err == nil }

// resizeDisk grows a disk image to sizeGB with qemu-img when available
// (best effort: without qemu-img the image keeps its original size and the
// guest's cloud-init growpart has nothing to grow).
func resizeDisk(ctx context.Context, path string, sizeGB int64) {
	if sizeGB <= 0 || !have("qemu-img") {
		return
	}
	_ = exec.CommandContext(ctx, "qemu-img", "resize", "-q", path, fmt.Sprintf("%dG", sizeGB)).Run()
}

// dirUsage returns the allocated bytes of all files below dir (sparse aware).
func dirUsage(dir string) int64 {
	var total int64
	_ = filepath.WalkDir(dir, func(_ string, de os.DirEntry, err error) error {
		if err != nil || de.IsDir() {
			return nil
		}
		info, err := de.Info()
		if err != nil {
			return nil
		}
		if st, ok := info.Sys().(*syscall.Stat_t); ok {
			total += int64(st.Blocks) * 512
		} else {
			total += info.Size()
		}
		return nil
	})
	return total
}
