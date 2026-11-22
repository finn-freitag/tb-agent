package hostinfo

import (
	"bufio"
	"os"
	"strconv"
	"strings"
	"sync"
)

// RAMMB returns total host memory in MiB.
func RAMMB() int64 {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		fields := strings.Fields(s.Text())
		if len(fields) >= 2 && fields[0] == "MemTotal:" {
			kb, _ := strconv.ParseInt(fields[1], 10, 64)
			return kb / 1024
		}
	}
	return 0
}

var (
	cpuMu               sync.Mutex
	lastIdle, lastTotal uint64
)

// CPUPercent returns host CPU utilisation since the previous call (0-100).
func CPUPercent() float64 {
	b, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0
	}
	line := strings.SplitN(string(b), "\n", 2)[0]
	fields := strings.Fields(line)
	if len(fields) < 5 || fields[0] != "cpu" {
		return 0
	}
	var total, idle uint64
	for i, f := range fields[1:] {
		v, _ := strconv.ParseUint(f, 10, 64)
		total += v
		if i == 3 || i == 4 { // idle + iowait
			idle += v
		}
	}
	cpuMu.Lock()
	defer cpuMu.Unlock()
	dt, di := total-lastTotal, idle-lastIdle
	first := lastTotal == 0
	lastTotal, lastIdle = total, idle
	if first || dt == 0 {
		return 0
	}
	return 100 * float64(dt-di) / float64(dt)
}
