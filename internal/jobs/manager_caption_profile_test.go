package jobs

import (
	"context"
	"errors"
	"github.com/example/autostream-worker/internal/control"
	"github.com/example/autostream-worker/internal/deepgram"
	"github.com/example/autostream-worker/internal/observability"
	"net/http"
	"testing"
	"time"
)

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

func TestManagerPublishesActiveSpeakerStopWithoutUserID(t *testing.T) {
	pub := &fakePublisher{}
	manager := NewManager(pub, observability.Client{})
	if err := manager.Start(t.Context(), StreamContext{StreamID: "stream-01"}); err != nil {
		t.Fatal(err)
	}
	event, err := manager.ActiveSpeakerState(t.Context(), "stream-01", "", "", false, testTime())
	if err != nil {
		t.Fatal(err)
	}
	if event.Type != "overlay.active_speaker" || event.Payload["speaking"] != false || event.Payload["user_id"] != "" {
		t.Fatalf("unexpected stop event: %#v", event)
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
	reporter := &fakeReporter{}
	manager := NewManager(pub, reporter)
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
	session.status = deepgram.Status{ActiveConnections: 1, AudioPacketsSent: 1, TranscriptMessages: 2, ProviderErrors: 1, LastErrorClass: "provider_auth_rejected", LastHTTPStatus: http.StatusUnauthorized}
	captionStatus := manager.Status()
	if !captionStatus.CaptionSessionActive || captionStatus.CaptionProviderConnections != 1 || captionStatus.CaptionProviderAudioPackets != 1 || captionStatus.CaptionProviderTranscriptMessages != 2 || captionStatus.CaptionProviderErrors != 1 || captionStatus.CaptionProviderLastErrorClass != "provider_auth_rejected" || captionStatus.CaptionProviderLastHTTPStatus != http.StatusUnauthorized {
		t.Fatalf("caption provider diagnostics were not exposed safely: %#v", captionStatus)
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
	var sawAudioStarted, sawInterim, sawFinal bool
	for i, name := range reporter.events {
		if i >= len(reporter.attrs) {
			continue
		}
		attrs := reporter.attrs[i]
		switch name {
		case "worker.caption.audio_started":
			sawAudioStarted = attrs["packet_count"] == 1
		case "worker.caption.transcript_received":
			if attrs["final"] == false {
				sawInterim = true
			}
			if attrs["final"] == true {
				sawFinal = true
			}
		}
	}
	if !sawAudioStarted || !sawInterim || !sawFinal {
		t.Fatalf("caption pipeline diagnostics were incomplete: events=%#v attrs=%#v", reporter.events, reporter.attrs)
	}
	if err := manager.Stop(t.Context(), "stream-01"); err != nil {
		t.Fatal(err)
	}
	if session.closed != 1 || resolveCalls != 1 {
		t.Fatalf("unexpected caption lifecycle: closed=%d resolve_calls=%d", session.closed, resolveCalls)
	}
}

func TestManagerReportsCaptionTranscriptPublishFailureWithoutTextLeak(t *testing.T) {
	reporter := &fakeReporter{}
	session := &fakeCaptionSession{}
	manager := NewManager(&fakePublisher{err: errors.New("encoder unavailable")}, reporter)
	manager.SetCaptionRuntime(RuntimeSecretResolverFunc(func(_ context.Context, _, secretName string) (control.RuntimeSecret, error) {
		return control.RuntimeSecret{SecretName: secretName, Value: "dg-runtime-key", ExpiresInSec: 300}, nil
	}), CaptionSessionFactoryFunc(func(_ deepgram.Config, _ []byte, handler deepgram.Handler) (CaptionSession, error) {
		session.handler = handler
		return session, nil
	}))
	manager.ApplyRuntimeConfig(captionRuntimeConfig(control.RuntimeProfile{ID: "caption-01", Kind: "caption", Config: captionProfileConfig("ja")}))
	if err := manager.Start(t.Context(), StreamContext{StreamID: "stream-01", CaptionProfileID: "caption-01"}); err != nil {
		t.Fatal(err)
	}
	if err := session.handler(t.Context(), deepgram.Transcript{Text: "秘匿すべき本文", SpeakerUserID: "speaker-42", Final: true}); err == nil {
		t.Fatal("expected encoder publish failure")
	}

	for i, name := range reporter.events {
		if name != "worker.caption.transcript_publish_failed" || i >= len(reporter.attrs) {
			continue
		}
		attrs := reporter.attrs[i]
		if attrs["error_class"] != "event_publish_failed" || attrs["text_length"] != len([]rune("秘匿すべき本文")) {
			t.Fatalf("unexpected caption failure diagnostic: %#v", attrs)
		}
		if _, leaked := attrs["text"]; leaked {
			t.Fatal("caption text leaked into failure diagnostics")
		}
		if err := manager.Stop(t.Context(), "stream-01"); err != nil {
			t.Fatal(err)
		}
		return
	}
	t.Fatalf("caption publish failure diagnostic was not reported: events=%#v attrs=%#v", reporter.events, reporter.attrs)
}

func TestManagerRejectsLateCaptionResultFromPreviousGeneration(t *testing.T) {
	manager := NewManager(&fakePublisher{}, observability.Client{})
	manager.SetCaptionRuntime(RuntimeSecretResolverFunc(func(_ context.Context, _, secretName string) (control.RuntimeSecret, error) {
		return control.RuntimeSecret{SecretName: secretName, Value: "dg-runtime-key", ExpiresInSec: 300}, nil
	}), nil)
	var sessions []*fakeCaptionSession
	manager.SetCaptionRuntime(manager.secretResolver, CaptionSessionFactoryFunc(func(_ deepgram.Config, _ []byte, handler deepgram.Handler) (CaptionSession, error) {
		session := &fakeCaptionSession{handler: handler}
		sessions = append(sessions, session)
		return session, nil
	}))
	manager.ApplyRuntimeConfig(captionRuntimeConfig(control.RuntimeProfile{ID: "caption-01", Kind: "caption", Config: captionProfileConfig("ja")}))

	if err := manager.Start(t.Context(), StreamContext{StreamID: "stream-01", CaptionProfileID: "caption-01"}); err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 {
		t.Fatalf("expected first caption session, got %d", len(sessions))
	}
	oldHandler := sessions[0].handler
	if err := manager.Stop(t.Context(), "stream-01"); err != nil {
		t.Fatal(err)
	}
	if err := manager.Start(t.Context(), StreamContext{StreamID: "stream-01", CaptionProfileID: "caption-01"}); err != nil {
		t.Fatal(err)
	}

	if err := oldHandler(t.Context(), deepgram.Transcript{Text: "late old result", Final: true}); !errors.Is(err, ErrJobGenerationMismatch) {
		t.Fatalf("late result from old generation was not fenced: %v", err)
	}
}

func TestManagerUpdatesRunningCaptionProfileWithoutChangingJobGeneration(t *testing.T) {
	pub := &fakePublisher{}
	manager := NewManager(pub, observability.Client{})
	manager.SetCaptionRuntime(RuntimeSecretResolverFunc(func(_ context.Context, _, secretName string) (control.RuntimeSecret, error) {
		return control.RuntimeSecret{SecretName: secretName, Value: "dg-runtime-key", ExpiresInSec: 300}, nil
	}), nil)
	var sessions []*fakeCaptionSession
	var configs []deepgram.Config
	manager.SetCaptionRuntime(manager.secretResolver, CaptionSessionFactoryFunc(func(config deepgram.Config, _ []byte, handler deepgram.Handler) (CaptionSession, error) {
		session := &fakeCaptionSession{handler: handler}
		sessions = append(sessions, session)
		configs = append(configs, config)
		return session, nil
	}))
	scene := &captionDisplaySceneRenderer{}
	manager.SetSceneRenderer(scene)
	manager.ApplyRuntimeConfig(captionRuntimeConfig(control.RuntimeProfile{ID: "caption-01", Kind: "caption", Config: captionProfileConfig("ja")}))
	if err := manager.Start(t.Context(), StreamContext{StreamID: "stream-01", CaptionProfileID: "caption-01"}); err != nil {
		t.Fatal(err)
	}
	jobGeneration := manager.Status().JobGeneration

	updatedConfig := captionProfileConfig("en")
	updatedConfig["conversation_max_items"] = 24
	updatedConfig["voice_final_ttl_seconds"] = 30
	manager.ApplyRuntimeConfig(captionRuntimeConfig(control.RuntimeProfile{ID: "caption-01", Kind: "caption", Config: updatedConfig}))
	if err := manager.UpdateCaptionRuntimeSettings(t.Context(), "stream-01", "caption-01"); err != nil {
		t.Fatal(err)
	}

	if len(sessions) != 2 || len(configs) != 2 {
		t.Fatalf("caption session refresh count = sessions:%d configs:%d, want 2", len(sessions), len(configs))
	}
	if configs[0].Language != "ja" || configs[1].Language != "en" {
		t.Fatalf("caption session languages = %#v", configs)
	}
	if sessions[0].closed != 1 || sessions[1].closed != 0 {
		t.Fatalf("caption session close state = old:%d new:%d", sessions[0].closed, sessions[1].closed)
	}
	if manager.Status().JobGeneration != jobGeneration {
		t.Fatalf("job generation changed during caption-only update: before=%d after=%d", jobGeneration, manager.Status().JobGeneration)
	}
	if manager.current.CaptionProfileID != "caption-01" {
		t.Fatalf("active caption profile = %q", manager.current.CaptionProfileID)
	}
	if len(scene.displays) < 2 || scene.displays[len(scene.displays)-1].maxItems != 24 || scene.displays[len(scene.displays)-1].finalTTL != 30*time.Second {
		t.Fatalf("updated caption display settings were not applied: %#v", scene.displays)
	}
	if err := sessions[0].handler(t.Context(), deepgram.Transcript{Text: "late old result", Final: true}); !errors.Is(err, ErrCaptionSessionGenerationMismatch) {
		t.Fatalf("late result from replaced caption session was not fenced: %v", err)
	}
	if err := sessions[1].handler(t.Context(), deepgram.Transcript{Text: "new session result", Final: true}); err != nil {
		t.Fatal(err)
	}
	if len(pub.events) != 1 || pub.events[0].Payload["text"] != "new session result" {
		t.Fatalf("unexpected caption events after session replacement: %#v", pub.events)
	}
	packet := deepgram.AudioPacket{SSRC: 42, UserID: "speaker-42", Opus: []byte{1, 2, 3}}
	if err := manager.IngestCaptionAudio(t.Context(), "stream-01", []deepgram.AudioPacket{packet}); err != nil {
		t.Fatal(err)
	}
	if len(sessions[0].packets) != 0 || len(sessions[1].packets) != 1 {
		t.Fatalf("caption audio did not converge on the new session: old=%d new=%d", len(sessions[0].packets), len(sessions[1].packets))
	}
}

func TestManagerKeepsRunningCaptionSessionWhenRuntimeUpdateFails(t *testing.T) {
	manager := NewManager(&fakePublisher{}, observability.Client{})
	manager.SetCaptionRuntime(RuntimeSecretResolverFunc(func(_ context.Context, _, secretName string) (control.RuntimeSecret, error) {
		return control.RuntimeSecret{SecretName: secretName, Value: "dg-runtime-key", ExpiresInSec: 300}, nil
	}), nil)
	oldSession := &fakeCaptionSession{}
	factoryCalls := 0
	manager.SetCaptionRuntime(manager.secretResolver, CaptionSessionFactoryFunc(func(_ deepgram.Config, _ []byte, handler deepgram.Handler) (CaptionSession, error) {
		factoryCalls++
		if factoryCalls == 1 {
			oldSession.handler = handler
			return oldSession, nil
		}
		return nil, errors.New("provider details must remain private")
	}))
	manager.ApplyRuntimeConfig(captionRuntimeConfig(control.RuntimeProfile{ID: "caption-01", Kind: "caption", Config: captionProfileConfig("ja")}))
	if err := manager.Start(t.Context(), StreamContext{StreamID: "stream-01", CaptionProfileID: "caption-01"}); err != nil {
		t.Fatal(err)
	}
	jobGeneration := manager.Status().JobGeneration
	manager.ApplyRuntimeConfig(captionRuntimeConfig(control.RuntimeProfile{ID: "caption-01", Kind: "caption", Config: captionProfileConfig("en")}))

	err := manager.UpdateCaptionRuntimeSettings(t.Context(), "stream-01", "caption-01")
	if !errors.Is(err, ErrCaptionRuntimeUnavailable) {
		t.Fatalf("runtime update error = %v", err)
	}
	if oldSession.closed != 0 || manager.captionSession != oldSession {
		t.Fatalf("old caption session was not preserved: closed=%d current=%T", oldSession.closed, manager.captionSession)
	}
	if manager.Status().JobGeneration != jobGeneration {
		t.Fatalf("job generation changed after failed caption update: before=%d after=%d", jobGeneration, manager.Status().JobGeneration)
	}
	packet := deepgram.AudioPacket{SSRC: 42, UserID: "speaker-42", Opus: []byte{1}}
	if err := manager.IngestCaptionAudio(t.Context(), "stream-01", []deepgram.AudioPacket{packet}); err != nil {
		t.Fatal(err)
	}
	if len(oldSession.packets) != 1 {
		t.Fatalf("old caption session stopped receiving audio after failed update: %#v", oldSession.packets)
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
