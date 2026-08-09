package jobs

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	// DefaultStoppedTargetReceiptPath is inside the Worker systemd unit's
	// service-writable StateDirectory.
	DefaultStoppedTargetReceiptPath = "/var/lib/autostream/worker/stopped-target-receipts.json"

	stoppedTargetReceiptFileVersion = 1
	stoppedTargetReceiptTTL         = 24 * time.Hour
	stoppedTargetReceiptMaxBytes    = 64 << 10
)

type stoppedTargetReceipt struct {
	StreamID  string    `json:"stream_id"`
	StoppedAt time.Time `json:"stopped_at"`
}

type stoppedTargetReceiptState struct {
	Version  int                    `json:"version"`
	Receipts []stoppedTargetReceipt `json:"receipts"`
}

func (m *Manager) loadStoppedTargetReceipts() error {
	receipts, err := loadStoppedTargetReceipts(m.stoppedTargetReceiptPath, time.Now().UTC())
	if err != nil {
		return err
	}
	m.mu.Lock()
	m.setStoppedTargetReceiptsLocked(receipts)
	m.mu.Unlock()
	return nil
}

func loadStoppedTargetReceipts(path string, now time.Time) ([]stoppedTargetReceipt, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, nil
	}
	if err := validateStoppedTargetReceiptDirectory(filepath.Dir(path)); err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("inspect stopped target receipts: %w", err)
	}
	if err := validateStoppedTargetReceiptFileInfo(info); err != nil {
		return nil, err
	}

	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open stopped target receipts: %w", err)
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, stoppedTargetReceiptMaxBytes))
	decoder.DisallowUnknownFields()
	var state stoppedTargetReceiptState
	if err := decoder.Decode(&state); err != nil {
		return nil, fmt.Errorf("decode stopped target receipts: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, errors.New("stopped target receipts must contain one JSON object")
	}
	if state.Version != stoppedTargetReceiptFileVersion {
		return nil, fmt.Errorf("unsupported stopped target receipt version %d", state.Version)
	}

	cutoff := now.Add(-stoppedTargetReceiptTTL)
	seen := make(map[string]struct{}, len(state.Receipts))
	receipts := make([]stoppedTargetReceipt, 0, len(state.Receipts))
	for _, receipt := range state.Receipts {
		if receipt.StreamID == "" || strings.TrimSpace(receipt.StreamID) != receipt.StreamID {
			return nil, errors.New("stopped target receipt has an invalid stream_id")
		}
		if receipt.StoppedAt.IsZero() {
			return nil, errors.New("stopped target receipt is missing stopped_at")
		}
		if _, duplicate := seen[receipt.StreamID]; duplicate {
			return nil, errors.New("stopped target receipt has a duplicate stream_id")
		}
		seen[receipt.StreamID] = struct{}{}
		receipt.StoppedAt = receipt.StoppedAt.UTC()
		if receipt.StoppedAt.Before(cutoff) {
			continue
		}
		receipts = append(receipts, receipt)
	}
	if len(receipts) > maxStoppedStreamTargets {
		receipts = receipts[len(receipts)-maxStoppedStreamTargets:]
	}
	return receipts, nil
}

func persistStoppedTargetReceipts(path string, receipts []stoppedTargetReceipt) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil
	}
	directory := filepath.Dir(path)
	if err := validateStoppedTargetReceiptDirectory(directory); err != nil {
		return err
	}
	if info, err := os.Lstat(path); err == nil {
		if err := validateStoppedTargetReceiptFileInfo(info); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect stopped target receipts: %w", err)
	}

	payload, err := json.Marshal(stoppedTargetReceiptState{
		Version:  stoppedTargetReceiptFileVersion,
		Receipts: receipts,
	})
	if err != nil {
		return fmt.Errorf("encode stopped target receipts: %w", err)
	}
	payload = append(payload, '\n')
	temporary, err := os.CreateTemp(directory, ".stopped-target-receipts-*.tmp")
	if err != nil {
		return fmt.Errorf("create stopped target receipt temporary file: %w", err)
	}
	temporaryPath := temporary.Name()
	closed := false
	defer func() {
		if !closed {
			_ = temporary.Close()
		}
		_ = os.Remove(temporaryPath)
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return fmt.Errorf("protect stopped target receipt temporary file: %w", err)
	}
	if _, err := temporary.Write(payload); err != nil {
		return fmt.Errorf("write stopped target receipts: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("sync stopped target receipts: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close stopped target receipt temporary file: %w", err)
	}
	closed = true
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("activate stopped target receipts: %w", err)
	}
	if err := syncStoppedTargetReceiptDirectory(directory); err != nil {
		return err
	}
	return nil
}

func validateStoppedTargetReceiptDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect stopped target receipt directory: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("stopped target receipt directory must be a non-symlink directory")
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o022 != 0 {
		return errors.New("stopped target receipt directory must not be group or world writable")
	}
	return nil
}

func validateStoppedTargetReceiptFileInfo(info os.FileInfo) error {
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return errors.New("stopped target receipt path must be a regular non-symlink file")
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return errors.New("stopped target receipt file must not be accessible by group or world")
	}
	return nil
}

func syncStoppedTargetReceiptDirectory(path string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	directory, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open stopped target receipt directory for sync: %w", err)
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		return fmt.Errorf("sync stopped target receipt directory: %w", err)
	}
	return nil
}
