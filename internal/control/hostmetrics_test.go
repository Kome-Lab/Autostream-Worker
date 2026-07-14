package control

import "testing"

func TestParseProcStatCPUTimes(t *testing.T) {
	got := parseProcStatCPUTimes("cpu  100 20 30 400 50 10 5 2 99 88\ncpu0 50 10 15 200 25 5 2 1 49 44\n")
	if got == nil || got.total != 617 || got.idle != 450 {
		t.Fatalf("unexpected CPU counters: %#v", got)
	}
}

func TestCPUUsedPercent(t *testing.T) {
	got, ok := cpuUsedPercent(cpuTimes{total: 1000, idle: 700}, cpuTimes{total: 1200, idle: 750})
	if !ok || got != 75 {
		t.Fatalf("unexpected CPU usage: value=%v ok=%v", got, ok)
	}
	if _, ok := cpuUsedPercent(cpuTimes{total: 1200, idle: 750}, cpuTimes{total: 1000, idle: 700}); ok {
		t.Fatal("counter reset must not produce CPU usage")
	}
}
