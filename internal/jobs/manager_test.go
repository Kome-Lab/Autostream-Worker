package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/example/autostream-worker/internal/control"
	"github.com/example/autostream-worker/internal/deepgram"
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

type fakeCaptionSession struct {
	packets      []deepgram.AudioPacket
	handler      deepgram.Handler
	closed       int
	err          error
	ingestCalls  int
	ingestErrors []error
}

func (s *fakeCaptionSession) Ingest(_ context.Context, packet deepgram.AudioPacket) error {
	call := s.ingestCalls
	s.ingestCalls++
	if call < len(s.ingestErrors) && s.ingestErrors[call] != nil {
		return s.ingestErrors[call]
	}
	if s.err != nil {
		return s.err
	}
	s.packets = append(s.packets, packet)
	return nil
}

func (s *fakeCaptionSession) Close(context.Context) error {
	s.closed++
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

func TestManagerStartsSelectedCaptionProfileAndPublishesDeepgramResults(t *testing.T) {
	pub := &fakePublisher{}
	manager := NewManager(pub, observability.Client{})
	resolveCalls := 0
	var resolvedStreamID string
	var resolvedName string
	resolver := RuntimeSecretResolverFunc(func(_ context.Context, streamID, secretName string) (control.RuntimeSecret, error) {
		resolveCalls++
		resolvedStreamID = streamID
		resolvedName = secretName
		return control.RuntimeSecret{SecretName: secretName, Value: "dg-runtime-key", ExpiresInSec: 300}, nil
	})
	session := &fakeCaptionSession{}
	var gotConfig deepgram.Config
	var transientKey []byte
	factory := CaptionSessionFactoryFunc(func(config deepgram.Config, apiKey []byte, handler deepgram.Handler) (CaptionSession, error) {
		gotConfig = config
		transientKey = apiKey
		session.handler = handler
		return session, nil
	})
	manager.SetCaptionRuntime(resolver, factory)
	manager.ApplyRuntimeConfig(captionRuntimeConfig(
		control.RuntimeProfile{ID: "caption-other", Kind: "caption", Config: captionProfileConfig("ja")},
		control.RuntimeProfile{ID: "caption-selected", Kind: "caption", Config: captionProfileConfig("en")},
	))

	if err := manager.Start(t.Context(), StreamContext{StreamID: "stream-01", CaptionProfileID: "caption-selected"}); err != nil {
		t.Fatal(err)
	}
	if resolveCalls != 1 || resolvedStreamID != "stream-01" || resolvedName != "deepgram_api_key" {
		t.Fatalf("unexpected runtime secret resolution: calls=%d stream=%q name=%q", resolveCalls, resolvedStreamID, resolvedName)
	}
	if gotConfig.Model != "nova-3" || gotConfig.Language != "en" || gotConfig.EndpointingMS != 300 || !gotConfig.InterimResults || !gotConfig.SmartFormat || gotConfig.Delay != 800*time.Millisecond {
		t.Fatalf("unexpected Deepgram config: %#v", gotConfig)
	}
	for _, value := range transientKey {
		if value != 0 {
			t.Fatalf("manager retained an unzeroed transient key: %v", transientKey)
		}
	}

	packet := deepgram.AudioPacket{SSRC: 42, UserID: "speaker-42", Sequence: 7, Timestamp: 960, ReceivedAt: testTime(), Opus: []byte{1, 2, 3}}
	if err := manager.IngestCaptionAudio(t.Context(), "stream-01", []deepgram.AudioPacket{packet}); err != nil {
		t.Fatal(err)
	}
	if len(session.packets) != 1 || session.packets[0].SSRC != 42 || session.packets[0].UserID != "speaker-42" {
		t.Fatalf("caption audio was not forwarded: %#v", session.packets)
	}
	if err := session.handler(t.Context(), deepgram.Transcript{Text: "interim", SpeakerUserID: "speaker-42"}); err != nil {
		t.Fatal(err)
	}
	if err := session.handler(t.Context(), deepgram.Transcript{Text: "final", SpeakerUserID: "speaker-42", Final: true}); err != nil {
		t.Fatal(err)
	}
	if len(pub.events) != 2 || pub.events[0].Type != "caption.telop" || pub.events[1].Type != "caption.final" {
		t.Fatalf("unexpected caption events: %#v", pub.events)
	}
	if pub.events[0].Payload["speaker_user_id"] != "speaker-42" || pub.events[1].Payload["speaker_user_id"] != "speaker-42" {
		t.Fatalf("speaker_user_id was not preserved: %#v", pub.events)
	}
	if err := manager.Stop(t.Context(), "stream-01"); err != nil {
		t.Fatal(err)
	}
	if session.closed != 1 || resolveCalls != 1 {
		t.Fatalf("unexpected caption lifecycle: closed=%d resolve_calls=%d", session.closed, resolveCalls)
	}
}

func TestManagerDoesNotEnableCaptionWithoutExplicitProfileID(t *testing.T) {
	manager := NewManager(&fakePublisher{}, observability.Client{})
	resolveCalls := 0
	manager.SetCaptionRuntime(RuntimeSecretResolverFunc(func(_ context.Context, _, secretName string) (control.RuntimeSecret, error) {
		resolveCalls++
		return control.RuntimeSecret{SecretName: secretName, Value: "dg-runtime-key", ExpiresInSec: 300}, nil
	}), CaptionSessionFactoryFunc(func(deepgram.Config, []byte, deepgram.Handler) (CaptionSession, error) {
		return &fakeCaptionSession{}, nil
	}))
	manager.ApplyRuntimeConfig(captionRuntimeConfig(control.RuntimeProfile{ID: "caption-default", Kind: "caption", Config: captionProfileConfig("ja")}))

	if err := manager.Start(t.Context(), StreamContext{StreamID: "stream-01"}); err != nil {
		t.Fatal(err)
	}
	if resolveCalls != 0 {
		t.Fatalf("runtime secret was resolved without an explicit caption profile: %d", resolveCalls)
	}
	err := manager.IngestCaptionAudio(t.Context(), "stream-01", []deepgram.AudioPacket{{SSRC: 42, Opus: []byte{1}}})
	if !errors.Is(err, ErrCaptionNotConfigured) {
		t.Fatalf("unexpected disabled-caption error: %v", err)
	}
}

func TestManagerProcessesNextCaptionPacketAfterSendFailure(t *testing.T) {
	manager := NewManager(&fakePublisher{}, observability.Client{})
	session := &fakeCaptionSession{ingestErrors: []error{errors.New("first send failed")}}
	manager.SetCaptionRuntime(RuntimeSecretResolverFunc(func(_ context.Context, _, secretName string) (control.RuntimeSecret, error) {
		return control.RuntimeSecret{SecretName: secretName, Value: "dg-runtime-key", ExpiresInSec: 300}, nil
	}), CaptionSessionFactoryFunc(func(deepgram.Config, []byte, deepgram.Handler) (CaptionSession, error) {
		return session, nil
	}))
	manager.ApplyRuntimeConfig(captionRuntimeConfig(control.RuntimeProfile{ID: "caption-01", Kind: "caption", Config: captionProfileConfig("ja")}))
	if err := manager.Start(t.Context(), StreamContext{StreamID: "stream-01", CaptionProfileID: "caption-01"}); err != nil {
		t.Fatal(err)
	}

	err := manager.IngestCaptionAudio(t.Context(), "stream-01", []deepgram.AudioPacket{
		{SSRC: 42, Sequence: 1, Opus: []byte{1}},
		{SSRC: 42, Sequence: 2, Opus: []byte{2}},
	})
	if !errors.Is(err, ErrCaptionAudioUnavailable) {
		t.Fatalf("unexpected batch error: %v", err)
	}
	if session.ingestCalls != 2 || len(session.packets) != 1 || session.packets[0].Sequence != 2 {
		t.Fatalf("next packet was not processed after send failure: calls=%d packets=%#v", session.ingestCalls, session.packets)
	}
}

func TestManagerStartFailsAtomicallyWhenRuntimeSecretResolutionFails(t *testing.T) {
	manager := NewManager(&fakePublisher{}, observability.Client{})
	resolveCalls := 0
	factoryCalls := 0
	manager.SetCaptionRuntime(RuntimeSecretResolverFunc(func(context.Context, string, string) (control.RuntimeSecret, error) {
		resolveCalls++
		return control.RuntimeSecret{}, errors.New("control panel failed with dg-secret-must-not-leak")
	}), CaptionSessionFactoryFunc(func(deepgram.Config, []byte, deepgram.Handler) (CaptionSession, error) {
		factoryCalls++
		return &fakeCaptionSession{}, nil
	}))
	manager.ApplyRuntimeConfig(captionRuntimeConfig(control.RuntimeProfile{ID: "caption-01", Kind: "caption", Config: captionProfileConfig("ja")}))

	err := manager.Start(t.Context(), StreamContext{StreamID: "stream-01", CaptionProfileID: "caption-01"})
	if !errors.Is(err, ErrCaptionRuntimeUnavailable) {
		t.Fatalf("unexpected start error: %v", err)
	}
	if strings.Contains(err.Error(), "dg-secret-must-not-leak") {
		t.Fatalf("runtime secret leaked in start error: %v", err)
	}
	if manager.CurrentStreamID() != "" {
		t.Fatalf("job started despite caption initialization failure: %#v", manager.Status())
	}
	if resolveCalls != 1 || factoryCalls != 0 {
		t.Fatalf("unexpected initialization calls: resolve=%d factory=%d", resolveCalls, factoryCalls)
	}
}

func TestManagerClosesPartialCaptionSessionWhenFactoryFails(t *testing.T) {
	manager := NewManager(&fakePublisher{}, observability.Client{})
	session := &fakeCaptionSession{}
	manager.SetCaptionRuntime(RuntimeSecretResolverFunc(func(_ context.Context, _, secretName string) (control.RuntimeSecret, error) {
		return control.RuntimeSecret{SecretName: secretName, Value: "dg-runtime-key", ExpiresInSec: 300}, nil
	}), CaptionSessionFactoryFunc(func(deepgram.Config, []byte, deepgram.Handler) (CaptionSession, error) {
		return session, errors.New("factory failed with dg-runtime-key")
	}))
	manager.ApplyRuntimeConfig(captionRuntimeConfig(control.RuntimeProfile{ID: "caption-01", Kind: "caption", Config: captionProfileConfig("ja")}))

	err := manager.Start(t.Context(), StreamContext{StreamID: "stream-01", CaptionProfileID: "caption-01"})
	if !errors.Is(err, ErrCaptionRuntimeUnavailable) || strings.Contains(err.Error(), "dg-runtime-key") {
		t.Fatalf("unexpected factory error: %v", err)
	}
	if session.closed != 1 || manager.CurrentStreamID() != "" {
		t.Fatalf("partial session was not closed: closed=%d status=%#v", session.closed, manager.Status())
	}
}

func TestManagerRejectsUnsupportedCaptionProviderBeforeSecretResolution(t *testing.T) {
	manager := NewManager(&fakePublisher{}, observability.Client{})
	resolveCalls := 0
	manager.SetCaptionRuntime(RuntimeSecretResolverFunc(func(context.Context, string, string) (control.RuntimeSecret, error) {
		resolveCalls++
		return control.RuntimeSecret{}, nil
	}), nil)
	config := captionProfileConfig("ja")
	config["provider"] = "other"
	manager.ApplyRuntimeConfig(captionRuntimeConfig(control.RuntimeProfile{ID: "caption-01", Kind: "caption", Config: config}))

	err := manager.Start(t.Context(), StreamContext{StreamID: "stream-01", CaptionProfileID: "caption-01"})
	if !errors.Is(err, ErrCaptionProfileInvalid) {
		t.Fatalf("unexpected profile error: %v", err)
	}
	if resolveCalls != 0 || manager.CurrentStreamID() != "" {
		t.Fatalf("unsupported provider reached runtime initialization: calls=%d status=%#v", resolveCalls, manager.Status())
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

func captionRuntimeConfig(profiles ...control.RuntimeProfile) control.RuntimeConfig {
	return control.RuntimeConfig{
		Service: control.RegisteredService{ServiceID: "worker-01", ServiceType: control.ServiceType},
		Assignments: []control.StreamServiceAssignment{{
			StreamID:       "stream-01",
			ServiceID:      "worker-01",
			ServiceType:    control.ServiceType,
			AssignmentRole: "primary",
		}},
		Profiles: map[string][]control.RuntimeProfile{"caption": profiles},
	}
}

func captionProfileConfig(language string) map[string]any {
	return map[string]any{
		"service_id":          "worker-01",
		"provider":            "deepgram",
		"model":               "nova-3",
		"language":            language,
		"api_key_secret_name": "deepgram_api_key",
		"endpointing_ms":      300,
		"interim_results":     true,
		"smart_format":        true,
		"delay_ms":            800,
	}
}
