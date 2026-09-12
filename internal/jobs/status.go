package jobs

import (
	"context"
	"time"
)

type Status struct {
	CurrentStreamID                   string         `json:"current_stream_id,omitempty"`
	StreamName                        string         `json:"stream_name,omitempty"`
	JobGeneration                     uint64         `json:"job_generation,omitempty"`
	StartedAt                         time.Time      `json:"started_at,omitempty"`
	EventCount                        int            `json:"event_count"`
	EventCounts                       map[string]int `json:"event_counts,omitempty"`
	SendFailures                      int            `json:"event_send_failures_total"`
	CaptionAudioQueueBatches          int            `json:"caption_audio_queue_batches"`
	CaptionAudioQueuePackets          int            `json:"caption_audio_queue_packets"`
	CaptionAudioQueueBytes            int            `json:"caption_audio_queue_bytes"`
	CaptionAudioQueueDrops            int            `json:"caption_audio_queue_drops_total"`
	CaptionAudioRetries               int            `json:"caption_audio_retries_total"`
	CaptionAudioProviderDrops         int            `json:"caption_audio_provider_drops_total"`
	CaptionAudioConnectionGeneration  uint64         `json:"caption_audio_connection_generation"`
	CaptionAudioSupersededDrops       int            `json:"caption_audio_superseded_drops_total"`
	CaptionSessionActive              bool           `json:"caption_session_active"`
	CaptionSessionGeneration          uint64         `json:"caption_session_generation,omitempty"`
	CaptionProviderConnections        int            `json:"caption_provider_connections"`
	CaptionProviderAudioPackets       uint64         `json:"caption_provider_audio_packets_total"`
	CaptionProviderTranscriptMessages uint64         `json:"caption_provider_transcript_messages_total"`
	CaptionProviderErrors             uint64         `json:"caption_provider_errors_total"`
	CaptionProviderLastErrorClass     string         `json:"caption_provider_last_error_class,omitempty"`
	CaptionProviderLastHTTPStatus     int            `json:"caption_provider_last_http_status,omitempty"`
	LastEventAt                       time.Time      `json:"last_event_at,omitempty"`
}

type Reporter interface {
	Event(ctx context.Context, streamID, name, status string, attributes map[string]any) error
	Metric(ctx context.Context, streamID, name, status string, value float64, attributes map[string]any) error
}

func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	status := Status{
		EventCount:                       len(m.events),
		SendFailures:                     m.sendFailures,
		CaptionAudioQueueDrops:           m.captionAudioQueueDrops,
		CaptionAudioRetries:              m.captionAudioRetries,
		CaptionAudioProviderDrops:        m.captionAudioProviderDrops,
		CaptionAudioConnectionGeneration: m.captionAudioConnectionGeneration,
		CaptionAudioSupersededDrops:      m.captionAudioSupersededDrops,
		CaptionSessionActive:             m.captionSession != nil,
		CaptionSessionGeneration:         m.captionSessionGeneration,
	}
	if provider, ok := m.captionSession.(captionSessionStatusProvider); ok {
		providerStatus := provider.Status()
		status.CaptionProviderConnections = providerStatus.ActiveConnections
		status.CaptionProviderAudioPackets = providerStatus.AudioPacketsSent
		status.CaptionProviderTranscriptMessages = providerStatus.TranscriptMessages
		status.CaptionProviderErrors = providerStatus.ProviderErrors
		status.CaptionProviderLastErrorClass = providerStatus.LastErrorClass
		status.CaptionProviderLastHTTPStatus = providerStatus.LastHTTPStatus
	}
	if m.captionIngress != nil {
		status.CaptionAudioQueueBatches, status.CaptionAudioQueuePackets, status.CaptionAudioQueueBytes = m.captionIngress.snapshot()
	}
	if len(m.eventCounts) > 0 {
		status.EventCounts = make(map[string]int, len(m.eventCounts))
		for name, count := range m.eventCounts {
			status.EventCounts[name] = count
		}
	}
	if m.current.StreamID != "" {
		status.CurrentStreamID = m.current.StreamID
		status.StreamName = m.current.StreamName
		status.JobGeneration = m.jobGeneration
		status.StartedAt = m.startedAt
	}
	if len(m.events) > 0 {
		status.LastEventAt = m.events[len(m.events)-1].Timestamp
	}
	return status
}

func (m *Manager) Metrics() map[string]float64 {
	status := m.Status()
	metrics := map[string]float64{
		"worker.event_send_failures_total":            float64(status.SendFailures),
		"worker.scene_updates_total":                  0,
		"worker.overlay_events_total":                 0,
		"worker.caption_events_total":                 0,
		"worker.caption_audio_queue_batches":          float64(status.CaptionAudioQueueBatches),
		"worker.caption_audio_queue_packets":          float64(status.CaptionAudioQueuePackets),
		"worker.caption_audio_queue_bytes":            float64(status.CaptionAudioQueueBytes),
		"worker.caption_audio_queue_drops_total":      float64(status.CaptionAudioQueueDrops),
		"worker.caption_audio_retries_total":          float64(status.CaptionAudioRetries),
		"worker.caption_audio_provider_drops_total":   float64(status.CaptionAudioProviderDrops),
		"worker.caption_audio_connection_generation":  float64(status.CaptionAudioConnectionGeneration),
		"worker.caption_audio_superseded_drops_total": float64(status.CaptionAudioSupersededDrops),
		"worker.caption_session_active":               boolMetric(status.CaptionSessionActive),
		"worker.caption_provider_connections":         float64(status.CaptionProviderConnections),
		"worker.caption_provider_audio_packets_total": float64(status.CaptionProviderAudioPackets),
		"worker.caption_provider_transcripts_total":   float64(status.CaptionProviderTranscriptMessages),
		"worker.caption_provider_errors_total":        float64(status.CaptionProviderErrors),
	}
	for name, count := range status.EventCounts {
		metrics[name] = float64(count)
	}
	return metrics
}

func boolMetric(value bool) float64 {
	if value {
		return 1
	}
	return 0
}

func (m *Manager) report(ctx context.Context, streamID, name, status string, attrs map[string]any) {
	if m.reporter == nil {
		return
	}
	_ = m.reporter.Event(ctx, streamID, name, status, attrs)
}

func (m *Manager) metric(ctx context.Context, streamID, name, status string, value float64, attrs map[string]any) {
	if m.reporter == nil {
		return
	}
	_ = m.reporter.Metric(ctx, streamID, name, status, value, attrs)
}
