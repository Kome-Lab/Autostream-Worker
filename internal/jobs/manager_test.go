package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/example/autostream-worker/internal/encoder"
	"github.com/example/autostream-worker/internal/observability"
)

type fakePublisher struct {
	events []encoder.Event
	err    error
}

func (f *fakePublisher) Publish(ctx context.Context, event encoder.Event) error {
	if f.err != nil {
		return f.err
	}
	f.events = append(f.events, event)
	return nil
}

func TestManagerStartsAndPublishesEvent(t *testing.T) {
	pub := &fakePublisher{}
	manager := NewManager(pub, observability.Client{})
	if err := manager.Start(t.Context(), StreamContext{StreamID: "stream-01", StreamName: "Test"}); err != nil {
		t.Fatal(err)
	}
	ev, err := manager.CurrentTime(t.Context(), "stream-01", testTime())
	if err != nil {
		t.Fatal(err)
	}
	if ev.Type != "overlay.current_time" || len(pub.events) != 1 || pub.events[0].Type != "overlay.current_time" {
		t.Fatalf("event was not published: ev=%#v published=%#v", ev, pub.events)
	}
	if manager.Status().EventCount != 1 {
		t.Fatalf("unexpected status: %#v", manager.Status())
	}
	if manager.Status().EventCounts["worker.overlay_events_total"] != 1 || manager.Status().EventCounts["worker.scene_updates_total"] != 1 {
		t.Fatalf("unexpected event counters: %#v", manager.Status().EventCounts)
	}
	metrics := manager.Metrics()
	if metrics["worker.overlay_events_total"] != 1 || metrics["worker.scene_updates_total"] != 1 || metrics["worker.event_send_failures_total"] != 0 {
		t.Fatalf("unexpected metrics: %#v", metrics)
	}
}

func TestManagerForwardsJobScopedEncoderRoute(t *testing.T) {
	pub := &fakePublisher{}
	manager := NewManager(pub, observability.Client{})
	if err := manager.Start(t.Context(), StreamContext{
		StreamID:           "stream-01",
		EncoderRecorderURL: "https://encoder.example.com",
		StreamIngestToken:  "signed-job-token",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.CurrentTime(t.Context(), "stream-01", testTime()); err != nil {
		t.Fatal(err)
	}
	if len(pub.events) != 1 || pub.events[0].URL != "https://encoder.example.com" || pub.events[0].Token != "signed-job-token" {
		t.Fatalf("job-scoped encoder route was not forwarded: %#v", pub.events)
	}
}

func TestManagerStartAppliesProfileDefaults(t *testing.T) {
	manager := NewManager(&fakePublisher{}, observability.Client{})
	manager.SetProfileDefaults(ProfileDefaults{OverlayProfileID: "overlay-default", CaptionProfileID: "caption-default"})
	if err := manager.Start(t.Context(), StreamContext{StreamID: "stream-01"}); err != nil {
		t.Fatal(err)
	}
	status := manager.Status()
	if status.CurrentStreamID != "stream-01" {
		t.Fatalf("unexpected status: %#v", status)
	}
	applied := manager.ApplyProfileDefaults(StreamContext{StreamID: "stream-02"})
	if applied.OverlayProfileID != "overlay-default" || applied.CaptionProfileID != "caption-default" {
		t.Fatalf("profile defaults were not applied: %#v", applied)
	}
	explicit := manager.ApplyProfileDefaults(StreamContext{StreamID: "stream-03", OverlayProfileID: "overlay-explicit"})
	if explicit.OverlayProfileID != "overlay-explicit" || explicit.CaptionProfileID != "caption-default" {
		t.Fatalf("explicit profile should not be overwritten: %#v", explicit)
	}
}

func TestManagerStartRequiresPrimaryAssignmentWhenPolicyEnforced(t *testing.T) {
	manager := NewManager(&fakePublisher{}, observability.Client{})
	manager.SetAssignmentPolicy(AssignmentPolicy{
		Enforce:        true,
		PrimaryStreams: map[string]bool{"stream-01": true},
	})
	if err := manager.Start(t.Context(), StreamContext{StreamID: "stream-02"}); err == nil {
		t.Fatal("expected unassigned stream to be rejected")
	}
	if err := manager.Start(t.Context(), StreamContext{StreamID: "stream-01"}); err != nil {
		t.Fatalf("expected assigned primary stream to start: %v", err)
	}
}

func TestManagerRejectsWrongStream(t *testing.T) {
	manager := NewManager(&fakePublisher{}, observability.Client{})
	if err := manager.Start(t.Context(), StreamContext{StreamID: "stream-01"}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.CurrentTime(t.Context(), "stream-02", testTime()); err == nil {
		t.Fatal("expected wrong stream to be rejected")
	}
}

func TestCustomOverlayRequiresKnownPrefix(t *testing.T) {
	manager := NewManager(&fakePublisher{}, observability.Client{})
	if err := manager.Start(t.Context(), StreamContext{StreamID: "stream-01"}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.CustomOverlay(t.Context(), "stream-01", "bad.event", nil, testTime()); err == nil {
		t.Fatal("expected bad event type to be rejected")
	}
}

func TestManagerReportsPublishFailure(t *testing.T) {
	var signals []observability.Signal
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var signal observability.Signal
		if err := json.NewDecoder(r.Body).Decode(&signal); err != nil {
			t.Fatal(err)
		}
		signals = append(signals, signal)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()

	manager := NewManager(&fakePublisher{err: errors.New("encoder unavailable")}, observability.Client{
		Config: observability.Config{URL: server.URL, Token: "obs-token", ServiceID: "worker-01", Timeout: time.Second},
	})
	if err := manager.Start(t.Context(), StreamContext{StreamID: "stream-01"}); err != nil {
		t.Fatal(err)
	}
	_, err := manager.CurrentTime(t.Context(), "stream-01", testTime())
	if err == nil || err.Error() != "event publish failed" {
		t.Fatalf("unexpected publish error: %v", err)
	}
	status := manager.Status()
	if status.EventCount != 0 || status.SendFailures != 1 {
		t.Fatalf("unexpected status after publish failure: %#v", status)
	}
	if len(signals) < 3 {
		t.Fatalf("expected start, failure event and failure metric signals: %#v", signals)
	}
	var sawFailureEvent, sawFailureMetric bool
	for _, signal := range signals {
		if signal.Name == "worker.event.send_failed" && signal.Status == "failed" {
			sawFailureEvent = true
		}
		if signal.Name == "worker.event_send_failures_total" && signal.Type == "metric" && signal.Value != nil && *signal.Value == 1 {
			sawFailureMetric = true
		}
	}
	if !sawFailureEvent || !sawFailureMetric {
		t.Fatalf("missing failure observability signals: %#v", signals)
	}
}

func testTime() time.Time {
	return time.Date(2026, 5, 28, 0, 0, 0, 0, time.UTC)
}
