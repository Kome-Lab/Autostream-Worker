package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"image"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/example/autostream-worker/internal/control"
	"github.com/example/autostream-worker/internal/deepgram"
	"github.com/example/autostream-worker/internal/encoder"
	"github.com/example/autostream-worker/internal/events"
	"github.com/example/autostream-worker/internal/observability"
)

type fakeSceneRenderer struct {
	resets []StreamContext
	clears []string
	events []events.OverlayEvent
}

type fakeVideoOutput struct {
	starts   []StreamContext
	scenes   []VideoSceneConfig
	stops    []string
	startErr error
}

type fakeReporter struct {
	events []string
	attrs  []map[string]any
}

func (f *fakeReporter) Event(_ context.Context, _, name, _ string, attributes map[string]any) error {
	f.events = append(f.events, name)
	f.attrs = append(f.attrs, attributes)
	return nil
}

func (*fakeReporter) Metric(context.Context, string, string, string, float64, map[string]any) error {
	return nil
}

func (f *fakeVideoOutput) Start(_ context.Context, stream StreamContext, scene VideoSceneConfig) error {
	f.starts = append(f.starts, stream)
	f.scenes = append(f.scenes, scene)
	return f.startErr
}
func (f *fakeVideoOutput) Stop(_ context.Context, streamID string) error {
	f.stops = append(f.stops, streamID)
	return nil
}

func (f *fakeSceneRenderer) Reset(_ uint64, streamID, streamName string) {
	f.resets = append(f.resets, StreamContext{StreamID: streamID, StreamName: streamName})
}

func (f *fakeSceneRenderer) Clear(streamID string) { f.clears = append(f.clears, streamID) }
func (f *fakeSceneRenderer) Apply(_ uint64, event events.OverlayEvent) error {
	f.events = append(f.events, event)
	return nil
}
func (f *fakeSceneRenderer) RenderSize(width, height int, _ time.Time) (*image.RGBA, error) {
	return image.NewRGBA(image.Rect(0, 0, width, height)), nil
}
func (*fakeSceneRenderer) AvatarRefreshInterval() time.Duration { return time.Minute }
func (*fakeSceneRenderer) RefreshAvatars()                      {}

func TestManagerSceneLifecycleAppliesEventsBeforeLegacyForwardFailure(t *testing.T) {
	scene := &fakeSceneRenderer{}
	manager := NewManager(&fakePublisher{err: errors.New("encoder unavailable")}, observability.Client{})
	manager.SetSceneRenderer(scene)
	if err := manager.Start(t.Context(), StreamContext{StreamID: "stream-01", StreamName: "Dev", VideoWidth: 1280, VideoHeight: 720, VideoFPS: 30}); err != nil {
		t.Fatal(err)
	}
	frame, err := manager.RenderScene(testTime())
	if err != nil || frame.Bounds() != image.Rect(0, 0, 1280, 720) {
		t.Fatalf("initial scene frame unavailable after start: frame=%v err=%v", frame.Bounds(), err)
	}
	if _, err := manager.CustomOverlay(t.Context(), "stream-01", "overlay.discord_chat", map[string]any{
		"message_id": "message-01", "author_id": "user-01", "content": "hello",
	}, testTime()); err == nil {
		t.Fatal("expected the legacy encoder forward failure to remain visible")
	}
	if len(scene.events) != 1 || scene.events[0].Type != "overlay.discord_chat" {
		t.Fatalf("local scene was not updated before forward failure: %#v", scene.events)
	}
	config, ok := manager.SceneVideoConfig()
	if !ok || config.Width != 1280 || config.Height != 720 || config.FPS != 30 {
		t.Fatalf("unexpected scene video config: %#v ok=%v", config, ok)
	}
	if err := manager.Stop(t.Context(), "stream-01"); err != nil {
		t.Fatal(err)
	}
	if len(scene.clears) != 1 || scene.clears[0] != "stream-01" {
		t.Fatalf("stop did not clear scene: %#v", scene.clears)
	}
}

func TestManagerSceneUsesCompatibilityVideoDefaultsAndResetsForSuccessor(t *testing.T) {
	scene := &fakeSceneRenderer{}
	manager := NewManager(&fakePublisher{}, observability.Client{})
	manager.SetSceneRenderer(scene)
	if err := manager.Start(t.Context(), StreamContext{StreamID: "stream-01", StreamName: "Old"}); err != nil {
		t.Fatal(err)
	}
	config, ok := manager.SceneVideoConfig()
	if !ok || config != (VideoSceneConfig{Width: 1920, Height: 1080, FPS: 30}) {
		t.Fatalf("unexpected compatibility defaults: %#v ok=%v", config, ok)
	}
	if err := manager.Stop(t.Context(), "stream-01"); err != nil {
		t.Fatal(err)
	}
	if err := manager.Start(t.Context(), StreamContext{StreamID: "stream-02", StreamName: "New"}); err != nil {
		t.Fatal(err)
	}
	if len(scene.resets) != 2 || scene.resets[1].StreamID != "stream-02" || scene.resets[1].StreamName != "New" {
		t.Fatalf("successor scene was not reset: %#v", scene.resets)
	}
}

func TestManagerVideoOutputStartsAfterSceneAndRollsBackAtomically(t *testing.T) {
	scene := &fakeSceneRenderer{}
	output := &fakeVideoOutput{startErr: errors.New("must not leak ingest-passphrase")}
	manager := NewManager(&fakePublisher{}, observability.Client{})
	manager.SetSceneRenderer(scene)
	manager.SetVideoOutput(output)
	err := manager.Start(t.Context(), StreamContext{
		StreamID: "stream-01", StreamName: "Dev", VideoIngestURL: "srt://encoder.example:10080",
		VideoIngestPassphrase: "0123456789abcdef0123456789abcdef", VideoIngestPBKeylen: 32, EncoderProfileID: "encoder-profile-01",
	})
	if !errors.Is(err, ErrVideoOutputUnavailable) || strings.Contains(err.Error(), "passphrase") {
		t.Fatalf("unexpected secret-safe start error: %v", err)
	}
	if len(output.starts) != 1 || len(output.stops) != 1 || len(scene.resets) != 1 || len(scene.clears) != 1 {
		t.Fatalf("video rollback lifecycle mismatch: starts=%d stops=%d resets=%d clears=%d", len(output.starts), len(output.stops), len(scene.resets), len(scene.clears))
	}
	if manager.CurrentStreamID() != "" {
		t.Fatalf("failed video output left an active job: %#v", manager.Status())
	}
}

func TestManagerVideoOutputFailureClearsOnlyMatchingActiveJob(t *testing.T) {
	scene := &fakeSceneRenderer{}
	output := &fakeVideoOutput{}
	manager := NewManager(&fakePublisher{}, observability.Client{})
	manager.SetSceneRenderer(scene)
	manager.SetVideoOutput(output)
	stream := StreamContext{
		StreamID: "stream-01", StreamName: "Dev", EncoderProfileID: "encoder-profile-01",
		VideoIngestURL: "srt://encoder.example:10080", VideoIngestPassphrase: "0123456789abcdef0123456789abcdef", VideoIngestPBKeylen: 32,
	}
	if err := manager.Start(t.Context(), stream); err != nil {
		t.Fatal(err)
	}

	generation := output.scenes[0].Generation
	if generation == 0 {
		t.Fatal("video start did not receive a job generation")
	}
	manager.HandleVideoOutputFailure(stream.StreamID, generation+1, "srt_write")
	if got := manager.CurrentStreamID(); got != stream.StreamID {
		t.Fatalf("stale generation cleared active stream: got %q", got)
	}
	if len(scene.clears) != 0 {
		t.Fatalf("stale video failure cleared scene: %#v", scene.clears)
	}

	manager.HandleVideoOutputFailure(stream.StreamID, generation, "srt_write")
	if got := manager.CurrentStreamID(); got != "" {
		t.Fatalf("video failure left active stream: got %q", got)
	}
	if len(scene.clears) != 1 || scene.clears[0] != stream.StreamID {
		t.Fatalf("video failure did not clear the matching scene: %#v", scene.clears)
	}
	if len(output.stops) != 0 {
		t.Fatalf("failure callback must not recursively stop its own video output: %#v", output.stops)
	}
	if _, err := manager.RenderScene(testTime()); !errors.Is(err, ErrNoActiveStreamJob) {
		t.Fatalf("render remained available after video failure: %v", err)
	}
	if err := manager.Stop(t.Context(), stream.StreamID); !errors.Is(err, ErrStreamAlreadyStopped) {
		t.Fatalf("control-panel stop was not idempotent after video failure: %v", err)
	}
}

func TestManagerVideoOutputFailureFailsClosedWhenStoppedReceiptCannotPersist(t *testing.T) {
	scene := &fakeSceneRenderer{}
	output := &fakeVideoOutput{}
	reporter := &fakeReporter{}
	receiptPath := filepath.Join(t.TempDir(), "missing", "stopped-targets.json")
	manager := newManager(&fakePublisher{}, reporter, receiptPath)
	manager.SetSceneRenderer(scene)
	manager.SetVideoOutput(output)
	stream := StreamContext{
		StreamID: "stream-01", EncoderProfileID: "encoder-profile-01",
		VideoIngestURL: "srt://encoder.example:10080", VideoIngestPassphrase: "0123456789abcdef0123456789abcdef", VideoIngestPBKeylen: 32,
	}
	if err := manager.Start(t.Context(), stream); err != nil {
		t.Fatal(err)
	}
	manager.HandleVideoOutputFailure(stream.StreamID, output.scenes[0].Generation, "srt_write")
	if got := manager.CurrentStreamID(); got != "" {
		t.Fatalf("receipt failure left a ghost active job: %q", got)
	}
	if len(reporter.events) == 0 || reporter.events[len(reporter.events)-1] != "worker.video.output_failed" {
		t.Fatalf("video failure was not reported: %#v", reporter.events)
	}
	attributes := reporter.attrs[len(reporter.attrs)-1]
	if attributes["stopped_target_receipt"] != "unavailable" {
		t.Fatalf("receipt durability failure was not reported: %#v", attributes)
	}
	if attributes["error_class"] != "srt_write" {
		t.Fatalf("video output error class was not reported: %#v", attributes)
	}
	if err := manager.Stop(t.Context(), stream.StreamID); !errors.Is(err, ErrStreamAlreadyStopped) {
		t.Fatalf("same-process stop did not converge after receipt durability failure: %v", err)
	}
}

type fakePublisher struct {
	events  []encoder.Event
	err     error
	mu      sync.Mutex
	handler func(encoder.Event) error
}

func (f *fakePublisher) Publish(ctx context.Context, event encoder.Event) error {
	if f.handler != nil {
		if err := f.handler(event); err != nil {
			return err
		}
	}
	if f.err != nil {
		return f.err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, event)
	return nil
}

func (f *fakePublisher) snapshot() []encoder.Event {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]encoder.Event(nil), f.events...)
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
	if _, err := manager.CustomOverlay(t.Context(), "stream-01", "overlay.discord_chat", map[string]any{"message_id": "msg-01", "text": "hello"}, testTime()); err == nil {
		t.Fatal("expected transport failure to be reported to the caller")
	}
	waitForManager(t, time.Second, func() bool { return len(pub.snapshot()) == 1 })
	if attempts != 2 || pub.snapshot()[0].Payload["message_id"] != "msg-01" {
		t.Fatalf("transport failure was not retried: attempts=%d events=%#v", attempts, pub.snapshot())
	}
}

func TestManagerStopsAtBoundedRetryLimit(t *testing.T) {
	attempts := 0
	pub := &fakePublisher{handler: func(encoder.Event) error {
		attempts++
		return encoder.NewRetryablePublishError(http.StatusServiceUnavailable, "http_status")
	}}
	manager := NewManager(pub, nil)
	if err := manager.Start(t.Context(), StreamContext{StreamID: "stream-01"}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Participants(t.Context(), "stream-01", []events.Participant{{UserID: "user-01"}}, testTime()); err == nil {
		t.Fatal("expected bounded delivery failure")
	}
	waitForManager(t, 3*time.Second, func() bool { return attempts >= maxWorkerEventAttempts })
	if attempts != maxWorkerEventAttempts {
		t.Fatalf("retry limit was not enforced: attempts=%d want=%d", attempts, maxWorkerEventAttempts)
	}
	time.Sleep(250 * time.Millisecond)
	if attempts != maxWorkerEventAttempts {
		t.Fatalf("retry continued past the bound: attempts=%d", attempts)
	}
}

func waitForManager(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out waiting for manager condition")
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
