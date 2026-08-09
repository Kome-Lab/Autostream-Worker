package jobs

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadStoppedTargetReceiptsFiltersExpiredAndBoundsTargets(t *testing.T) {
	now := time.Date(2026, time.August, 9, 12, 0, 0, 0, time.UTC)
	state := stoppedTargetReceiptState{Version: stoppedTargetReceiptFileVersion}
	state.Receipts = append(state.Receipts, stoppedTargetReceipt{
		StreamID:  "expired",
		StoppedAt: now.Add(-stoppedTargetReceiptTTL - time.Second),
	})
	for index := 0; index <= maxStoppedStreamTargets; index++ {
		state.Receipts = append(state.Receipts, stoppedTargetReceipt{
			StreamID:  fmt.Sprintf("stream-%d", index),
			StoppedAt: now.Add(time.Duration(index) * time.Second),
		})
	}
	payload, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "stopped-target-receipts.json")
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatal(err)
	}

	receipts, err := loadStoppedTargetReceipts(path, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(receipts) != maxStoppedStreamTargets {
		t.Fatalf("receipt count = %d, want %d", len(receipts), maxStoppedStreamTargets)
	}
	if receipts[0].StreamID != "stream-1" || receipts[len(receipts)-1].StreamID != "stream-64" {
		t.Fatalf("unexpected bounded receipt range: first=%q last=%q", receipts[0].StreamID, receipts[len(receipts)-1].StreamID)
	}
}

func TestManagerExpiresStoppedTargetReceiptDuringLongRunningProcess(t *testing.T) {
	manager := NewManager(nil, nil)
	manager.stoppedOrder = []stoppedTargetReceipt{{
		StreamID:  "expired",
		StoppedAt: time.Now().UTC().Add(-stoppedTargetReceiptTTL - time.Second),
	}}
	if err := manager.Stop(t.Context(), "expired"); !errors.Is(err, ErrNoActiveStreamJob) {
		t.Fatalf("expired target stop error = %v, want %v", err, ErrNoActiveStreamJob)
	}
}
