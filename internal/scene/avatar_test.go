package scene

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestAvatarFetcherAllowsOnlyDiscordHTTPSAndRevalidatesRedirects(t *testing.T) {
	if err := validateAvatarURL("http://cdn.discordapp.com/avatars/u/a.png"); err == nil {
		t.Fatal("expected plain HTTP to be rejected")
	}
	if err := validateAvatarURL("https://cdn.discordapp.com.evil.example/avatars/u/a.png"); err == nil {
		t.Fatal("expected a lookalike host to be rejected")
	}
	requests := atomic.Int32{}
	cache := newAvatarCache(AvatarConfig{HTTPClient: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		requests.Add(1)
		return &http.Response{StatusCode: http.StatusFound, Header: http.Header{"Location": []string{"https://evil.example/avatar.png"}}, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
	})}})
	if _, err := cache.fetch(t.Context(), "https://cdn.discordapp.com/avatars/u/a.png"); err == nil {
		t.Fatal("expected an off-CDN redirect to be rejected")
	}
	if requests.Load() != 1 {
		t.Fatalf("redirect target was requested: %d requests", requests.Load())
	}
}

func TestAvatarFetcherBoundsBytesAndDimensions(t *testing.T) {
	large := bytes.Repeat([]byte("x"), defaultAvatarMaxBytes+1)
	cache := newAvatarCache(AvatarConfig{HTTPClient: staticImageClient(large), MaxBytes: defaultAvatarMaxBytes})
	if _, err := cache.fetch(t.Context(), "https://cdn.discordapp.com/avatars/u/large.png"); err == nil {
		t.Fatal("expected oversized body to be rejected")
	}

	var encoded bytes.Buffer
	if err := png.Encode(&encoded, image.NewRGBA(image.Rect(0, 0, defaultAvatarMaxDimension+1, 1))); err != nil {
		t.Fatal(err)
	}
	cache = newAvatarCache(AvatarConfig{HTTPClient: staticImageClient(encoded.Bytes()), MaxDimension: defaultAvatarMaxDimension})
	if _, err := cache.fetch(t.Context(), "https://media.discordapp.net/avatars/u/wide.png"); err == nil {
		t.Fatal("expected oversized dimensions to be rejected")
	}
}

func TestAvatarCacheBoundsConcurrencyAndEntries(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{}, 8)
	var active atomic.Int32
	var maxActive atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		current := active.Add(1)
		for {
			previous := maxActive.Load()
			if current <= previous || maxActive.CompareAndSwap(previous, current) {
				break
			}
		}
		started <- struct{}{}
		<-release
		active.Add(-1)
		return imageResponse(r, onePixelPNG(t)), nil
	})}
	cache := newAvatarCache(AvatarConfig{HTTPClient: client, MaxConcurrent: 2, MaxEntries: 3, SuccessTTL: time.Hour})
	for i := 0; i < 6; i++ {
		cache.Prefetch("https://cdn.discordapp.com/avatars/u/" + string(rune('a'+i)) + ".png")
	}
	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("avatar fetch did not start")
		}
	}
	select {
	case <-started:
		t.Fatal("concurrency bound was exceeded before a slot was released")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		cache.mu.Lock()
		entries := len(cache.entries)
		inflight := len(cache.inflight)
		cache.mu.Unlock()
		if inflight == 0 {
			if entries > 3 {
				t.Fatalf("cache exceeded entry bound: %d", entries)
			}
			if maxActive.Load() > 2 {
				t.Fatalf("fetch concurrency exceeded bound: %d", maxActive.Load())
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("avatar fetches did not finish")
}

func staticImageClient(body []byte) *http.Client {
	return &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"image/png"}}, Body: io.NopCloser(bytes.NewReader(body)), Request: r}, nil
	})}
}

func imageResponse(r *http.Request, body []byte) *http.Response {
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"image/png"}}, Body: io.NopCloser(bytes.NewReader(body)), Request: r}
}

func onePixelPNG(t *testing.T) []byte {
	t.Helper()
	var encoded bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, 1, 1))
	img.Set(0, 0, color.White)
	if err := png.Encode(&encoded, img); err != nil {
		t.Fatal(err)
	}
	return encoded.Bytes()
}
