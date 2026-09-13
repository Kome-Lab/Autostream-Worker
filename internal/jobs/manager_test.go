package jobs

import (
	"context"
	"errors"
	"github.com/example/autostream-worker/internal/control"
	"github.com/example/autostream-worker/internal/deepgram"
	"github.com/example/autostream-worker/internal/events"
	"github.com/example/autostream-worker/internal/observability"
	"image"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fakeSceneRenderer struct {
	resets []StreamContext
	clears []string
	events []events.OverlayEvent
}

type captionDisplaySceneRenderer struct {
	fakeSceneRenderer
	displays []captionDisplayConfig
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
func (*fakeSceneRenderer) ConfigureDisplay(int, time.Duration, time.Duration, time.Duration, bool) {
}

func (f *captionDisplaySceneRenderer) ConfigureDisplay(maxItems int, reorderWindow, interimTTL, finalTTL time.Duration, showVoiceTranscripts bool) {
	f.displays = append(f.displays, captionDisplayConfig{
		maxItems:             maxItems,
		reorderWindow:        reorderWindow,
		interimTTL:           interimTTL,
		finalTTL:             finalTTL,
		showVoiceTranscripts: showVoiceTranscripts,
	})
}

func TestManagerSceneLifecycleAppliesEventsBeforeForwardFailure(t *testing.T) {
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
		t.Fatal("expected the encoder forward failure to remain visible")
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

func TestManagerVideoOutputFailureFencesRearmUntilCaptionShutdown(t *testing.T) {
	oldSession := &closeBlockingCaptionSession{
		closeStarted: make(chan struct{}),
		releaseClose: make(chan struct{}),
	}
	newSession := &fakeCaptionSession{}
	sessions := []CaptionSession{oldSession, newSession}
	factoryCalls := 0
	output := &fakeVideoOutput{}
	manager := NewManager(&fakePublisher{}, observability.Client{})
	manager.SetSceneRenderer(&fakeSceneRenderer{})
	manager.SetVideoOutput(output)
	manager.SetCaptionRuntime(RuntimeSecretResolverFunc(func(_ context.Context, _, secretName string) (control.RuntimeSecret, error) {
		return control.RuntimeSecret{SecretName: secretName, Value: "dg-runtime-key", ExpiresInSec: 300}, nil
	}), CaptionSessionFactoryFunc(func(deepgram.Config, []byte, deepgram.Handler) (CaptionSession, error) {
		session := sessions[factoryCalls]
		factoryCalls++
		return session, nil
	}))
	manager.ApplyRuntimeConfig(captionRuntimeConfig(control.RuntimeProfile{ID: "caption-01", Kind: "caption", Config: captionProfileConfig("ja")}))
	oldStream := StreamContext{
		StreamID: "stream-01", CaptionProfileID: "caption-01", EncoderProfileID: "encoder-profile-01",
		VideoIngestURL: "srt://encoder.example:10080", VideoIngestPassphrase: "0123456789abcdef0123456789abcdef", VideoIngestPBKeylen: 32,
	}
	if err := manager.Start(t.Context(), oldStream); err != nil {
		t.Fatal(err)
	}

	failureDone := make(chan struct{})
	go func() {
		manager.HandleVideoOutputFailure(oldStream.StreamID, output.scenes[0].Generation, "srt_write")
		close(failureDone)
	}()
	select {
	case <-oldSession.closeStarted:
	case <-time.After(time.Second):
		t.Fatal("video failure did not begin old caption shutdown")
	}

	startDone := make(chan error, 1)
	go func() {
		startDone <- manager.Start(t.Context(), StreamContext{StreamID: oldStream.StreamID, CaptionProfileID: "caption-01"})
	}()
	select {
	case err := <-startDone:
		close(oldSession.releaseClose)
		t.Fatalf("rearm crossed the old caption shutdown fence: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(oldSession.releaseClose)
	select {
	case <-failureDone:
	case <-time.After(time.Second):
		t.Fatal("video failure did not finish after caption shutdown")
	}
	select {
	case err := <-startDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("rearm did not proceed after caption shutdown")
	}
	if got := manager.CurrentStreamID(); got != oldStream.StreamID {
		t.Fatalf("unexpected active stream after fenced rearm: %q", got)
	}
	if err := manager.Stop(t.Context(), oldStream.StreamID); err != nil {
		t.Fatal(err)
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
