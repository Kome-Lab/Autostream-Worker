package jobs

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/example/autostream-worker/internal/control"
	"github.com/example/autostream-worker/internal/encoder"
	"github.com/example/autostream-worker/internal/events"
	"github.com/example/autostream-worker/internal/observability"
)

type StreamContext struct {
	StreamID           string `json:"stream_id"`
	StreamName         string `json:"stream_name,omitempty"`
	EncoderRecorderURL string `json:"encoder_recorder_url,omitempty"`
	StreamIngestToken  string `json:"stream_ingest_token,omitempty"`
	OverlayProfileID   string `json:"overlay_profile_id,omitempty"`
	CaptionProfileID   string `json:"caption_profile_id,omitempty"`
}

type ProfileDefaults struct {
	OverlayProfileID string
	CaptionProfileID string
}

type AssignmentPolicy struct {
	Enforce        bool
	PrimaryStreams map[string]bool
}

type Status struct {
	CurrentStreamID string         `json:"current_stream_id,omitempty"`
	StreamName      string         `json:"stream_name,omitempty"`
	StartedAt       time.Time      `json:"started_at,omitempty"`
	EventCount      int            `json:"event_count"`
	EventCounts     map[string]int `json:"event_counts,omitempty"`
	SendFailures    int            `json:"event_send_failures_total"`
	LastEventAt     time.Time      `json:"last_event_at,omitempty"`
}

type Manager struct {
	publisher    encoder.Publisher
	reporter     observability.Client
	mu           sync.Mutex
	current      StreamContext
	defaults     ProfileDefaults
	assignments  AssignmentPolicy
	startedAt    time.Time
	events       []events.OverlayEvent
	eventCounts  map[string]int
	sendFailures int
	maxEvents    int
}

func NewManager(publisher encoder.Publisher, reporter observability.Client) *Manager {
	if publisher == nil {
		publisher = encoder.NoopPublisher{}
	}
	return &Manager{publisher: publisher, reporter: reporter, eventCounts: map[string]int{}, maxEvents: 200}
}

func (m *Manager) Start(ctx context.Context, stream StreamContext) error {
	stream = m.ApplyProfileDefaults(stream)
	if strings.TrimSpace(stream.StreamID) == "" {
		return errors.New("stream_id is required")
	}
	m.mu.Lock()
	if !m.streamAssignedLocked(stream.StreamID) {
		m.mu.Unlock()
		return errors.New("stream is not assigned to this worker service as primary")
	}
	if m.current.StreamID != "" && m.current.StreamID != stream.StreamID {
		m.mu.Unlock()
		return errors.New("another stream job is already active")
	}
	m.current = stream
	m.startedAt = time.Now().UTC()
	m.events = nil
	m.eventCounts = map[string]int{}
	m.sendFailures = 0
	m.mu.Unlock()
	m.report(ctx, stream.StreamID, "worker.job.started", "running", nil)
	return nil
}

func (m *Manager) SetAssignmentPolicy(policy AssignmentPolicy) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.assignments = AssignmentPolicy{Enforce: policy.Enforce, PrimaryStreams: map[string]bool{}}
	for streamID, allowed := range policy.PrimaryStreams {
		streamID = strings.TrimSpace(streamID)
		if streamID != "" && allowed {
			m.assignments.PrimaryStreams[streamID] = true
		}
	}
}

func (m *Manager) SetProfileDefaults(defaults ProfileDefaults) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.defaults = ProfileDefaults{
		OverlayProfileID: strings.TrimSpace(defaults.OverlayProfileID),
		CaptionProfileID: strings.TrimSpace(defaults.CaptionProfileID),
	}
}

func (m *Manager) ApplyRuntimeConfig(cfg control.RuntimeConfig) {
	m.SetProfileDefaults(ProfileDefaultsFromRuntimeConfig(cfg))
	m.SetAssignmentPolicy(AssignmentPolicyFromRuntimeConfig(cfg))
}

func ProfileDefaultsFromRuntimeConfig(cfg control.RuntimeConfig) ProfileDefaults {
	defaults := ProfileDefaults{}
	if profile, ok := firstRuntimeProfileForService(cfg.Profiles["overlay"], cfg.Service.ServiceID); ok {
		defaults.OverlayProfileID = profile.ID
	}
	if profile, ok := firstRuntimeProfileForService(cfg.Profiles["caption"], cfg.Service.ServiceID); ok {
		defaults.CaptionProfileID = profile.ID
	}
	return defaults
}

func AssignmentPolicyFromRuntimeConfig(cfg control.RuntimeConfig) AssignmentPolicy {
	policy := AssignmentPolicy{Enforce: true, PrimaryStreams: map[string]bool{}}
	serviceID := strings.TrimSpace(cfg.Service.ServiceID)
	for _, assignment := range cfg.Assignments {
		if assignment.ServiceType != control.ServiceType {
			continue
		}
		if strings.TrimSpace(assignment.ServiceID) != serviceID {
			continue
		}
		if assignment.AssignmentRole != "primary" {
			continue
		}
		streamID := strings.TrimSpace(assignment.StreamID)
		if streamID != "" {
			policy.PrimaryStreams[streamID] = true
		}
	}
	return policy
}

func firstRuntimeProfileForService(profiles []control.RuntimeProfile, serviceID string) (control.RuntimeProfile, bool) {
	for _, profile := range profiles {
		if profileBelongsToService(profile, serviceID) {
			return profile, true
		}
	}
	return control.RuntimeProfile{}, false
}

func profileBelongsToService(profile control.RuntimeProfile, serviceID string) bool {
	rawServiceID, ok := profile.Config["service_id"]
	if !ok {
		return true
	}
	profileServiceID, ok := rawServiceID.(string)
	if !ok {
		return false
	}
	profileServiceID = strings.TrimSpace(profileServiceID)
	return profileServiceID == "" || profileServiceID == strings.TrimSpace(serviceID)
}

func (m *Manager) ApplyProfileDefaults(stream StreamContext) StreamContext {
	m.mu.Lock()
	defaults := m.defaults
	m.mu.Unlock()
	if strings.TrimSpace(stream.OverlayProfileID) == "" {
		stream.OverlayProfileID = defaults.OverlayProfileID
	}
	if strings.TrimSpace(stream.CaptionProfileID) == "" {
		stream.CaptionProfileID = defaults.CaptionProfileID
	}
	return stream
}

func (m *Manager) Stop(ctx context.Context, streamID string) error {
	m.mu.Lock()
	if m.current.StreamID == "" {
		m.mu.Unlock()
		return errors.New("no active stream job")
	}
	if streamID != "" && streamID != m.current.StreamID {
		m.mu.Unlock()
		return errors.New("stream_id does not match current job")
	}
	stoppedStreamID := m.current.StreamID
	m.current = StreamContext{}
	m.startedAt = time.Time{}
	m.mu.Unlock()
	m.report(ctx, stoppedStreamID, "worker.job.stopped", "stopped", nil)
	return nil
}

func (m *Manager) CurrentStreamID() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.current.StreamID
}

func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	status := Status{EventCount: len(m.events), SendFailures: m.sendFailures}
	if len(m.eventCounts) > 0 {
		status.EventCounts = make(map[string]int, len(m.eventCounts))
		for name, count := range m.eventCounts {
			status.EventCounts[name] = count
		}
	}
	if m.current.StreamID != "" {
		status.CurrentStreamID = m.current.StreamID
		status.StreamName = m.current.StreamName
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
		"worker.event_send_failures_total": float64(status.SendFailures),
		"worker.scene_updates_total":       0,
		"worker.overlay_events_total":      0,
		"worker.caption_events_total":      0,
	}
	for name, count := range status.EventCounts {
		metrics[name] = float64(count)
	}
	return metrics
}

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

func (m *Manager) Caption(ctx context.Context, streamID, text, speakerUserID string, now time.Time) (events.OverlayEvent, error) {
	if strings.TrimSpace(text) == "" {
		return events.OverlayEvent{}, errors.New("caption text is required")
	}
	return m.publish(ctx, events.CaptionEvent(streamID, text, speakerUserID, now))
}

func (m *Manager) Participants(ctx context.Context, streamID string, participants []events.Participant, now time.Time) (events.OverlayEvent, error) {
	return m.publish(ctx, events.ParticipantListEvent(streamID, participants, now))
}

func (m *Manager) ActiveSpeaker(ctx context.Context, streamID, userID, displayName string, now time.Time) (events.OverlayEvent, error) {
	if strings.TrimSpace(userID) == "" {
		return events.OverlayEvent{}, errors.New("user_id is required")
	}
	return m.publish(ctx, events.ActiveSpeakerEvent(streamID, userID, displayName, now))
}

func (m *Manager) CustomOverlay(ctx context.Context, streamID, eventType string, payload map[string]any, now time.Time) (events.OverlayEvent, error) {
	if !strings.HasPrefix(eventType, "overlay.") && !strings.HasPrefix(eventType, "caption.") {
		return events.OverlayEvent{}, errors.New("event type must start with overlay. or caption.")
	}
	return m.publish(ctx, events.CustomOverlayEvent(streamID, eventType, payload, now))
}

func (m *Manager) publish(ctx context.Context, event events.OverlayEvent) (events.OverlayEvent, error) {
	m.mu.Lock()
	if err := m.ensureStreamLocked(event.StreamID); err != nil {
		m.mu.Unlock()
		return events.OverlayEvent{}, err
	}
	encoderRecorderURL := m.current.EncoderRecorderURL
	streamIngestToken := m.current.StreamIngestToken
	m.mu.Unlock()

	err := m.publisher.Publish(ctx, encoder.Event{
		ID:        event.ID,
		StreamID:  event.StreamID,
		Type:      event.Type,
		Payload:   event.Payload,
		Timestamp: event.Timestamp,
		URL:       encoderRecorderURL,
		Token:     streamIngestToken,
	})
	if err != nil {
		m.mu.Lock()
		m.sendFailures++
		failures := m.sendFailures
		m.mu.Unlock()
		attrs := map[string]any{"event_type": event.Type}
		m.report(ctx, event.StreamID, "worker.event.send_failed", "failed", attrs)
		m.metric(ctx, event.StreamID, "worker.event_send_failures_total", "warning", float64(failures), attrs)
		return events.OverlayEvent{}, errors.New("event publish failed")
	}

	m.mu.Lock()
	m.events = append(m.events, event)
	if len(m.events) > m.maxEvents {
		m.events = append([]events.OverlayEvent(nil), m.events[len(m.events)-m.maxEvents:]...)
	}
	metrics := m.recordEventLocked(event.Type)
	m.mu.Unlock()
	m.report(ctx, event.StreamID, "worker.event.sent", "sent", map[string]any{"event_type": event.Type})
	for metricName, value := range metrics {
		m.metric(ctx, event.StreamID, metricName, "ok", float64(value), map[string]any{"event_type": event.Type})
	}
	return event, nil
}

func (m *Manager) ensureStreamLocked(streamID string) error {
	if m.current.StreamID == "" {
		return errors.New("no active stream job")
	}
	if streamID == "" {
		return errors.New("stream_id is required")
	}
	if streamID != m.current.StreamID {
		return errors.New("stream_id does not match current job")
	}
	return nil
}

func (m *Manager) streamAssignedLocked(streamID string) bool {
	if !m.assignments.Enforce {
		return true
	}
	return m.assignments.PrimaryStreams[strings.TrimSpace(streamID)]
}

func (m *Manager) report(ctx context.Context, streamID, name, status string, attrs map[string]any) {
	if !m.reporter.Enabled() {
		return
	}
	_ = m.reporter.Event(ctx, streamID, name, status, attrs)
}

func (m *Manager) metric(ctx context.Context, streamID, name, status string, value float64, attrs map[string]any) {
	if !m.reporter.Enabled() {
		return
	}
	_ = m.reporter.Metric(ctx, streamID, name, status, value, attrs)
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
