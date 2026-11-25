package driver

import (
	"sync"
	"time"
)

// CPUMeter turns cumulative CPU counters into a usage percentage between
// two samples. Drivers keep one per backend and call Percent on every
// status poll.
type CPUMeter struct {
	mu   sync.Mutex
	last map[string]cpuSample
}

type cpuSample struct{ used, total float64 }

// Percent records a sample for id: used is the device's cumulative CPU time
// and total the cumulative CPU time the whole node had available in the same
// unit (e.g. wall clock ns × cores). It returns the device's share of the
// node (0-100) since the previous sample, or 0 for the first sample.
func (m *CPUMeter) Percent(id string, used, total float64) float64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.last == nil {
		m.last = map[string]cpuSample{}
	}
	prev, ok := m.last[id]
	m.last[id] = cpuSample{used, total}
	if !ok || total <= prev.total || used < prev.used {
		return 0
	}
	p := (used - prev.used) / (total - prev.total) * 100
	switch {
	case p < 0:
		return 0
	case p > 100:
		return 100
	}
	return float64(int(p*10+0.5)) / 10 // one decimal
}

// Forget drops the samples of a device (destroyed or stopped).
func (m *CPUMeter) Forget(id string) {
	m.mu.Lock()
	delete(m.last, id)
	m.mu.Unlock()
}

// WallTotal is the "total" for Percent when the device's counter is CPU time
// in ns: wall clock ns × number of host cores.
func WallTotal(cores int) float64 {
	if cores < 1 {
		cores = 1
	}
	return float64(time.Now().UnixNano()) * float64(cores)
}
