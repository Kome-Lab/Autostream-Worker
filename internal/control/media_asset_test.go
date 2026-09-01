package control

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/example/autostream-worker/internal/sceneappearance"
)

func TestFetchSceneAssetUsesAuthenticatedStreamBoundEndpointAndSafeMetadata(t *testing.T) {
	body, descriptor := controlAssetFixture(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/internal/streams/stream-01/media-assets/variant-01" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer runtime-service-token" {
			t.Fatalf("authorization = %q", got)
		}
		if got := r.Header.Get("Accept"); got != sceneappearance.MediaTypePNG {
			t.Fatalf("Accept = %q", got)
		}
		writeAssetResponse(t, w, body, descriptor)
	}))
	defer server.Close()

	client := Client{Config: Config{ControlPanelURL: server.URL, Token: "runtime-service-token", ServiceID: "worker-01"}}
	response, err := client.FetchSceneAsset(t.Context(), "stream-01", descriptor)
	if err != nil {
		t.Fatal(err)
	}
	if response.AssetID != descriptor.AssetID || response.VariantID != descriptor.VariantID || response.SHA256 != descriptor.SHA256 || !bytes.Equal(response.Body, body) {
		t.Fatalf("unexpected safe fetch response: %#v", response)
	}
}

func TestFetchSceneAssetMapsStatusAndNeverExposesBodyOrToken(t *testing.T) {
	_, descriptor := controlAssetFixture(t)
	for _, test := range []struct {
		status int
		want   sceneappearance.ErrorCode
	}{
		{http.StatusUnauthorized, sceneappearance.CodeMediaAssetUnauthorized},
		{http.StatusForbidden, sceneappearance.CodeMediaAssetUnauthorized},
		{http.StatusNotFound, sceneappearance.CodeMediaAssetNotFound},
		{http.StatusConflict, sceneappearance.CodeMediaAssetHashMismatch},
		{http.StatusGatewayTimeout, sceneappearance.CodeMediaAssetTimeout},
		{http.StatusRequestEntityTooLarge, sceneappearance.CodeMediaAssetTooLarge},
		{http.StatusUnsupportedMediaType, sceneappearance.CodeMediaAssetFormatUnsupported},
		{http.StatusInternalServerError, sceneappearance.CodeMediaAssetVariantFailed},
	} {
		t.Run(http.StatusText(test.status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(test.status)
				_, _ = w.Write([]byte(`{"storage_key":"C:\\private\\asset.png","token":"runtime-service-token","url":"https://forbidden.example"}`))
			}))
			defer server.Close()
			client := Client{Config: Config{ControlPanelURL: server.URL, Token: "runtime-service-token", ServiceID: "worker-01"}}
			_, err := client.FetchSceneAsset(t.Context(), "stream-01", descriptor)
			code, ok := sceneappearance.CodeOf(err)
			if !ok || code != test.want || err.Error() != string(test.want) {
				t.Fatalf("status %d error=%v code=%q ok=%v", test.status, err, code, ok)
			}
			for _, forbidden := range []string{"runtime-service-token", "private", "forbidden.example", "storage_key"} {
				if strings.Contains(err.Error(), forbidden) {
					t.Fatalf("fetch error leaked %q: %v", forbidden, err)
				}
			}
		})
	}
}

func TestFetchSceneAssetNeverFollowsRedirectWithCustomClient(t *testing.T) {
	_, descriptor := controlAssetFixture(t)
	targetHits := 0
	targetAuth := ""
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetHits++
		targetAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/capture", http.StatusFound)
	}))
	defer redirect.Close()

	client := Client{
		Config: Config{ControlPanelURL: redirect.URL, Token: "runtime-service-token", ServiceID: "worker-01"},
		HTTP:   &http.Client{Timeout: time.Second},
	}
	_, err := client.FetchSceneAsset(t.Context(), "stream-01", descriptor)
	code, ok := sceneappearance.CodeOf(err)
	if !ok || code != sceneappearance.CodeMediaAssetVariantFailed {
		t.Fatalf("redirect error=%v code=%q", err, code)
	}
	if targetHits != 0 || targetAuth != "" {
		t.Fatalf("redirect target received request/token: hits=%d auth=%q", targetHits, targetAuth)
	}
}

func TestFetchSceneAssetHonorsCallerDeadline(t *testing.T) {
	_, descriptor := controlAssetFixture(t)
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		time.Sleep(100 * time.Millisecond)
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancel()
	client := Client{Config: Config{ControlPanelURL: server.URL, Token: "runtime-service-token", ServiceID: "worker-01"}}
	_, err := client.FetchSceneAsset(ctx, "stream-01", descriptor)
	code, ok := sceneappearance.CodeOf(err)
	if !ok || code != sceneappearance.CodeMediaAssetTimeout {
		t.Fatalf("deadline error=%v code=%q", err, code)
	}
}

func controlAssetFixture(t *testing.T) ([]byte, sceneappearance.AssetDescriptor) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 4, 2))
	for y := 0; y < 2; y++ {
		for x := 0; x < 4; x++ {
			img.SetRGBA(x, y, color.RGBA{R: 20, G: 40, B: 60, A: 255})
		}
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, img); err != nil {
		t.Fatal(err)
	}
	body := encoded.Bytes()
	digest := sha256.Sum256(body)
	descriptor := sceneappearance.AssetDescriptor{
		AssetID: "asset-01", VariantID: "variant-01", Usage: sceneappearance.UsageSceneBackground,
		MediaType: sceneappearance.MediaTypePNG, Width: 4, Height: 2,
		ByteSize: int64(len(body)), PixelCount: 8, SHA256: hex.EncodeToString(digest[:]),
		Revision: 1, Readiness: sceneappearance.ReadinessReady,
	}
	return body, descriptor
}

func writeAssetResponse(t *testing.T, w http.ResponseWriter, body []byte, descriptor sceneappearance.AssetDescriptor) {
	t.Helper()
	digest, err := hex.DecodeString(descriptor.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	w.Header().Set("Content-Type", descriptor.MediaType)
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.Header().Set("Digest", "sha-256="+base64.StdEncoding.EncodeToString(digest))
	w.Header().Set("X-AutoStream-Asset-ID", descriptor.AssetID)
	w.Header().Set("X-AutoStream-Variant-ID", descriptor.VariantID)
	w.Header().Set("X-AutoStream-Width", strconv.Itoa(descriptor.Width))
	w.Header().Set("X-AutoStream-Height", strconv.Itoa(descriptor.Height))
	_, _ = w.Write(body)
}
