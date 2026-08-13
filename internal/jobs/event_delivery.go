package jobs

import (
	"context"
	"strings"
	"time"

	"github.com/example/autostream-worker/internal/encoder"
	"github.com/example/autostream-worker/internal/events"
)

const (
	maxPendingWorkerEvents = 256
	maxWorkerEventAttempts = 5
	workerEventRetryBase   = 100 * time.Millisecond
	workerEventRetryMax    = 2 * time.Second
)

type pendingWorkerEvent struct {
	event     encoder.Event
	key       string
	attempts  int
	nextTryAt time.Time
	queuedAt  time.Time
}

func (m *Manager) startEventDeliveryLocked() {
	if m.deliveryCancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.deliveryCtx = ctx
	m.deliveryCancel = cancel
	m.deliveryWake = make(chan struct{}, 1)
	if m.pendingEvents == nil {
		m.pendingEvents = map[string]pendingWorkerEvent{}
	}
	m.deliveryWG.Add(1)
	go m.eventDeliveryLoop(ctx, m.deliveryWake)
}

func (m *Manager) stopEventDelivery() {
	m.mu.Lock()
	cancel := m.deliveryCancel
	m.deliveryCancel = nil
	m.deliveryCtx = nil
	m.pendingEvents = map[string]pendingWorkerEvent{}
	m.latestEventByKey = map[string]string{}
	wake := m.deliveryWake
	m.deliveryWake = nil
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if wake != nil {
		select {
		case wake <- struct{}{}:
		default:
		}
	}
	m.deliveryWG.Wait()
}

func (m *Manager) enqueueWorkerEvent(event encoder.Event) bool {
	key := workerEventKey(event)
	now := time.Now().UTC()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.current.StreamID != event.StreamID || m.jobGeneration != event.Generation || m.deliveryCancel == nil {
		return false
	}
	if m.pendingEvents == nil {
		m.pendingEvents = map[string]pendingWorkerEvent{}
	}
	if m.latestEventByKey == nil {
		m.latestEventByKey = map[string]string{}
	}
	m.latestEventByKey[key] = event.ID
	if _, exists := m.pendingEvents[key]; !exists && len(m.pendingEvents) >= maxPendingWorkerEvents {
		if !m.dropOldestPendingLocked() {
			return false
		}
	}
	event.Attempt = 1
	m.pendingEvents[key] = pendingWorkerEvent{event: event, key: key, attempts: 1, nextTryAt: now.Add(workerEventRetryBase), queuedAt: now}
	if m.deliveryWake != nil {
		select {
		case m.deliveryWake <- struct{}{}:
		default:
		}
	}
	return true
}

func (m *Manager) supersedePendingWorkerEvent(event events.OverlayEvent, generation uint64) {
	key := workerEventKey(encoder.Event{StreamID: event.StreamID, Type: event.Type, Payload: event.Payload, ID: event.ID, Generation: generation})
	m.mu.Lock()
	if m.jobGeneration == generation && m.current.StreamID == event.StreamID {
		if m.latestEventByKey == nil {
			m.latestEventByKey = map[string]string{}
		}
		m.latestEventByKey[key] = event.ID
		delete(m.pendingEvents, key)
	}
	m.mu.Unlock()
}

func (m *Manager) queueWorkerEventAfterFailure(event encoder.Event, err error) bool {
	if !encoder.IsRetryablePublishError(err) {
		return false
	}
	// The error is intentionally not stored. Only its safe classification is
	// reported by recordWorkerEventFailure.
	return m.enqueueWorkerEvent(event)
}

func workerEventKey(event encoder.Event) string {
	stream := strings.TrimSpace(event.StreamID)
	switch event.Type {
	case "overlay.participants":
		return stream + "\x00" + "participants"
	case "overlay.active_speaker":
		return stream + "\x00" + "active_speaker"
	case "overlay.discord_chat":
		if id, ok := event.Payload["message_id"].(string); ok && strings.TrimSpace(id) != "" {
			return stream + "\x00chat\x00" + strings.TrimSpace(id)
		}
	case "caption.telop", "caption.final":
		if id, ok := event.Payload["utterance_id"].(string); ok && strings.TrimSpace(id) != "" {
			return stream + "\x00caption\x00" + strings.TrimSpace(id)
		}
	}
	return stream + "\x00event\x00" + strings.TrimSpace(event.ID)
}

func (m *Manager) dropOldestPendingLocked() bool {
	oldestKey := ""
	var oldest time.Time
	for key, pending := range m.pendingEvents {
		if pending.event.Type == "overlay.participants" || pending.event.Type == "overlay.active_speaker" {
			continue
		}
		if oldestKey == "" || pending.queuedAt.Before(oldest) {
			oldestKey, oldest = key, pending.queuedAt
		}
	}
	if oldestKey == "" {
		return false
	}
	delete(m.pendingEvents, oldestKey)
	return true
}

func (m *Manager) eventDeliveryLoop(ctx context.Context, wake <-chan struct{}) {
	defer m.deliveryWG.Done()
	for {
		pending, ready, wait := m.nextPendingWorkerEvent()
		if ready {
			m.deliverPendingWorkerEvent(ctx, pending)
			continue
		}
		if wait > 0 {
			timer := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-wake:
				timer.Stop()
			case <-timer.C:
			}
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-wake:
		}
	}
}

func (m *Manager) nextPendingWorkerEvent() (pendingWorkerEvent, bool, time.Duration) {
	now := time.Now().UTC()
	m.mu.Lock()
	defer m.mu.Unlock()
	var selected pendingWorkerEvent
	var next time.Time
	for _, pending := range m.pendingEvents {
		if pending.nextTryAt.IsZero() || !pending.nextTryAt.After(now) {
			if selected.key == "" || pending.queuedAt.Before(selected.queuedAt) {
				selected = pending
			}
			continue
		}
		if next.IsZero() || pending.nextTryAt.Before(next) {
			next = pending.nextTryAt
		}
	}
	if selected.key != "" {
		delete(m.pendingEvents, selected.key)
		return selected, true, 0
	}
	if !next.IsZero() {
		return pendingWorkerEvent{}, false, time.Until(next)
	}
	return pendingWorkerEvent{}, false, 0
}

func (m *Manager) deliverPendingWorkerEvent(ctx context.Context, pending pendingWorkerEvent) {
	if !m.isCurrentPendingEvent(pending) {
		return
	}
	pending.attempts++
	pending.event.Attempt = uint32(pending.attempts)
	m.publisherMu.Lock()
	if !m.isCurrentPendingEvent(pending) {
		m.publisherMu.Unlock()
		return
	}
	err := m.publisher.Publish(ctx, pending.event)
	m.publisherMu.Unlock()
	if err == nil {
		m.recordDeliveredWorkerEvent(pending.event)
		return
	}
	class, status := encoder.PublishErrorMetadata(err)
	retryable := encoder.IsRetryablePublishError(err)
	m.recordWorkerEventFailure(ctx, pending.event, pending.attempts, retryable, class, status)
	if !retryable || pending.attempts >= maxWorkerEventAttempts || ctx.Err() != nil {
		return
	}
	delay := workerEventRetryBase * time.Duration(1<<(pending.attempts-1))
	if delay > workerEventRetryMax {
		delay = workerEventRetryMax
	}
	pending.nextTryAt = time.Now().UTC().Add(delay)
	m.mu.Lock()
	if m.current.StreamID == pending.event.StreamID && m.jobGeneration == pending.event.Generation && m.deliveryCancel != nil {
		if _, newer := m.pendingEvents[pending.key]; !newer {
			m.pendingEvents[pending.key] = pending
		}
	}
	m.mu.Unlock()
}

func (m *Manager) isCurrentPendingEvent(pending pendingWorkerEvent) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.current.StreamID == pending.event.StreamID && m.jobGeneration == pending.event.Generation && m.latestEventByKey[pending.key] == pending.event.ID && m.deliveryCancel != nil
}

func (m *Manager) recordDeliveredWorkerEvent(event encoder.Event) bool {
	m.mu.Lock()
	if m.current.StreamID != event.StreamID || m.jobGeneration != event.Generation {
		m.mu.Unlock()
		return false
	}
	m.events = append(m.events, events.OverlayEvent{ID: event.ID, StreamID: event.StreamID, Type: event.Type, Payload: event.Payload, Timestamp: event.Timestamp})
	if len(m.events) > m.maxEvents {
		m.events = append([]events.OverlayEvent(nil), m.events[len(m.events)-m.maxEvents:]...)
	}
	metrics := m.recordEventLocked(event.Type)
	m.mu.Unlock()
	attributes := map[string]any{"event_type": event.Type, "attempt": event.Attempt}
	m.report(context.Background(), event.StreamID, "worker.event.sent", "sent", attributes)
	for metricName, value := range metrics {
		m.metric(context.Background(), event.StreamID, metricName, "ok", float64(value), attributes)
	}
	return true
}

func (m *Manager) recordWorkerEventFailure(ctx context.Context, event encoder.Event, attempt int, retryable bool, class string, status int) {
	m.mu.Lock()
	m.sendFailures++
	failures := m.sendFailures
	m.mu.Unlock()
	attributes := map[string]any{
		"event_type":  event.Type,
		"attempt":     attempt,
		"retryable":   retryable,
		"error_class": class,
	}
	if status > 0 {
		attributes["http_status"] = status
	}
	reportCtx := ctx
	if reportCtx == nil || reportCtx.Err() != nil {
		reportCtx = context.Background()
	}
	m.report(reportCtx, event.StreamID, "worker.event.send_failed", "failed", attributes)
	m.metric(reportCtx, event.StreamID, "worker.event_send_failures_total", "warning", float64(failures), attributes)
}
