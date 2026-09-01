package sceneappearance

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"testing"
	"time"
)

func TestRuntimeOmissionAndExplicitDefaultNeverFetch(t *testing.T) {
	fetches := 0
	runtime := New(FetcherFunc(func(context.Context, string, AssetDescriptor) (AssetResponse, error) {
		fetches++
		return AssetResponse{}, errors.New("must not fetch")
	}), Options{})
	t.Cleanup(runtime.Close)

	omitted, err := runtime.Prepare(t.Context(), "stream-01", nil)
	if err != nil || !omitted.IsDefault() {
		t.Fatalf("omitted appearance changed legacy default: prepared=%#v err=%v", omitted, err)
	}
	explicit, err := runtime.Prepare(t.Context(), "stream-01", &Snapshot{
		Generation: 9, Revision: 4, Capability: Capability, Readiness: ReadinessReady,
		BackgroundMode: BackgroundModeDefault, HeaderTitleMode: HeaderTitleModeDefault,
	})
	if err != nil || !explicit.IsDefault() || explicit.Generation != 9 || explicit.Revision != 4 {
		t.Fatalf("explicit default was not prepared: prepared=%#v err=%v", explicit, err)
	}
	if fetches != 0 {
		t.Fatalf("default appearance fetched an asset %d times", fetches)
	}
}

func TestRuntimeDecodesEveryContractMediaType(t *testing.T) {
	pngBody := testPNG(t, 4, 2, color.RGBA{R: 10, A: 255})
	jpegImage := image.NewRGBA(image.Rect(0, 0, 3, 2))
	for y := 0; y < 2; y++ {
		for x := 0; x < 3; x++ {
			jpegImage.SetRGBA(x, y, color.RGBA{G: 120, A: 255})
		}
	}
	var jpegOutput bytes.Buffer
	if err := jpeg.Encode(&jpegOutput, jpegImage, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	webpBody, err := base64.StdEncoding.DecodeString("UklGRrIBAABXRUJQVlA4TKUBAAAvSsAYAA8w//M///MfeJAkbXvaSG7m8Q3GfYSBJekwQztm/IcZlgwnmWImn2BK7aFmBtnVir6q//8VOkFE/xm4baTIu8c48ArEo6+B3zFKYln3pqClSCKX0begFTAXFOLXHSyF8cCNcZEG4OywuA4KVVfJCiArU7GAgJI8+lJP/OKMT/fBAjevg1cYB7YVkFuWga2lyPi5I0HFy5YTpWIHg0RZpkniRVW9odHAKOwosWuOGdxIyn2OvaCDvhg/we6TwadPBPbqBV58MsLmMJ8yZnOWk8SRz4N+QoyPL+MnamzMvcE1rHNEr91F9GKZPVUcS9w7PhhH36suB9qPeYb/oLk6cuTiJ0wOK3m5h1cKjW6EVZCYMK7dxcKCBdgP9HkKr9gkAO2P8GKZGWVdIAatQa+1IDpt6qyorVwdy01xdW8Jkfk6xjEXmVQQ+HQdFr6OKhIN34dXWq0+0qr6EJSCeeVLH9+gvGTLyqM65PQ44ihzlTXxQKjKbAvshXgir7Lil9w4L2bvMycmjQcqXaMCO6BlY28i+FOLzbfI1vEqxAhotocAAA==")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name      string
		mediaType string
		width     int
		height    int
		body      []byte
	}{
		{name: "png", mediaType: MediaTypePNG, width: 4, height: 2, body: pngBody},
		{name: "jpeg", mediaType: MediaTypeJPEG, width: 3, height: 2, body: jpegOutput.Bytes()},
		{name: "webp", mediaType: MediaTypeWebP, width: 75, height: 100, body: webpBody},
	} {
		t.Run(test.name, func(t *testing.T) {
			descriptor := testDescriptor(test.body, test.width, test.height, 1)
			descriptor.MediaType = test.mediaType
			runtime := New(FetcherFunc(func(context.Context, string, AssetDescriptor) (AssetResponse, error) {
				return testResponse(test.body, descriptor), nil
			}), Options{})
			t.Cleanup(runtime.Close)
			snapshot := testImageSnapshot(descriptor)
			prepared, err := runtime.Prepare(t.Context(), "stream-01", &snapshot)
			if err != nil || prepared.Background == nil || prepared.Background.Bounds().Dx() != test.width || prepared.Background.Bounds().Dy() != test.height {
				t.Fatalf("%s decode failed: bounds=%v err=%v", test.mediaType, prepared.Background, err)
			}
		})
	}
}

func TestRuntimeSanitizesCustomTitleAndRejectsRequiredCustomFallback(t *testing.T) {
	runtime := New(nil, Options{})
	t.Cleanup(runtime.Close)
	snapshot := Snapshot{
		Generation: 1, Revision: 1, Capability: Capability, Readiness: ReadinessReady,
		BackgroundMode: BackgroundModeDefault, HeaderTitleMode: HeaderTitleModeCustom,
		CustomTitle: "  Production\tLive  ",
	}
	prepared, err := runtime.Prepare(t.Context(), "stream-01", &snapshot)
	if err != nil || prepared.CustomTitle != "Production Live" {
		t.Fatalf("custom title was not sanitized deterministically: prepared=%#v err=%v", prepared, err)
	}

	for _, title := range []string{"", " \t ", "line one\nline two"} {
		snapshot.CustomTitle = title
		_, err := runtime.Prepare(t.Context(), "stream-01", &snapshot)
		assertCode(t, err, CodeRevisionPayloadConflict)
	}
}

func TestRuntimeFetchesValidAssetOnceAndRejectsSameRevisionMutation(t *testing.T) {
	body := testPNG(t, 4, 2, color.RGBA{R: 10, G: 20, B: 30, A: 255})
	descriptor := testDescriptor(body, 4, 2, 7)
	fetches := 0
	runtime := New(FetcherFunc(func(_ context.Context, streamID string, got AssetDescriptor) (AssetResponse, error) {
		fetches++
		if streamID != "stream-01" || got != descriptor {
			t.Fatalf("unsafe or incorrect fetch input: stream=%q descriptor=%#v", streamID, got)
		}
		return testResponse(body, descriptor), nil
	}), Options{MaxEntries: 2})
	t.Cleanup(runtime.Close)
	snapshot := testImageSnapshot(descriptor)

	first, err := runtime.Prepare(t.Context(), "stream-01", &snapshot)
	if err != nil || first.Background == nil {
		t.Fatalf("valid background was not prepared: prepared=%#v err=%v", first, err)
	}
	second, err := runtime.Prepare(t.Context(), "stream-01", &snapshot)
	if err != nil || second.Background == nil {
		t.Fatalf("cached background was not prepared: prepared=%#v err=%v", second, err)
	}
	if fetches != 1 {
		t.Fatalf("immutable revision fetched %d times, want 1", fetches)
	}

	mutated := snapshot
	mutatedDescriptor := *snapshot.Background
	mutatedDescriptor.SHA256 = string(bytes.Repeat([]byte{'b'}, 64))
	mutated.Background = &mutatedDescriptor
	_, err = runtime.Prepare(t.Context(), "stream-01", &mutated)
	assertCode(t, err, CodeRevisionPayloadConflict)
	if fetches != 1 {
		t.Fatalf("same revision mutation bypassed cache conflict: fetches=%d", fetches)
	}
}

func TestRuntimeRejectsUnsupportedCapabilityBeforeFetch(t *testing.T) {
	fetches := 0
	runtime := New(FetcherFunc(func(context.Context, string, AssetDescriptor) (AssetResponse, error) {
		fetches++
		return AssetResponse{}, nil
	}), Options{})
	t.Cleanup(runtime.Close)
	snapshot := Snapshot{
		Generation: 1, Revision: 1, Capability: "scene_appearance_v2", Readiness: ReadinessReady,
		BackgroundMode: BackgroundModeDefault, HeaderTitleMode: HeaderTitleModeDefault,
	}
	_, err := runtime.Prepare(t.Context(), "stream-01", &snapshot)
	assertCode(t, err, CodeCapabilityRequired)
	if fetches != 0 {
		t.Fatalf("unsupported capability reached fetcher %d times", fetches)
	}
}

func TestRuntimeFencesSceneSnapshotGenerationAndAllowsRestartReconcile(t *testing.T) {
	firstRuntime := New(nil, Options{})
	first := Snapshot{
		Generation: 10, Revision: 2, Capability: Capability, Readiness: ReadinessReady,
		BackgroundMode: BackgroundModeDefault, HeaderTitleMode: HeaderTitleModeCustom,
		CustomTitle: "First",
	}
	if _, err := firstRuntime.Prepare(t.Context(), "stream-01", &first); err != nil {
		t.Fatal(err)
	}
	mutated := first
	mutated.Revision++
	mutated.CustomTitle = "Mutated"
	_, err := firstRuntime.Prepare(t.Context(), "stream-01", &mutated)
	assertCode(t, err, CodeRevisionPayloadConflict)
	stale := first
	stale.Generation--
	_, err = firstRuntime.Prepare(t.Context(), "stream-01", &stale)
	assertCode(t, err, CodeStaleJobGeneration)
	firstRuntime.Close()

	// A process restart has no in-memory generation binding. Reconciliation of
	// the Control Panel's still-authoritative generation must remain possible.
	restarted := New(nil, Options{})
	t.Cleanup(restarted.Close)
	if _, err := restarted.Prepare(t.Context(), "stream-01", &first); err != nil {
		t.Fatalf("restart reconcile rejected authoritative snapshot: %v", err)
	}
}

func TestRuntimeAtomicallyInvalidatesSuccessfulVariantRevision(t *testing.T) {
	oldBody := testPNG(t, 4, 2, color.RGBA{R: 200, A: 255})
	newBody := testPNG(t, 4, 2, color.RGBA{G: 200, A: 255})
	oldDescriptor := testDescriptor(oldBody, 4, 2, 1)
	newDescriptor := testDescriptor(newBody, 4, 2, 2)
	newDescriptor.VariantID = "variant-02"
	fetches := map[string]int{}
	runtime := New(FetcherFunc(func(_ context.Context, _ string, descriptor AssetDescriptor) (AssetResponse, error) {
		fetches[descriptor.VariantID]++
		if descriptor.VariantID == newDescriptor.VariantID {
			return testResponse(newBody, descriptor), nil
		}
		return testResponse(oldBody, descriptor), nil
	}), Options{})
	t.Cleanup(runtime.Close)
	oldSnapshot := testImageSnapshot(oldDescriptor)
	newSnapshot := testImageSnapshot(newDescriptor)
	newSnapshot.Generation = oldSnapshot.Generation + 1
	if _, err := runtime.Prepare(t.Context(), "stream-01", &oldSnapshot); err != nil {
		t.Fatal(err)
	}
	prepared, err := runtime.Prepare(t.Context(), "stream-01", &newSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	got := color.RGBAModel.Convert(prepared.Background.At(0, 0)).(color.RGBA)
	if got.G <= got.R {
		t.Fatalf("new variant did not replace old decoded content: %#v", got)
	}
	runtime.mu.Lock()
	oldCached := runtime.cache[cacheKey{streamID: "stream-01", generation: oldSnapshot.Generation, assetID: oldDescriptor.AssetID, variantID: oldDescriptor.VariantID, revision: oldDescriptor.Revision}]
	newCached := runtime.cache[cacheKey{streamID: "stream-01", generation: newSnapshot.Generation, assetID: newDescriptor.AssetID, variantID: newDescriptor.VariantID, revision: newDescriptor.Revision}]
	runtime.mu.Unlock()
	if oldCached != nil || newCached == nil {
		t.Fatalf("atomic cache replacement retained old=%v new=%v", oldCached != nil, newCached != nil)
	}
	if fetches[oldDescriptor.VariantID] != 1 || fetches[newDescriptor.VariantID] != 1 {
		t.Fatalf("cache invalidation mismatch: %#v", fetches)
	}
}

func TestRuntimeFailedNewRevisionNeverFallsBackToLastGood(t *testing.T) {
	oldBody := testPNG(t, 4, 2, color.RGBA{R: 1, A: 255})
	newBody := testPNG(t, 4, 2, color.RGBA{G: 2, A: 255})
	oldDescriptor := testDescriptor(oldBody, 4, 2, 1)
	newDescriptor := testDescriptor(newBody, 4, 2, 2)
	fetches := map[uint64]int{}
	runtime := New(FetcherFunc(func(_ context.Context, _ string, descriptor AssetDescriptor) (AssetResponse, error) {
		fetches[descriptor.Revision]++
		if descriptor.Revision == 2 {
			response := testResponse(newBody, descriptor)
			response.Body = append([]byte(nil), newBody...)
			response.Body[len(response.Body)-1] ^= 0xff
			return response, nil
		}
		return testResponse(oldBody, descriptor), nil
	}), Options{})
	t.Cleanup(runtime.Close)

	oldSnapshot := testImageSnapshot(oldDescriptor)
	if prepared, err := runtime.Prepare(t.Context(), "stream-01", &oldSnapshot); err != nil || prepared.Background == nil {
		t.Fatalf("old revision did not prepare: prepared=%#v err=%v", prepared, err)
	}
	newSnapshot := testImageSnapshot(newDescriptor)
	newSnapshot.Generation = oldSnapshot.Generation + 1
	prepared, err := runtime.Prepare(t.Context(), "stream-01", &newSnapshot)
	assertCode(t, err, CodeMediaAssetHashMismatch)
	if prepared.Background != nil {
		t.Fatal("failed new revision returned a last-good background")
	}
	if fetches[1] != 1 || fetches[2] != 1 {
		t.Fatalf("unexpected revision fetches: %#v", fetches)
	}
}

func TestRuntimeCacheNeverBypassesAnotherStreamAuthorization(t *testing.T) {
	body := testPNG(t, 4, 2, color.RGBA{R: 4, G: 5, B: 6, A: 255})
	descriptor := testDescriptor(body, 4, 2, 1)
	fetches := 0
	runtime := New(FetcherFunc(func(_ context.Context, streamID string, _ AssetDescriptor) (AssetResponse, error) {
		fetches++
		if streamID == "stream-02" {
			return AssetResponse{}, NewError(CodeMediaAssetUnauthorized)
		}
		return testResponse(body, descriptor), nil
	}), Options{})
	t.Cleanup(runtime.Close)
	snapshot := testImageSnapshot(descriptor)
	if _, err := runtime.Prepare(t.Context(), "stream-01", &snapshot); err != nil {
		t.Fatal(err)
	}
	_, err := runtime.Prepare(t.Context(), "stream-02", &snapshot)
	assertCode(t, err, CodeMediaAssetUnauthorized)
	if fetches != 2 {
		t.Fatalf("cross-stream cache bypassed authorization: fetches=%d", fetches)
	}
}

func TestRuntimeNewStartGenerationRevalidatesStreamSnapshot(t *testing.T) {
	body := testPNG(t, 4, 2, color.RGBA{R: 7, G: 8, B: 9, A: 255})
	descriptor := testDescriptor(body, 4, 2, 1)
	fetches := 0
	runtime := New(FetcherFunc(func(context.Context, string, AssetDescriptor) (AssetResponse, error) {
		fetches++
		return testResponse(body, descriptor), nil
	}), Options{})
	t.Cleanup(runtime.Close)
	first := testImageSnapshot(descriptor)
	if _, err := runtime.Prepare(t.Context(), "stream-01", &first); err != nil {
		t.Fatal(err)
	}
	second := testImageSnapshot(descriptor)
	second.Generation = first.Generation + 1
	if _, err := runtime.Prepare(t.Context(), "stream-01", &second); err != nil {
		t.Fatal(err)
	}
	if fetches != 2 {
		t.Fatalf("new start generation reused prior authorization snapshot: fetches=%d", fetches)
	}
}

func TestRuntimeMapsFaultsToCanonicalSecretSafeCodes(t *testing.T) {
	body := testPNG(t, 4, 2, color.RGBA{B: 3, A: 255})
	descriptor := testDescriptor(body, 4, 2, 1)
	tests := []struct {
		name  string
		fetch func(context.Context, string, AssetDescriptor) (AssetResponse, error)
		want  ErrorCode
	}{
		{
			name: "unauthorized",
			fetch: func(context.Context, string, AssetDescriptor) (AssetResponse, error) {
				return AssetResponse{}, NewError(CodeMediaAssetUnauthorized)
			},
			want: CodeMediaAssetUnauthorized,
		},
		{
			name: "untyped failure is redacted",
			fetch: func(context.Context, string, AssetDescriptor) (AssetResponse, error) {
				return AssetResponse{}, errors.New("Bearer secret-token C:\\private\\asset.png https://forbidden.example")
			},
			want: CodeMediaAssetVariantFailed,
		},
		{
			name: "timeout",
			fetch: func(ctx context.Context, _ string, _ AssetDescriptor) (AssetResponse, error) {
				<-ctx.Done()
				return AssetResponse{}, ctx.Err()
			},
			want: CodeMediaAssetTimeout,
		},
		{
			name: "dimension mismatch",
			fetch: func(context.Context, string, AssetDescriptor) (AssetResponse, error) {
				response := testResponse(body, descriptor)
				response.Width++
				return response, nil
			},
			want: CodeMediaAssetDimensionMismatch,
		},
		{
			name: "content type mismatch",
			fetch: func(context.Context, string, AssetDescriptor) (AssetResponse, error) {
				response := testResponse(body, descriptor)
				response.MediaType = MediaTypeJPEG
				return response, nil
			},
			want: CodeMediaAssetFormatUnsupported,
		},
		{
			name: "response digest mismatch",
			fetch: func(context.Context, string, AssetDescriptor) (AssetResponse, error) {
				response := testResponse(body, descriptor)
				response.SHA256 = string(bytes.Repeat([]byte{'c'}, 64))
				return response, nil
			},
			want: CodeMediaAssetHashMismatch,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runtime := New(FetcherFunc(test.fetch), Options{FetchTimeout: 20 * time.Millisecond})
			t.Cleanup(runtime.Close)
			snapshot := testImageSnapshot(descriptor)
			_, err := runtime.Prepare(t.Context(), "stream-01", &snapshot)
			assertCode(t, err, test.want)
			if err == nil || err.Error() != string(test.want) {
				t.Fatalf("error exposed noncanonical detail: %v", err)
			}
		})
	}
}

func TestRuntimeRejectsCorruptDecodableMediaWithoutFallback(t *testing.T) {
	corrupt := append([]byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}, bytes.Repeat([]byte{0x41}, 32)...)
	descriptor := testDescriptor(corrupt, 4, 2, 1)
	runtime := New(FetcherFunc(func(context.Context, string, AssetDescriptor) (AssetResponse, error) {
		return testResponse(corrupt, descriptor), nil
	}), Options{})
	t.Cleanup(runtime.Close)
	snapshot := testImageSnapshot(descriptor)
	prepared, err := runtime.Prepare(t.Context(), "stream-01", &snapshot)
	assertCode(t, err, CodeMediaAssetDecodeFailed)
	if prepared.Background != nil {
		t.Fatal("corrupt media produced a background")
	}
}

func TestRuntimeRejectsDescriptorBoundsBeforeFetch(t *testing.T) {
	body := testPNG(t, 2, 2, color.RGBA{A: 255})
	descriptor := testDescriptor(body, 2, 2, 1)
	fetches := 0
	runtime := New(FetcherFunc(func(context.Context, string, AssetDescriptor) (AssetResponse, error) {
		fetches++
		return AssetResponse{}, nil
	}), Options{})
	t.Cleanup(runtime.Close)

	tooLarge := descriptor
	tooLarge.ByteSize = maxAssetBytes + 1
	snapshot := testImageSnapshot(tooLarge)
	_, err := runtime.Prepare(t.Context(), "stream-01", &snapshot)
	assertCode(t, err, CodeMediaAssetTooLarge)

	badPixels := descriptor
	badPixels.PixelCount++
	snapshot = testImageSnapshot(badPixels)
	_, err = runtime.Prepare(t.Context(), "stream-01", &snapshot)
	assertCode(t, err, CodeMediaAssetDimensionMismatch)

	forbiddenAspect := 0
	badUsageShape := descriptor
	badUsageShape.AspectRatioErrorPPM = &forbiddenAspect
	snapshot = testImageSnapshot(badUsageShape)
	_, err = runtime.Prepare(t.Context(), "stream-01", &snapshot)
	assertCode(t, err, CodeRevisionPayloadConflict)
	if fetches != 0 {
		t.Fatalf("invalid descriptors reached fetcher %d times", fetches)
	}
}

func TestRuntimeCloseZerosCachedPixelsAndRejectsReuse(t *testing.T) {
	body := testPNG(t, 2, 2, color.RGBA{R: 250, G: 10, B: 20, A: 255})
	descriptor := testDescriptor(body, 2, 2, 1)
	runtime := New(FetcherFunc(func(context.Context, string, AssetDescriptor) (AssetResponse, error) {
		return testResponse(body, descriptor), nil
	}), Options{})
	snapshot := testImageSnapshot(descriptor)
	prepared, err := runtime.Prepare(t.Context(), "stream-01", &snapshot)
	if err != nil {
		t.Fatal(err)
	}
	cached, ok := prepared.Background.(*image.RGBA)
	if !ok || allZero(cached.Pix) {
		t.Fatalf("unexpected cached image: %T", prepared.Background)
	}
	runtime.Close()
	if !allZero(cached.Pix) {
		t.Fatal("Close retained decoded cache pixels")
	}
	_, err = runtime.Prepare(t.Context(), "stream-01", nil)
	assertCode(t, err, CodeCapabilityRequired)
}

func testImageSnapshot(descriptor AssetDescriptor) Snapshot {
	return Snapshot{
		Generation: 3, Revision: 5, Capability: Capability, Readiness: ReadinessReady,
		BackgroundMode: BackgroundModeImage, Background: &descriptor,
		HeaderTitleMode: HeaderTitleModeDefault,
	}
}

func testDescriptor(body []byte, width, height int, revision uint64) AssetDescriptor {
	digest := sha256.Sum256(body)
	return AssetDescriptor{
		AssetID: "asset-01", VariantID: "variant-01", Usage: UsageSceneBackground,
		MediaType: MediaTypePNG, Width: width, Height: height,
		ByteSize: int64(len(body)), PixelCount: int64(width * height), Animated: false,
		SHA256: hex.EncodeToString(digest[:]), Revision: revision, Readiness: ReadinessReady,
	}
}

func testResponse(body []byte, descriptor AssetDescriptor) AssetResponse {
	return AssetResponse{
		AssetID: descriptor.AssetID, VariantID: descriptor.VariantID,
		MediaType: descriptor.MediaType, Width: descriptor.Width, Height: descriptor.Height,
		ByteSize: int64(len(body)), SHA256: descriptor.SHA256, Body: append([]byte(nil), body...),
	}
}

func testPNG(t *testing.T, width, height int, fill color.RGBA) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.SetRGBA(x, y, fill)
		}
	}
	var output bytes.Buffer
	if err := png.Encode(&output, img); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func assertCode(t *testing.T, err error, want ErrorCode) {
	t.Helper()
	got, ok := CodeOf(err)
	if !ok || got != want {
		t.Fatalf("error code = %q ok=%v, want %q (err=%v)", got, ok, want, err)
	}
}

func allZero(value []byte) bool {
	for _, current := range value {
		if current != 0 {
			return false
		}
	}
	return true
}
