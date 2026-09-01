package jobs

import (
	"context"
	"errors"
	"image"
	"strings"
	"testing"
	"time"

	"github.com/example/autostream-worker/internal/control"
	"github.com/example/autostream-worker/internal/deepgram"
	"github.com/example/autostream-worker/internal/observability"
	"github.com/example/autostream-worker/internal/sceneappearance"
)

type appearanceSceneRenderer struct {
	fakeSceneRenderer
	configured []sceneappearance.Prepared
	configure  error
}

func (renderer *appearanceSceneRenderer) ConfigureAppearance(prepared sceneappearance.Prepared) error {
	renderer.configured = append(renderer.configured, prepared)
	return renderer.configure
}

func TestManagerSceneAppearanceFailureIsAtomicBeforeCaptionAndVideoSideEffects(t *testing.T) {
	renderer := &appearanceSceneRenderer{}
	output := &fakeVideoOutput{}
	manager := NewManager(&fakePublisher{}, observability.Client{})
	manager.SetSceneRenderer(renderer)
	manager.SetVideoOutput(output)
	prepareCalls := 0
	manager.SetSceneAppearanceRuntime(SceneAppearanceRuntimeFunc(func(context.Context, string, *sceneappearance.Snapshot) (sceneappearance.Prepared, error) {
		prepareCalls++
		return sceneappearance.Prepared{}, sceneappearance.NewError(sceneappearance.CodeMediaAssetHashMismatch)
	}))
	resolverCalls := 0
	manager.SetCaptionRuntime(RuntimeSecretResolverFunc(func(context.Context, string, string) (control.RuntimeSecret, error) {
		resolverCalls++
		return control.RuntimeSecret{SecretName: "deepgram_api_key", Value: "must-not-be-resolved", ExpiresInSec: 300}, nil
	}), CaptionSessionFactoryFunc(func(deepgram.Config, []byte, deepgram.Handler) (CaptionSession, error) {
		t.Fatal("caption factory ran after scene appearance preparation failed")
		return nil, nil
	}))
	manager.ApplyRuntimeConfig(captionRuntimeConfig(control.RuntimeProfile{ID: "caption-01", Kind: "caption", Config: captionProfileConfig("ja")}))

	err := manager.Start(t.Context(), StreamContext{
		StreamID: "stream-01", CaptionProfileID: "caption-01", SceneAppearance: defaultSceneAppearance(),
		EncoderProfileID: "encoder-01", VideoIngestURL: "srt://encoder.example:10080",
		VideoIngestPassphrase: "0123456789abcdef0123456789abcdef", VideoIngestPBKeylen: 32,
	})
	code, ok := sceneappearance.CodeOf(err)
	if !ok || code != sceneappearance.CodeMediaAssetHashMismatch {
		t.Fatalf("appearance fault = %v code=%q", err, code)
	}
	if prepareCalls != 1 || resolverCalls != 0 || len(renderer.resets) != 0 || len(renderer.configured) != 0 || len(output.starts) != 0 {
		t.Fatalf("failed appearance crossed start boundary: prepare=%d resolve=%d resets=%d configured=%d video=%d", prepareCalls, resolverCalls, len(renderer.resets), len(renderer.configured), len(output.starts))
	}
	if manager.CurrentStreamID() != "" || manager.Status().JobGeneration != 0 {
		t.Fatalf("failed appearance left active state: %#v", manager.Status())
	}
	// A failed required custom appearance must not poison a later legacy start.
	if err := manager.Start(t.Context(), StreamContext{StreamID: "stream-01"}); err != nil {
		t.Fatalf("legacy reconcile after appearance failure failed: %v", err)
	}
	if err := manager.Stop(t.Context(), "stream-01"); err != nil {
		t.Fatal(err)
	}
}

func TestManagerRedactsUntypedAppearancePreparationFailure(t *testing.T) {
	manager := NewManager(&fakePublisher{}, observability.Client{})
	manager.SetSceneRenderer(&appearanceSceneRenderer{})
	manager.SetSceneAppearanceRuntime(SceneAppearanceRuntimeFunc(func(context.Context, string, *sceneappearance.Snapshot) (sceneappearance.Prepared, error) {
		return sceneappearance.Prepared{}, errors.New(`Bearer secret C:\private\asset.png https://forbidden.example`)
	}))

	err := manager.Start(t.Context(), StreamContext{StreamID: "stream-01", SceneAppearance: defaultSceneAppearance()})
	code, ok := sceneappearance.CodeOf(err)
	if !ok || code != sceneappearance.CodeMediaAssetVariantFailed || err.Error() != string(sceneappearance.CodeMediaAssetVariantFailed) {
		t.Fatalf("untyped prepare error was not normalized: err=%v code=%q", err, code)
	}
	for _, forbidden := range []string{"secret", "private", "forbidden.example"} {
		if strings.Contains(err.Error(), forbidden) {
			t.Fatalf("appearance prepare error leaked %q: %v", forbidden, err)
		}
	}
	if manager.CurrentStreamID() != "" || manager.Status().JobGeneration != 0 {
		t.Fatalf("untyped prepare error crossed start boundary: %#v", manager.Status())
	}
}

func TestManagerPreparesAppearanceOnceAndRenderingPreservesAudioSession(t *testing.T) {
	renderer := &appearanceSceneRenderer{}
	manager := NewManager(&fakePublisher{}, observability.Client{})
	manager.SetSceneRenderer(renderer)
	prepareCalls := 0
	manager.SetSceneAppearanceRuntime(SceneAppearanceRuntimeFunc(func(_ context.Context, streamID string, snapshot *sceneappearance.Snapshot) (sceneappearance.Prepared, error) {
		prepareCalls++
		if streamID != "stream-01" || snapshot.Generation != 11 {
			t.Fatalf("unexpected immutable start snapshot: stream=%q snapshot=%#v", streamID, snapshot)
		}
		return sceneappearance.Prepared{
			Generation: snapshot.Generation, Revision: snapshot.Revision,
			BackgroundMode:  sceneappearance.BackgroundModeDefault,
			HeaderTitleMode: sceneappearance.HeaderTitleModeCustom, CustomTitle: "Operations Live",
		}, nil
	}))
	session := &fakeCaptionSession{}
	factoryCalls := 0
	manager.SetCaptionRuntime(RuntimeSecretResolverFunc(func(_ context.Context, _, secretName string) (control.RuntimeSecret, error) {
		return control.RuntimeSecret{SecretName: secretName, Value: "dg-runtime-key", ExpiresInSec: 300}, nil
	}), CaptionSessionFactoryFunc(func(deepgram.Config, []byte, deepgram.Handler) (CaptionSession, error) {
		factoryCalls++
		return session, nil
	}))
	manager.ApplyRuntimeConfig(captionRuntimeConfig(control.RuntimeProfile{ID: "caption-01", Kind: "caption", Config: captionProfileConfig("ja")}))
	snapshot := defaultSceneAppearance()
	snapshot.Generation = 11
	snapshot.Revision = 6
	if err := manager.Start(t.Context(), StreamContext{StreamID: "stream-01", CaptionProfileID: "caption-01", SceneAppearance: snapshot}); err != nil {
		t.Fatal(err)
	}
	generation := manager.Status().JobGeneration
	const timestampStep uint64 = 960
	firstTimestamp := uint64(48_000)
	secondTimestamp := firstTimestamp + timestampStep
	if err := manager.IngestCaptionAudioForGeneration(t.Context(), "stream-01", generation, []deepgram.AudioPacket{{SSRC: 42, Sequence: 1, Timestamp: firstTimestamp, Opus: []byte{1}}}); err != nil {
		t.Fatalf("audio before appearance render failed: %v", err)
	}
	for index := 0; index < 5; index++ {
		frame, err := manager.RenderScene(testTime().Add(time.Duration(index) * time.Second))
		if err != nil || frame.Bounds() != image.Rect(0, 0, 1920, 1080) {
			t.Fatalf("render %d failed: bounds=%v err=%v", index, frame.Bounds(), err)
		}
	}
	if err := manager.IngestCaptionAudioForGeneration(t.Context(), "stream-01", generation, []deepgram.AudioPacket{{SSRC: 42, Sequence: 2, Timestamp: secondTimestamp, Opus: []byte{2}}}); err != nil {
		t.Fatalf("audio after appearance render failed: %v", err)
	}
	if prepareCalls != 1 || len(renderer.configured) != 1 {
		t.Fatalf("per-frame path repeated appearance work: prepare=%d configured=%d", prepareCalls, len(renderer.configured))
	}
	if len(session.packets) != 2 {
		t.Fatalf("appearance render changed audio packet count: packets=%#v", session.packets)
	}
	timestampDiscontinuity := int64(session.packets[1].Timestamp) - int64(session.packets[0].Timestamp) - int64(timestampStep)
	if factoryCalls != 1 || session.closed != 0 ||
		session.packets[0].Sequence != 1 || session.packets[1].Sequence != 2 ||
		session.packets[0].Timestamp != firstTimestamp || session.packets[1].Timestamp != secondTimestamp ||
		timestampDiscontinuity != 0 || !manager.Status().CaptionSessionActive {
		t.Fatalf("appearance render disrupted audio continuity: factory=%d closed=%d timestamp_discontinuity=%d packets=%#v status=%#v", factoryCalls, session.closed, timestampDiscontinuity, session.packets, manager.Status())
	}
	if err := manager.Stop(t.Context(), "stream-01"); err != nil {
		t.Fatal(err)
	}
	if session.closed != 1 || len(renderer.clears) != 1 {
		t.Fatalf("stop did not clean appearance/audio exactly once: closed=%d clears=%#v", session.closed, renderer.clears)
	}
}

func TestManagerAppearanceRendererFailureIsRedactedAndCleansStartState(t *testing.T) {
	renderer := &appearanceSceneRenderer{configure: errors.New("C:\\private\\asset.png https://forbidden.example Bearer secret")}
	manager := NewManager(&fakePublisher{}, observability.Client{})
	manager.SetSceneRenderer(renderer)
	manager.SetSceneAppearanceRuntime(SceneAppearanceRuntimeFunc(func(context.Context, string, *sceneappearance.Snapshot) (sceneappearance.Prepared, error) {
		return sceneappearance.Prepared{
			BackgroundMode:  sceneappearance.BackgroundModeDefault,
			HeaderTitleMode: sceneappearance.HeaderTitleModeDefault,
		}, nil
	}))
	session := &fakeCaptionSession{}
	manager.SetCaptionRuntime(RuntimeSecretResolverFunc(func(_ context.Context, _, secretName string) (control.RuntimeSecret, error) {
		return control.RuntimeSecret{SecretName: secretName, Value: "dg-runtime-key", ExpiresInSec: 300}, nil
	}), CaptionSessionFactoryFunc(func(deepgram.Config, []byte, deepgram.Handler) (CaptionSession, error) {
		return session, nil
	}))
	manager.ApplyRuntimeConfig(captionRuntimeConfig(control.RuntimeProfile{ID: "caption-01", Kind: "caption", Config: captionProfileConfig("ja")}))

	err := manager.Start(t.Context(), StreamContext{StreamID: "stream-01", CaptionProfileID: "caption-01", SceneAppearance: defaultSceneAppearance()})
	code, ok := sceneappearance.CodeOf(err)
	if !ok || code != sceneappearance.CodeMediaAssetVariantFailed || err.Error() != string(sceneappearance.CodeMediaAssetVariantFailed) {
		t.Fatalf("renderer error was not normalized: err=%v code=%q", err, code)
	}
	for _, forbidden := range []string{"private", "forbidden.example", "Bearer", "secret"} {
		if strings.Contains(err.Error(), forbidden) {
			t.Fatalf("renderer error leaked %q: %v", forbidden, err)
		}
	}
	if manager.CurrentStreamID() != "" || len(renderer.resets) != 1 || len(renderer.clears) != 1 || session.closed != 1 {
		t.Fatalf("renderer failure cleanup mismatch: status=%#v resets=%d clears=%d closed=%d", manager.Status(), len(renderer.resets), len(renderer.clears), session.closed)
	}
}

func TestManagerSceneAppearanceStopRestartReconcilesNewSnapshot(t *testing.T) {
	renderer := &appearanceSceneRenderer{}
	manager := NewManager(&fakePublisher{}, observability.Client{})
	manager.SetSceneRenderer(renderer)
	var generations []uint64
	manager.SetSceneAppearanceRuntime(SceneAppearanceRuntimeFunc(func(_ context.Context, _ string, snapshot *sceneappearance.Snapshot) (sceneappearance.Prepared, error) {
		generations = append(generations, snapshot.Generation)
		return sceneappearance.Prepared{
			Generation: snapshot.Generation, Revision: snapshot.Revision,
			BackgroundMode:  sceneappearance.BackgroundModeDefault,
			HeaderTitleMode: sceneappearance.HeaderTitleModeCustom,
			CustomTitle:     "title-" + string(rune('0'+snapshot.Revision)),
		}, nil
	}))
	first := defaultSceneAppearance()
	first.Generation, first.Revision = 20, 1
	first.HeaderTitleMode, first.CustomTitle = sceneappearance.HeaderTitleModeCustom, "ignored-by-fake-runtime"
	if err := manager.Start(t.Context(), StreamContext{StreamID: "stream-01", SceneAppearance: first}); err != nil {
		t.Fatal(err)
	}
	if err := manager.Stop(t.Context(), "stream-01"); err != nil {
		t.Fatal(err)
	}
	second := defaultSceneAppearance()
	second.Generation, second.Revision = 21, 2
	second.HeaderTitleMode, second.CustomTitle = sceneappearance.HeaderTitleModeCustom, "ignored-by-fake-runtime"
	if err := manager.Start(t.Context(), StreamContext{StreamID: "stream-01", SceneAppearance: second}); err != nil {
		t.Fatal(err)
	}
	if len(generations) != 2 || generations[0] != 20 || generations[1] != 21 || len(renderer.configured) != 2 {
		t.Fatalf("restart did not reconcile both snapshots: generations=%#v configured=%#v", generations, renderer.configured)
	}
	if renderer.configured[0].CustomTitle != "title-1" || renderer.configured[1].CustomTitle != "title-2" || len(renderer.clears) != 1 || len(renderer.resets) != 2 {
		t.Fatalf("restart retained or skipped appearance state: configured=%#v clears=%#v resets=%#v", renderer.configured, renderer.clears, renderer.resets)
	}
	if err := manager.Stop(t.Context(), "stream-01"); err != nil {
		t.Fatal(err)
	}
}

func defaultSceneAppearance() *sceneappearance.Snapshot {
	return &sceneappearance.Snapshot{
		Generation: 1, Revision: 1, Capability: sceneappearance.Capability,
		Readiness:       sceneappearance.ReadinessReady,
		BackgroundMode:  sceneappearance.BackgroundModeDefault,
		HeaderTitleMode: sceneappearance.HeaderTitleModeDefault,
	}
}
