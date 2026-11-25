package driver

import "testing"

func TestCPUMeter(t *testing.T) {
	var m CPUMeter
	if p := m.Percent("a", 100, 1000); p != 0 {
		t.Fatalf("first sample %v", p)
	}
	if p := m.Percent("a", 150, 1200); p != 25 { // 50 of 200
		t.Fatalf("got %v", p)
	}
	if p := m.Percent("a", 100, 1300); p != 0 { // counter reset (restart)
		t.Fatalf("reset %v", p)
	}
	m.Forget("a")
	if p := m.Percent("a", 500, 2000); p != 0 {
		t.Fatalf("after forget %v", p)
	}
}
