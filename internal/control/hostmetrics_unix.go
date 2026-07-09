//go:build !windows

package control

import (
	"os"
	"strconv"
	"strings"
	"syscall"
)

func filesystemMetrics() map[string]float64 {
	var stat syscall.Statfs_t
	if err := syscall.Statfs("/", &stat); err != nil {
		return nil
	}
	blockSize := float64(stat.Bsize)
	total := float64(stat.Blocks) * blockSize
	free := float64(stat.Bavail) * blockSize
	if total <= 0 {
		return nil
	}
	used := total - free
	return map[string]float64{
		"node.filesystem.root.size_bytes":    total,
		"node.filesystem.root.free_bytes":    free,
		"node.filesystem.root.used_bytes":    used,
		"node.filesystem.root.used_percent":  used / total * 100,
		"node.filesystem.root.files":         float64(stat.Files),
		"node.filesystem.root.files_free":    float64(stat.Ffree),
		"node.filesystem.root.files_percent": inodeUsedPercent(stat.Files, stat.Ffree),
	}
}

func inodeUsedPercent(files, free uint64) float64 {
	if files == 0 {
		return 0
	}
	used := files - free
	return float64(used) / float64(files) * 100
}

func networkByteCounters() map[string]float64 {
	data, err := os.ReadFile("/proc/net/dev")
	if err != nil {
		return nil
	}
	var rxTotal, txTotal float64
	for _, line := range strings.Split(string(data), "\n") {
		parts := strings.Split(line, ":")
		if len(parts) != 2 {
			continue
		}
		iface := strings.TrimSpace(parts[0])
		if iface == "" || iface == "lo" {
			continue
		}
		fields := strings.Fields(parts[1])
		if len(fields) < 16 {
			continue
		}
		rx, rxErr := strconv.ParseFloat(fields[0], 64)
		tx, txErr := strconv.ParseFloat(fields[8], 64)
		if rxErr == nil {
			rxTotal += rx
		}
		if txErr == nil {
			txTotal += tx
		}
	}
	if rxTotal == 0 && txTotal == 0 {
		return nil
	}
	return map[string]float64{"rx": rxTotal, "tx": txTotal}
}
