package jobs

import (
	"encoding/json"
	"errors"
	"github.com/example/autostream-worker/internal/encoder"
	"github.com/example/autostream-worker/internal/events"
	"github.com/example/autostream-worker/internal/observability"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

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

func TestManagerRetriesInitialParticipantsAfterEncoderReadiness(t *testing.T) {
	var attempts int
	pub := &fakePublisher{handler: func(event encoder.Event) error {
		attempts++
		if attempts <= 2 {
			return encoder.NewRetryablePublishError(http.StatusConflict, "http_status")
		}
		return nil
	}}
	manager := NewManager(pub, nil)
	if err := manager.Start(t.Context(), StreamContext{StreamID: "stream-01"}); err != nil {
		t.Fatal(err)
	}
	_, err := manager.Participants(t.Context(), "stream-01", []events.Participant{{UserID: "user-01", DisplayName: "Alice"}}, testTime())
	if err == nil {
		t.Fatal("initial participant publish unexpectedly succeeded")
	}
	waitForManager(t, time.Second, func() bool { return len(pub.snapshot()) == 1 })
	if attempts != 3 || pub.snapshot()[0].Type != "overlay.participants" {
		t.Fatalf("participants were not retried to success: attempts=%d events=%#v", attempts, pub.snapshot())
	}
}

func TestManagerCancelsRetryOnStopAndRejectsOldGeneration(t *testing.T) {
	pub := &fakePublisher{handler: func(encoder.Event) error {
		return encoder.NewRetryablePublishError(http.StatusServiceUnavailable, "http_status")
	}}
	manager := NewManager(pub, nil)
	if err := manager.Start(t.Context(), StreamContext{StreamID: "stream-01"}); err != nil {
		t.Fatal(err)
	}
	_, _ = manager.CurrentTime(t.Context(), "stream-01", testTime())
	if err := manager.Stop(t.Context(), "stream-01"); err != nil {
		t.Fatal(err)
	}
	if err := manager.Start(t.Context(), StreamContext{StreamID: "stream-01"}); err != nil {
		t.Fatal(err)
	}
	old := encoder.Event{ID: "old", StreamID: "stream-01", Generation: 1, Type: "overlay.current_time"}
	if manager.recordDeliveredWorkerEvent(old) {
		t.Fatal("old generation was accepted after rearm")
	}
	if got := len(pub.snapshot()); got != 0 {
		t.Fatalf("stale retry reached publisher after stop/rearm: %d", got)
	}
}

func TestManagerSupersedesPendingActiveSpeakerStartWithStop(t *testing.T) {
	var attempts []encoder.Event
	var mu sync.Mutex
	pub := &fakePublisher{handler: func(event encoder.Event) error {
		mu.Lock()
		attempts = append(attempts, event)
		count := len(attempts)
		mu.Unlock()
		if count == 1 {
			return encoder.NewRetryablePublishError(http.StatusConflict, "http_status")
		}
		return nil
	}}
	manager := NewManager(pub, nil)
	if err := manager.Start(t.Context(), StreamContext{StreamID: "stream-01"}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.ActiveSpeakerState(t.Context(), "stream-01", "user-01", "Alice", true, testTime()); err == nil {
		t.Fatal("expected active-speaker start to enter bounded retry")
	}
	if _, err := manager.ActiveSpeakerState(t.Context(), "stream-01", "user-01", "Alice", false, testTime().Add(time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	waitForManager(t, time.Second, func() bool { return len(pub.snapshot()) == 1 })
	mu.Lock()
	defer mu.Unlock()
	if len(attempts) != 2 || attempts[0].Payload["speaking"] != true || attempts[1].Payload["speaking"] != false {
		t.Fatalf("active-speaker stop did not supersede the pending start: attempts=%#v", attempts)
	}
	if pub.snapshot()[0].Payload["speaking"] != false {
		t.Fatalf("green-ring stop was not the converged event: %#v", pub.snapshot()[0])
	}
}

func TestManagerRetriesTransportFailure(t *testing.T) {
	attempts := 0
	pub := &fakePublisher{handler: func(encoder.Event) error {
		attempts++
		if attempts == 1 {
			return encoder.NewRetryablePublishError(0, "transport")
		}
		return nil
	}}
	manager := NewManager(pub, nil)
	if err := manager.Start(t.Context(), StreamContext{StreamID: "stream-01"}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.CustomOverlay(t.Context(), "stream-01", "overlay.discord_chat", map[string]any{"message_id": "msg-01", "author_id": "user-01", "content": "hello"}, testTime()); err == nil {
		t.Fatal("expected transport failure to be reported to the caller")
	}
	waitForManager(t, time.Second, func() bool { return len(pub.snapshot()) == 1 })
	if attempts != 2 || pub.snapshot()[0].Payload["message_id"] != "msg-01" {
		t.Fatalf("transport failure was not retried: attempts=%d events=%#v", attempts, pub.snapshot())
	}
}

func TestManagerStopsAtBoundedRetryLimit(t *testing.T) {
	var attempts atomic.Int32
	pub := &fakePublisher{handler: func(encoder.Event) error {
		attempts.Add(1)
		return encoder.NewRetryablePublishError(http.StatusServiceUnavailable, "http_status")
	}}
	manager := NewManager(pub, nil)
	if err := manager.Start(t.Context(), StreamContext{StreamID: "stream-01"}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Participants(t.Context(), "stream-01", []events.Participant{{UserID: "user-01"}}, testTime()); err == nil {
		t.Fatal("expected bounded delivery failure")
	}
	waitForManager(t, 3*time.Second, func() bool { return attempts.Load() >= maxWorkerEventAttempts })
	if got := attempts.Load(); got != maxWorkerEventAttempts {
		t.Fatalf("retry limit was not enforced: attempts=%d want=%d", got, maxWorkerEventAttempts)
	}
	time.Sleep(250 * time.Millisecond)
	if got := attempts.Load(); got != maxWorkerEventAttempts {
		t.Fatalf("retry continued past the bound: attempts=%d", got)
	}
}
