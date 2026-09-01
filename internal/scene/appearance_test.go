package scene

import (
	"bytes"
	"image"
	"image/color"
	"strings"
	"testing"
	"time"

	"github.com/example/autostream-worker/internal/sceneappearance"
)

func TestSceneAppearanceOmissionAndExplicitDefaultArePixelIdentical(t *testing.T) {
	now := time.Date(2026, 9, 1, 1, 2, 3, 0, time.UTC)
	omitted := newTestScene(t, 854, 480, now)
	explicit := newTestScene(t, 854, 480, now)
	t.Cleanup(omitted.Close)
	t.Cleanup(explicit.Close)
	omitted.Reset(1, "stream-01", "Legacy Stream")
	explicit.Reset(1, "stream-01", "Legacy Stream")
	if err := explicit.ConfigureAppearance(sceneappearance.Prepared{
		Generation: 4, Revision: 7,
		BackgroundMode:  sceneappearance.BackgroundModeDefault,
		HeaderTitleMode: sceneappearance.HeaderTitleModeDefault,
	}); err != nil {
		t.Fatal(err)
	}
	omittedFrame, err := omitted.RenderSize(854, 480, now)
	if err != nil {
		t.Fatal(err)
	}
	explicitFrame, err := explicit.RenderSize(854, 480, now)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(omittedFrame.Pix, explicitFrame.Pix) {
		t.Fatal("explicit default changed the legacy scene pixels")
	}
}

func TestScaleBackgroundCoverUsesDeterministicCenteredCrop(t *testing.T) {
	wide := image.NewRGBA(image.Rect(0, 0, 6, 2))
	fillRect(wide, image.Rect(0, 0, 2, 2), color.RGBA{R: 255, A: 255})
	fillRect(wide, image.Rect(2, 0, 4, 2), color.RGBA{G: 255, A: 255})
	fillRect(wide, image.Rect(4, 0, 6, 2), color.RGBA{B: 255, A: 255})
	wideResult := scaleBackgroundCover(wide, 2, 2)
	assertMostlyGreen(t, wideResult.RGBAAt(0, 0))
	assertMostlyGreen(t, wideResult.RGBAAt(1, 1))

	tall := image.NewRGBA(image.Rect(0, 0, 2, 6))
	fillRect(tall, image.Rect(0, 0, 2, 2), color.RGBA{R: 255, A: 255})
	fillRect(tall, image.Rect(0, 2, 2, 4), color.RGBA{G: 255, A: 255})
	fillRect(tall, image.Rect(0, 4, 2, 6), color.RGBA{B: 255, A: 255})
	tallResult := scaleBackgroundCover(tall, 2, 2)
	assertMostlyGreen(t, tallResult.RGBAAt(0, 0))
	assertMostlyGreen(t, tallResult.RGBAAt(1, 1))

	square := image.NewRGBA(image.Rect(0, 0, 2, 2))
	fillRect(square, square.Bounds(), color.RGBA{R: 220, G: 180, B: 20, A: 255})
	squareResult := scaleBackgroundCover(square, 4, 2)
	if got := squareResult.RGBAAt(2, 1); got.R < 200 || got.G < 160 || got.B > 40 {
		t.Fatalf("square source scaling color = %#v", got)
	}
}

func TestSceneAppearanceExtremeTallOnePixelSourceRendersCustomPixels(t *testing.T) {
	now := time.Date(2026, 9, 1, 1, 2, 3, 0, time.UTC)
	background := image.NewRGBA(image.Rect(0, 0, 1, 2))
	want := color.RGBA{R: 210, G: 30, B: 170, A: 255}
	fillRect(background, background.Bounds(), want)

	scene := newTestScene(t, 854, 480, now)
	t.Cleanup(scene.Close)
	scene.Reset(1, "stream-01", "Extreme Tall")
	if err := scene.ConfigureAppearance(sceneappearance.Prepared{
		Generation: 4, Revision: 7,
		BackgroundMode:  sceneappearance.BackgroundModeImage,
		Background:      background,
		HeaderTitleMode: sceneappearance.HeaderTitleModeDefault,
	}); err != nil {
		t.Fatal(err)
	}
	frame, err := scene.RenderSize(854, 480, now)
	if err != nil {
		t.Fatal(err)
	}
	// The left gutter is not covered by panels, so this pixel is a direct
	// witness that the 1x2 custom source produced a non-empty centered crop.
	if got := frame.RGBAAt(5, 200); got != want {
		t.Fatalf("extreme-aspect custom background pixel = %#v, want %#v", got, want)
	}
}

func TestSceneAppearancePreparesFixedFramesAndCustomTitleAtStartBoundary(t *testing.T) {
	now := time.Date(2026, 9, 1, 1, 2, 3, 0, time.UTC)
	background := image.NewRGBA(image.Rect(0, 0, 12, 6))
	fillRect(background, background.Bounds(), color.RGBA{R: 180, G: 90, B: 20, A: 255})
	prepared := sceneappearance.Prepared{
		Generation: 4, Revision: 7,
		BackgroundMode: sceneappearance.BackgroundModeImage, Background: background,
		HeaderTitleMode: sceneappearance.HeaderTitleModeCustom, CustomTitle: "Operations Live",
	}

	first := newTestScene(t, 854, 480, now)
	second := newTestScene(t, 854, 480, now)
	t.Cleanup(first.Close)
	t.Cleanup(second.Close)
	first.Reset(1, "stream-01", "Name A")
	second.Reset(1, "stream-01", "A Different Stream Name")
	if err := first.ConfigureAppearance(prepared); err != nil {
		t.Fatal(err)
	}
	if err := second.ConfigureAppearance(prepared); err != nil {
		t.Fatal(err)
	}
	snapshot := first.Snapshot(now)
	if snapshot.appearance == nil || snapshot.appearance.customTitle != "Operations Live" || len(snapshot.appearance.backgrounds) != 3 {
		t.Fatalf("appearance was not fixed at start: %#v", snapshot.appearance)
	}
	var firstFrame *image.RGBA
	for _, size := range []image.Point{{X: 1920, Y: 1080}, {X: 1280, Y: 720}, {X: 854, Y: 480}} {
		frame, err := first.RenderSize(size.X, size.Y, now)
		if err != nil || frame.Bounds() != image.Rect(0, 0, size.X, size.Y) {
			t.Fatalf("target size %v render failed: bounds=%v err=%v", size, frame.Bounds(), err)
		}
		if got := frame.RGBAAt(5, size.Y/2); got.R < 150 || got.G < 60 || got.B > 60 {
			t.Fatalf("target size %v did not render custom background: %#v", size, got)
		}
		if size == (image.Point{X: 854, Y: 480}) {
			firstFrame = frame
		}
	}
	secondFrame, err := second.RenderSize(854, 480, now)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(firstFrame.Pix, secondFrame.Pix) {
		t.Fatal("custom header rendering depended on the legacy stream name")
	}
	repeat, err := first.RenderSize(854, 480, now)
	if err != nil || !bytes.Equal(firstFrame.Pix, repeat.Pix) {
		t.Fatalf("appearance render was nondeterministic: err=%v", err)
	}
	// The outer gutter remains uncovered by panels and witnesses that the
	// start-prepared custom background, rather than the legacy fill, rendered.
	if got := firstFrame.RGBAAt(5, 200); got.R < 150 || got.G < 60 || got.B > 60 {
		t.Fatalf("custom background was not rendered in the fixed layer: %#v", got)
	}
}

func TestSceneAppearanceCustomTitlePreservesDefaultBackground(t *testing.T) {
	now := time.Date(2026, 9, 1, 1, 2, 3, 0, time.UTC)
	legacy := newTestScene(t, 854, 480, now)
	custom := newTestScene(t, 854, 480, now)
	t.Cleanup(legacy.Close)
	t.Cleanup(custom.Close)
	legacy.Reset(1, "stream-01", "Legacy Name")
	custom.Reset(1, "stream-01", "Legacy Name")
	if err := custom.ConfigureAppearance(sceneappearance.Prepared{
		BackgroundMode:  sceneappearance.BackgroundModeDefault,
		HeaderTitleMode: sceneappearance.HeaderTitleModeCustom,
		CustomTitle:     "Custom Header",
	}); err != nil {
		t.Fatal(err)
	}
	legacyFrame, err := legacy.RenderSize(854, 480, now)
	if err != nil {
		t.Fatal(err)
	}
	customFrame, err := custom.RenderSize(854, 480, now)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(legacyFrame.Pix, customFrame.Pix) {
		t.Fatal("custom title did not change the rendered header")
	}
	if got := customFrame.RGBAAt(5, 200); got != backgroundColor {
		t.Fatalf("custom title changed the default background: got=%#v want=%#v", got, backgroundColor)
	}
}

func TestSceneAppearanceCustomTitleUsesDeterministicEllipsisBeforeClock(t *testing.T) {
	now := time.Date(2026, 9, 1, 1, 2, 3, 0, time.UTC)
	longTitle := strings.Repeat("W", 80)
	longScene := newTestScene(t, 854, 480, now)
	clippedScene := newTestScene(t, 854, 480, now)
	t.Cleanup(longScene.Close)
	t.Cleanup(clippedScene.Close)
	longScene.Reset(1, "stream-01", "ignored")
	clippedScene.Reset(1, "stream-01", "ignored")
	fonts, err := longScene.fontsForHeight(480)
	if err != nil {
		t.Fatal(err)
	}
	px := func(value int) int {
		result := int(float64(value)*480/defaultHeight + 0.5)
		if result < 1 {
			return 1
		}
		return result
	}
	clock := now.In(jstLocation()).Format("2006/01/02 15:04:05 JST")
	clockX := 854 - px(34) - measureText(fonts.body, clock)
	clippedTitle := truncateText(longTitle, clockX-px(34)-px(24), fonts.strong)
	if clippedTitle == longTitle || !strings.HasSuffix(clippedTitle, "…") {
		t.Fatalf("fixture did not require ellipsis: %q", clippedTitle)
	}
	for scene, title := range map[*Scene]string{longScene: longTitle, clippedScene: clippedTitle} {
		if err := scene.ConfigureAppearance(sceneappearance.Prepared{
			BackgroundMode:  sceneappearance.BackgroundModeDefault,
			HeaderTitleMode: sceneappearance.HeaderTitleModeCustom,
			CustomTitle:     title,
		}); err != nil {
			t.Fatal(err)
		}
	}
	longFrame, err := longScene.RenderSize(854, 480, now)
	if err != nil {
		t.Fatal(err)
	}
	clippedFrame, err := clippedScene.RenderSize(854, 480, now)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(longFrame.Pix, clippedFrame.Pix) {
		t.Fatal("rendered long title did not use the expected deterministic ellipsis")
	}
}

func TestSceneAppearanceCustomBackgroundPreservesDefaultHeader(t *testing.T) {
	now := time.Date(2026, 9, 1, 1, 2, 3, 0, time.UTC)
	legacy := newTestScene(t, 854, 480, now)
	custom := newTestScene(t, 854, 480, now)
	t.Cleanup(legacy.Close)
	t.Cleanup(custom.Close)
	legacy.Reset(1, "stream-01", "Legacy Name")
	custom.Reset(1, "stream-01", "Legacy Name")
	background := image.NewRGBA(image.Rect(0, 0, 2, 2))
	fillRect(background, background.Bounds(), color.RGBA{R: 100, G: 40, B: 10, A: 255})
	if err := custom.ConfigureAppearance(sceneappearance.Prepared{
		BackgroundMode:  sceneappearance.BackgroundModeImage,
		Background:      background,
		HeaderTitleMode: sceneappearance.HeaderTitleModeDefault,
	}); err != nil {
		t.Fatal(err)
	}
	legacyFrame, err := legacy.RenderSize(854, 480, now)
	if err != nil {
		t.Fatal(err)
	}
	customFrame, err := custom.RenderSize(854, 480, now)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(legacyFrame.Pix, customFrame.Pix) {
		t.Fatal("custom background did not change the rendered frame")
	}
	rowBytes := legacyFrame.Stride * 40
	if !bytes.Equal(legacyFrame.Pix[:rowBytes], customFrame.Pix[:rowBytes]) {
		t.Fatal("custom background changed the default title/header layer")
	}
}

func TestSceneAppearanceClearAndResetCannotLeakPredecessorVisuals(t *testing.T) {
	now := time.Date(2026, 9, 1, 1, 2, 3, 0, time.UTC)
	background := image.NewRGBA(image.Rect(0, 0, 2, 2))
	fillRect(background, background.Bounds(), color.RGBA{R: 255, A: 255})
	scene := newTestScene(t, 854, 480, now)
	fresh := newTestScene(t, 854, 480, now)
	t.Cleanup(scene.Close)
	t.Cleanup(fresh.Close)
	scene.Reset(1, "old-stream", "Old")
	if err := scene.ConfigureAppearance(sceneappearance.Prepared{
		BackgroundMode: sceneappearance.BackgroundModeImage, Background: background,
		HeaderTitleMode: sceneappearance.HeaderTitleModeCustom, CustomTitle: "Old Title",
	}); err != nil {
		t.Fatal(err)
	}
	scene.Clear("old-stream")
	scene.Reset(2, "new-stream", "New")
	fresh.Reset(2, "new-stream", "New")
	got, err := scene.RenderSize(854, 480, now)
	if err != nil {
		t.Fatal(err)
	}
	want, err := fresh.RenderSize(854, 480, now)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Pix, want.Pix) {
		t.Fatal("successor render retained predecessor appearance")
	}
}

func TestSceneAppearanceRequiredModesCannotSilentlyFallback(t *testing.T) {
	now := time.Date(2026, 9, 1, 1, 2, 3, 0, time.UTC)
	scene := newTestScene(t, 854, 480, now)
	t.Cleanup(scene.Close)
	scene.Reset(1, "stream-01", "Legacy")
	for _, test := range []struct {
		name     string
		prepared sceneappearance.Prepared
		want     sceneappearance.ErrorCode
	}{
		{
			name: "image without decoded background",
			prepared: sceneappearance.Prepared{
				BackgroundMode:  sceneappearance.BackgroundModeImage,
				HeaderTitleMode: sceneappearance.HeaderTitleModeDefault,
			},
			want: sceneappearance.CodeMediaAssetDecodeFailed,
		},
		{
			name: "custom title without title",
			prepared: sceneappearance.Prepared{
				BackgroundMode:  sceneappearance.BackgroundModeDefault,
				HeaderTitleMode: sceneappearance.HeaderTitleModeCustom,
			},
			want: sceneappearance.CodeRevisionPayloadConflict,
		},
		{
			name: "unknown mode",
			prepared: sceneappearance.Prepared{
				BackgroundMode:  "automatic",
				HeaderTitleMode: sceneappearance.HeaderTitleModeDefault,
			},
			want: sceneappearance.CodeRevisionPayloadConflict,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := scene.ConfigureAppearance(test.prepared)
			code, ok := sceneappearance.CodeOf(err)
			if !ok || code != test.want {
				t.Fatalf("configure error=%v code=%q, want %q", err, code, test.want)
			}
		})
	}
	if got := scene.Snapshot(now); got.appearance != nil {
		t.Fatal("failed required appearance installed a default fallback")
	}
}

func fillRect(destination *image.RGBA, rect image.Rectangle, fill color.RGBA) {
	for y := rect.Min.Y; y < rect.Max.Y; y++ {
		for x := rect.Min.X; x < rect.Max.X; x++ {
			destination.SetRGBA(x, y, fill)
		}
	}
}

func assertMostlyGreen(t *testing.T, got color.RGBA) {
	t.Helper()
	if got.G < 230 || got.R > 25 || got.B > 25 {
		t.Fatalf("center crop color = %#v, want green", got)
	}
}
