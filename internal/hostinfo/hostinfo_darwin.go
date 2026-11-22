package hostinfo

import "golang.org/x/sys/unix"

// RAMMB returns total host memory in MiB.
func RAMMB() int64 {
	v, err := unix.SysctlUint64("hw.memsize")
	if err != nil {
		return 0
	}
	return int64(v >> 20)
}

// RAMUsedMB is not measured on macOS.
func RAMUsedMB() int64 { return 0 }

// CPUPercent is not measured on macOS (orchard nodes are slot based).
func CPUPercent() float64 { return 0 }
