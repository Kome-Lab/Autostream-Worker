//go:build !windows

package control

import "syscall"

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
