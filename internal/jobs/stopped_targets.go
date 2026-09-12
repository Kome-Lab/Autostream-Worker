package jobs

import (
	"strings"
	"time"
)

func (m *Manager) wasStoppedTargetLocked(streamID string) bool {
	cutoff := time.Now().UTC().Add(-stoppedTargetReceiptTTL)
	for _, receipt := range m.stoppedOrder {
		if receipt.StreamID == streamID {
			return !receipt.StoppedAt.Before(cutoff)
		}
	}
	return false
}

func (m *Manager) rememberStoppedTargetLocked(streamID string) error {
	if streamID == "" {
		return nil
	}
	receipts := m.withoutStoppedTargetLocked(streamID)
	receipts = append(receipts, stoppedTargetReceipt{StreamID: streamID, StoppedAt: time.Now().UTC()})
	if len(receipts) > maxStoppedStreamTargets {
		receipts = receipts[len(receipts)-maxStoppedStreamTargets:]
	}
	if err := persistStoppedTargetReceipts(m.stoppedTargetReceiptPath, receipts); err != nil {
		return err
	}
	m.setStoppedTargetReceiptsLocked(receipts)
	return nil
}

func (m *Manager) rememberStoppedTargetInMemoryLocked(streamID string, stoppedAt time.Time) {
	streamID = strings.TrimSpace(streamID)
	if streamID == "" {
		return
	}
	receipts := m.withoutStoppedTargetLocked(streamID)
	receipts = append(receipts, stoppedTargetReceipt{StreamID: streamID, StoppedAt: stoppedAt.UTC()})
	if len(receipts) > maxStoppedStreamTargets {
		receipts = receipts[len(receipts)-maxStoppedStreamTargets:]
	}
	m.setStoppedTargetReceiptsLocked(receipts)
}

func (m *Manager) forgetStoppedTargetLocked(streamID string) error {
	if !m.wasStoppedTargetLocked(streamID) {
		return nil
	}
	receipts := m.withoutStoppedTargetLocked(streamID)
	if err := persistStoppedTargetReceipts(m.stoppedTargetReceiptPath, receipts); err != nil {
		return err
	}
	m.setStoppedTargetReceiptsLocked(receipts)
	return nil
}

func (m *Manager) withoutStoppedTargetLocked(streamID string) []stoppedTargetReceipt {
	receipts := make([]stoppedTargetReceipt, 0, len(m.stoppedOrder))
	cutoff := time.Now().UTC().Add(-stoppedTargetReceiptTTL)
	for _, receipt := range m.stoppedOrder {
		if receipt.StreamID != streamID && !receipt.StoppedAt.Before(cutoff) {
			receipts = append(receipts, receipt)
		}
	}
	return receipts
}

func (m *Manager) setStoppedTargetReceiptsLocked(receipts []stoppedTargetReceipt) {
	m.stoppedOrder = append([]stoppedTargetReceipt(nil), receipts...)
}
