package jobs

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/example/autostream-worker/internal/encoder"
	"github.com/example/autostream-worker/internal/events"
)

func (m *Manager) RecentEvents(streamID string) ([]events.OverlayEvent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.ensureStreamLocked(streamID); err != nil {
		return nil, err
	}
	out := make([]events.OverlayEvent, len(m.events))
	copy(out, m.events)
	return out, nil
}

func (m *Manager) CurrentTime(ctx context.Context, streamID string, now time.Time) (events.OverlayEvent, error) {
	return m.publish(ctx, events.CurrentTimeEvent(streamID, now))
}

func (m *Manager) CurrentTimeForGeneration(ctx context.Context, streamID string, generation uint64, now time.Time) (events.OverlayEvent, error) {
	return m.publishWithGeneration(ctx, events.CurrentTimeEvent(streamID, now), generation)
}

func (m *Manager) Participants(ctx context.Context, streamID string, participants []events.Participant, now time.Time) (events.OverlayEvent, error) {
	return m.publish(ctx, events.ParticipantListEvent(streamID, participants, now))
}

func (m *Manager) ParticipantsForGeneration(ctx context.Context, streamID string, generation uint64, participants []events.Participant, now time.Time) (events.OverlayEvent, error) {
	return m.publishWithGeneration(ctx, events.ParticipantListEvent(streamID, participants, now), generation)
}

func (m *Manager) ActiveSpeaker(ctx context.Context, streamID, userID, displayName string, now time.Time) (events.OverlayEvent, error) {
	return m.ActiveSpeakerState(ctx, streamID, userID, displayName, true, now)
}

func (m *Manager) ActiveSpeakerState(ctx context.Context, streamID, userID, displayName string, speaking bool, now time.Time) (events.OverlayEvent, error) {
	return m.ActiveSpeakerStateForGeneration(ctx, streamID, 0, userID, displayName, speaking, now)
}

func (m *Manager) ActiveSpeakerStateForGeneration(ctx context.Context, streamID string, generation uint64, userID, displayName string, speaking bool, now time.Time) (events.OverlayEvent, error) {
	if speaking && strings.TrimSpace(userID) == "" {
		return events.OverlayEvent{}, errors.New("user_id is required")
	}
	return m.publishWithGeneration(ctx, events.ActiveSpeakerStateEvent(streamID, userID, displayName, speaking, now), generation)
}

func (m *Manager) CustomOverlay(ctx context.Context, streamID, eventType string, payload map[string]any, now time.Time) (events.OverlayEvent, error) {
	return m.CustomOverlayForGeneration(ctx, streamID, 0, eventType, payload, now)
}

func (m *Manager) CustomOverlayForGeneration(ctx context.Context, streamID string, generation uint64, eventType string, payload map[string]any, now time.Time) (events.OverlayEvent, error) {
	if !strings.HasPrefix(eventType, "overlay.") && !strings.HasPrefix(eventType, "caption.") {
		return events.OverlayEvent{}, errors.New("event type must start with overlay. or caption.")
	}
	return m.publishWithGeneration(ctx, events.CustomOverlayEvent(streamID, eventType, payload, now), generation)
}

func (m *Manager) publish(ctx context.Context, event events.OverlayEvent) (events.OverlayEvent, error) {
	return m.publishWithGeneration(ctx, event, 0)
}

func (m *Manager) publishWithGeneration(ctx context.Context, event events.OverlayEvent, expectedGeneration uint64) (events.OverlayEvent, error) {
	return m.publishWithGenerations(ctx, event, expectedGeneration, 0)
}

func (m *Manager) publishWithGenerations(ctx context.Context, event events.OverlayEvent, expectedGeneration, expectedCaptionSessionGeneration uint64) (events.OverlayEvent, error) {
	m.mu.Lock()
	if err := m.ensureStreamLocked(event.StreamID); err != nil {
		m.mu.Unlock()
		return events.OverlayEvent{}, err
	}
	if expectedGeneration != 0 && expectedGeneration != m.jobGeneration {
		m.mu.Unlock()
		return events.OverlayEvent{}, ErrJobGenerationMismatch
	}
	if expectedCaptionSessionGeneration != 0 && expectedCaptionSessionGeneration != m.captionSessionGeneration {
		m.mu.Unlock()
		return events.OverlayEvent{}, ErrCaptionSessionGenerationMismatch
	}
	if event.Type == "overlay.discord_chat" {
		_, hasLegacyUserID := event.Payload["user_id"]
		_, hasLegacyText := event.Payload["text"]
		// Reject key presence, including null, before replacing pending events
		// or applying the event to the local scene.
		if hasLegacyUserID || hasLegacyText {
			m.mu.Unlock()
			return events.OverlayEvent{}, ErrLegacyDiscordChatFields
		}
	}
	encoderRecorderURL := m.current.EncoderRecorderURL
	streamIngestToken := m.current.StreamIngestToken
	sceneRenderer := m.sceneRenderer
	generation := m.jobGeneration
	deliveryCtx := m.deliveryCtx
	m.mu.Unlock()
	if deliveryCtx == nil {
		deliveryCtx = ctx
	}
	m.supersedePendingWorkerEvent(event, generation)

	if sceneRenderer != nil {
		if err := sceneRenderer.Apply(generation, event); err != nil {
			m.report(ctx, event.StreamID, "worker.scene.apply_failed", "failed", map[string]any{"event_type": event.Type})
			return events.OverlayEvent{}, errors.New("scene event apply failed")
		}
	}

	encoderEvent := encoder.Event{
		ID:         event.ID,
		StreamID:   event.StreamID,
		Type:       event.Type,
		Payload:    event.Payload,
		Timestamp:  event.Timestamp,
		URL:        encoderRecorderURL,
		Token:      streamIngestToken,
		Generation: generation,
		Attempt:    1,
	}
	m.publisherMu.Lock()
	err := m.publisher.Publish(deliveryCtx, encoderEvent)
	m.publisherMu.Unlock()
	if err != nil {
		class, status := encoder.PublishErrorMetadata(err)
		retryable := encoder.IsRetryablePublishError(err)
		m.recordWorkerEventFailure(ctx, encoderEvent, 1, retryable, class, status)
		m.queueWorkerEventAfterFailure(encoderEvent, err)
		return events.OverlayEvent{}, errors.New("event publish failed")
	}
	if !m.recordDeliveredWorkerEvent(encoderEvent) {
		return events.OverlayEvent{}, errors.New("stream job changed while publishing event")
	}
	return event, nil
}

func (m *Manager) recordEventLocked(eventType string) map[string]int {
	if m.eventCounts == nil {
		m.eventCounts = map[string]int{}
	}
	metrics := map[string]int{}
	if strings.HasPrefix(eventType, "caption.") {
		m.eventCounts["worker.caption_events_total"]++
		metrics["worker.caption_events_total"] = m.eventCounts["worker.caption_events_total"]
	}
	if strings.HasPrefix(eventType, "overlay.") {
		m.eventCounts["worker.overlay_events_total"]++
		metrics["worker.overlay_events_total"] = m.eventCounts["worker.overlay_events_total"]
	}
	m.eventCounts["worker.scene_updates_total"]++
	metrics["worker.scene_updates_total"] = m.eventCounts["worker.scene_updates_total"]
	return metrics
}
