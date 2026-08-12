package scene

import (
	"image"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/example/autostream-worker/internal/events"
	"golang.org/x/image/font/gofont/goregular"
)

func TestNewRequiresExplicitValidFontFile(t *testing.T) {
	if _, err := New(Config{Width: 1920, Height: 1080}); err == nil {
		t.Fatal("expected a missing font file to fail")
	}
	invalid := filepath.Join(t.TempDir(), "invalid.ttf")
	if err := os.WriteFile(invalid, []byte("not a font"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(Config{Width: 1920, Height: 1080, FontFile: invalid}); err == nil {
		t.Fatal("expected an invalid font file to fail")
	}
}

func TestSceneReducesCanonicalEventsAndRendersSupportedSizes(t *testing.T) {
	now := time.Date(2026, 8, 12, 3, 4, 5, 0, time.UTC)
	for _, size := range []image.Point{{X: 1920, Y: 1080}, {X: 1280, Y: 720}, {X: 854, Y: 480}} {
		t.Run(size.String(), func(t *testing.T) {
			s := newTestScene(t, size.X, size.Y, now)
			s.Reset(1, "stream-01", "Dev")
			apply(t, s, events.CustomOverlayEvent("stream-01", "overlay.participants", map[string]any{
				"participants": []any{
					map[string]any{"user_id": "user-01", "display_name": "Alice", "avatar_url": "https://cdn.discordapp.com/avatars/user-01/a.png", "speaking": true},
					map[string]any{"user_id": "bot-02", "display_name": "Music Bot", "is_bot": true, "speaking": true},
				},
			}, now))
			apply(t, s, events.CustomOverlayEvent("stream-01", "overlay.discord_chat", map[string]any{
				"message_id": "message-01", "author_id": "canonical-user", "user_id": "legacy-user",
				"display_name": "Alice", "content": "canonical text", "text": "legacy text", "is_bot": false,
				"created_at": now.Format(time.RFC3339), "attachments": []any{map[string]any{"url": "https://example.com/secret.png"}},
			}, now))
			apply(t, s, events.CustomOverlayEvent("stream-01", "caption.telop", map[string]any{"text": "途中字幕", "speaker_user_id": "user-01"}, now))

			snapshot := s.Snapshot(now)
			if snapshot.StreamID != "stream-01" || snapshot.StreamName != "Dev" || len(snapshot.Participants) != 2 {
				t.Fatalf("unexpected snapshot: %#v", snapshot)
			}
			if !snapshot.Participants[0].Speaking || !snapshot.Participants[1].Speaking || !snapshot.Participants[1].IsBot {
				t.Fatalf("multiple speakers or bot flag was lost: %#v", snapshot.Participants)
			}
			if len(snapshot.Chat) != 1 || snapshot.Chat[0].AuthorID != "canonical-user" || snapshot.Chat[0].Content != "canonical text" {
				t.Fatalf("canonical chat fields were not preferred: %#v", snapshot.Chat)
			}
			if len(snapshot.Captions) != 1 || snapshot.Captions[0].Text != "途中字幕" || snapshot.Captions[0].Final {
				t.Fatalf("caption state was not applied: %#v", snapshot.Captions)
			}

			frame, err := s.Render(now)
			if err != nil {
				t.Fatal(err)
			}
			if frame.Bounds() != image.Rect(0, 0, size.X, size.Y) {
				t.Fatalf("unexpected frame bounds: %v", frame.Bounds())
			}
			if !containsSpeakingGreen(frame) {
				t.Fatal("rendered frame did not contain a speaking green border")
			}
		})
	}
}

func TestSceneBoundsDeduplicatesAndExpiresChatAndCaptions(t *testing.T) {
	now := time.Date(2026, 8, 12, 3, 4, 5, 0, time.UTC)
	s := newTestSceneWithConfig(t, Config{
		Width: 854, Height: 480, FontFile: writeTestFont(t), Now: func() time.Time { return now },
		MaxChat: 3, ChatTTL: time.Minute, MaxCaptions: 2, InterimCaptionTTL: 4 * time.Second, FinalCaptionTTL: 10 * time.Second,
	})
	s.Reset(1, "stream-01", "Dev")
	for i, id := range []string{"one", "two", "two", "three", "four"} {
		apply(t, s, events.CustomOverlayEvent("stream-01", "overlay.discord_chat", map[string]any{
			"message_id": id, "author_id": "user", "display_name": "Alice", "content": id,
		}, now.Add(time.Duration(i)*time.Second)))
	}
	for i, text := range []string{"first", "second", "third"} {
		apply(t, s, events.CustomOverlayEvent("stream-01", "caption.final", map[string]any{"text": text}, now.Add(time.Duration(i)*time.Second)))
	}

	snapshot := s.Snapshot(now.Add(5 * time.Second))
	if len(snapshot.Chat) != 3 || snapshot.Chat[0].MessageID != "two" || snapshot.Chat[2].MessageID != "four" {
		t.Fatalf("chat bound/deduplication failed: %#v", snapshot.Chat)
	}
	if len(snapshot.Captions) != 2 || snapshot.Captions[0].Text != "second" || snapshot.Captions[1].Text != "third" {
		t.Fatalf("caption bound failed: %#v", snapshot.Captions)
	}
	if got := s.Snapshot(now.Add(2 * time.Minute)); len(got.Chat) != 0 || len(got.Captions) != 0 {
		t.Fatalf("expired content remained: chat=%#v captions=%#v", got.Chat, got.Captions)
	}
}

func TestSceneKeepsDefaultChatUntilItIsDisplacedOrTheStreamStops(t *testing.T) {
	now := time.Date(2026, 8, 12, 3, 4, 5, 0, time.UTC)
	s := newTestScene(t, 854, 480, now)
	s.Reset(1, "stream-01", "Dev")
	apply(t, s, events.CustomOverlayEvent("stream-01", "overlay.discord_chat", map[string]any{
		"message_id": "message-01", "author_id": "user-01", "content": "keep me",
	}, now))

	if got := s.Snapshot(now.Add(24 * time.Hour)); len(got.Chat) != 1 || got.Chat[0].MessageID != "message-01" {
		t.Fatalf("default chat expired while the stream was still active: %#v", got.Chat)
	}
}

func TestSceneResetAndClearFenceStreamState(t *testing.T) {
	now := time.Date(2026, 8, 12, 3, 4, 5, 0, time.UTC)
	s := newTestScene(t, 854, 480, now)
	s.Reset(1, "old-stream", "Old")
	apply(t, s, events.CustomOverlayEvent("old-stream", "overlay.discord_chat", map[string]any{
		"message_id": "old-message", "author_id": "user", "content": "old",
	}, now))
	s.Reset(2, "new-stream", "New")
	if got := s.Snapshot(now); got.StreamID != "new-stream" || len(got.Chat) != 0 || len(got.Participants) != 0 {
		t.Fatalf("reset leaked predecessor state: %#v", got)
	}
	if err := s.Apply(0, events.CurrentTimeEvent("old-stream", now)); err == nil {
		t.Fatal("expected a stale stream event to be rejected")
	}
	if err := s.Apply(1, events.CustomOverlayEvent("new-stream", "overlay.discord_chat", map[string]any{
		"message_id": "delayed-old", "author_id": "old-user", "content": "must be fenced",
	}, now)); err == nil {
		t.Fatal("expected an in-flight predecessor generation to be rejected after same-process restart")
	}
	s.Clear("old-stream")
	if got := s.Snapshot(now); got.StreamID != "new-stream" {
		t.Fatalf("stale clear removed successor scene: %#v", got)
	}
	s.Clear("new-stream")
	if got := s.Snapshot(now); got.StreamID != "" || len(got.Chat) != 0 {
		t.Fatalf("clear retained state: %#v", got)
	}
}

func newTestScene(t *testing.T, width, height int, now time.Time) *Scene {
	t.Helper()
	return newTestSceneWithConfig(t, Config{Width: width, Height: height, FontFile: writeTestFont(t), Now: func() time.Time { return now }})
}

func newTestSceneWithConfig(t *testing.T, cfg Config) *Scene {
	t.Helper()
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func writeTestFont(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.ttf")
	if err := os.WriteFile(path, goregular.TTF, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func apply(t *testing.T, s *Scene, event events.OverlayEvent) {
	t.Helper()
	if err := s.Apply(0, event); err != nil {
		t.Fatal(err)
	}
}

func containsSpeakingGreen(img *image.RGBA) bool {
	for y := img.Bounds().Min.Y; y < img.Bounds().Max.Y; y++ {
		for x := img.Bounds().Min.X; x < img.Bounds().Max.X; x++ {
			r, g, b, a := img.At(x, y).RGBA()
			if a > 0 && g > 45000 && r < 25000 && b < 40000 {
				return true
			}
		}
	}
	return false
}
