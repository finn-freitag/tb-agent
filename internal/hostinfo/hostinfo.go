// Package hostinfo reports host hardware and usage.
package hostinfo

import (
	"runtime"

	"golang.org/x/sys/unix"
)

// Arch returns the host architecture in Tossbox notation ("x86_64", "arm64").
func Arch() string {
	switch runtime.GOARCH {
	case "amd64":
		return "x86_64"
	default:
		return runtime.GOARCH
	}
}

// OS returns "linux" or "darwin".
func OS() string { return runtime.GOOS }

// Cores returns the number of logical CPUs.
func Cores() int32 { return int32(runtime.NumCPU()) }

// StorageGB returns total and free GiB of the filesystem holding path.
func StorageGB(path string) (total, free int64) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, 0
	}
	bs := int64(st.Bsize)
	return int64(st.Blocks) * bs >> 30, int64(st.Bavail) * bs >> 30
}
