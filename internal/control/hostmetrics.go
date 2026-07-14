package control

import (
	"math"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

var processStartedAt = time.Now()
var cpuSampleState = struct {
	sync.Mutex
	previous *cpuTimes
}{}
var networkSampleState = struct {
	sync.Mutex
	previous *networkSample
}{}

type networkSample struct {
	at time.Time
	rx float64
	tx float64
}

type cpuTimes struct {
	total uint64
	idle  uint64
}

func NodeHostMetrics() map[string]float64 {
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	return mergeFloatMetrics(
		map[string]float64{
			"node.cpu_count":                 float64(runtime.NumCPU()),
			"process.goroutines":             float64(runtime.NumGoroutine()),
			"process.heap_alloc_bytes":       float64(mem.HeapAlloc),
			"process.heap_sys_bytes":         float64(mem.HeapSys),
			"process.heap_objects":           float64(mem.HeapObjects),
			"process.uptime_seconds":         time.Since(processStartedAt).Seconds(),
			"process.gc_pause_seconds_total": float64(mem.PauseTotalNs) / 1e9,
		},
		cpuMetrics(),
		procMemInfoMetrics(),
		procLoadMetrics(),
		filesystemMetrics(),
		networkMetrics(),
	)
}

func cpuMetrics() map[string]float64 {
	cpuSampleState.Lock()
	defer cpuSampleState.Unlock()
	current := cpuTimeCounters()
	if current == nil {
		return nil
	}
	previous := cpuSampleState.previous
	cpuSampleState.previous = current
	if previous == nil {
		return nil
	}
	usedPercent, ok := cpuUsedPercent(*previous, *current)
	if !ok {
		return nil
	}
	return map[string]float64{"node.cpu.used_percent": usedPercent}
}

func cpuUsedPercent(previous, current cpuTimes) (float64, bool) {
	if current.total <= previous.total || current.idle < previous.idle {
		return 0, false
	}
	totalDelta := current.total - previous.total
	idleDelta := current.idle - previous.idle
	if idleDelta > totalDelta {
		return 0, false
	}
	return float64(totalDelta-idleDelta) / float64(totalDelta) * 100, true
}

func parseProcStatCPUTimes(data string) *cpuTimes {
	for _, line := range strings.Split(data, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 || fields[0] != "cpu" {
			continue
		}
		limit := len(fields)
		if limit > 9 {
			limit = 9
		}
		values := make([]uint64, 0, limit-1)
		var total uint64
		for _, field := range fields[1:limit] {
			value, err := strconv.ParseUint(field, 10, 64)
			if err != nil {
				return nil
			}
			values = append(values, value)
			total += value
		}
		idle := values[3]
		if len(values) > 4 {
			idle += values[4]
		}
		if total == 0 || idle > total {
			return nil
		}
		return &cpuTimes{total: total, idle: idle}
	}
	return nil
}

func mergeFloatMetrics(inputs ...map[string]float64) map[string]float64 {
	out := make(map[string]float64)
	for _, input := range inputs {
		for key, value := range input {
			key = strings.TrimSpace(key)
			if key == "" || math.IsNaN(value) || math.IsInf(value, 0) {
				continue
			}
			out[key] = value
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func procMemInfoMetrics() map[string]float64 {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return nil
	}
	values := make(map[string]float64)
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		parsed, err := strconv.ParseFloat(fields[1], 64)
		if err != nil {
			continue
		}
		bytes := parsed * 1024
		switch strings.TrimSuffix(fields[0], ":") {
		case "MemTotal":
			values["node.memory.total_bytes"] = bytes
		case "MemFree":
			values["node.memory.free_bytes"] = bytes
		case "MemAvailable":
			values["node.memory.available_bytes"] = bytes
		case "Buffers":
			values["node.memory.buffers_bytes"] = bytes
		case "Cached":
			values["node.memory.cached_bytes"] = bytes
		}
	}
	total := values["node.memory.total_bytes"]
	if total > 0 {
		available := values["node.memory.available_bytes"]
		if available <= 0 {
			available = values["node.memory.free_bytes"]
		}
		used := total - available
		if used >= 0 {
			values["node.memory.used_bytes"] = used
			values["node.memory.used_percent"] = used / total * 100
		}
	}
	return values
}

func procLoadMetrics() map[string]float64 {
	data, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return nil
	}
	fields := strings.Fields(string(data))
	if len(fields) < 3 {
		return nil
	}
	keys := []string{"node.load1", "node.load5", "node.load15"}
	out := make(map[string]float64, len(keys))
	for i, key := range keys {
		value, err := strconv.ParseFloat(fields[i], 64)
		if err == nil {
			out[key] = value
		}
	}
	return out
}

func networkMetrics() map[string]float64 {
	counters := networkByteCounters()
	if counters == nil {
		return nil
	}
	rx := counters["rx"]
	tx := counters["tx"]
	now := time.Now()
	out := map[string]float64{
		"node.network.rx_bytes_total": rx,
		"node.network.tx_bytes_total": tx,
	}
	networkSampleState.Lock()
	defer networkSampleState.Unlock()
	if previous := networkSampleState.previous; previous != nil {
		elapsed := now.Sub(previous.at).Seconds()
		if elapsed > 0 {
			rxPerSec := (rx - previous.rx) / elapsed
			txPerSec := (tx - previous.tx) / elapsed
			if rxPerSec >= 0 {
				out["node.network.rx_bytes_per_sec"] = rxPerSec
				out["node.network.rx_kbps"] = rxPerSec * 8 / 1000
			}
			if txPerSec >= 0 {
				out["node.network.tx_bytes_per_sec"] = txPerSec
				out["node.network.tx_kbps"] = txPerSec * 8 / 1000
			}
		}
	}
	networkSampleState.previous = &networkSample{at: now, rx: rx, tx: tx}
	return out
}
