package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/example/autostream-worker/internal/events"
	"github.com/example/autostream-worker/internal/jobs"
	"github.com/example/autostream-worker/internal/scene"
	"golang.org/x/image/font/gofont/goregular"
)

type chatPayloadHTTPScene struct {
	*scene.Scene
	applyCalls int
}

func (s *chatPayloadHTTPScene) Apply(generation uint64, event events.OverlayEvent) error {
	s.applyCalls++
	return s.Scene.Apply(generation, event)
}

func newChatPayloadHandler(t *testing.T) (http.Handler, *jobs.Manager, *chatPayloadHTTPScene, *capturePublisher, time.Time) {
	t.Helper()
	fontPath := filepath.Join(t.TempDir(), "test.ttf")
	if err := os.WriteFile(fontPath, goregular.TTF, 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	realScene, err := scene.New(scene.Config{FontFile: fontPath, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(realScene.Close)
	renderer := &chatPayloadHTTPScene{Scene: realScene}
	publisher := &capturePublisher{}
	manager := jobs.NewManager(publisher, nil)
	manager.SetSceneRenderer(renderer)
	if err := manager.Start(t.Context(), jobs.StreamContext{StreamID: "stream-01"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := manager.Stop(context.Background(), "stream-01"); err != nil {
			t.Error(err)
		}
	})
	return NewServer("worker", manager, TokenVerifier{PlainToken: "chat-payload-test-token"}), manager, renderer, publisher, now
}

func canonicalHTTPChatPayload(messageID string) map[string]any {
	return map[string]any{"message_id": messageID, "author_id": "author-01", "content": "original chat"}
}

func postChatPayload(t *testing.T, handler http.Handler, streamID string, generation uint64, eventType string, payload map[string]any, authorized bool) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(map[string]any{"job_generation": generation, "type": eventType, "payload": payload})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/streams/"+streamID+"/events/overlay", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	if authorized {
		request.Header.Set("Authorization", "Bearer chat-payload-test-token")
	} else {
		request.Header.Set("Authorization", "Bearer invalid-test-token")
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

type httpChatPayloadState struct {
	status         jobs.Status
	metrics        map[string]float64
	history        string
	scene          scene.Snapshot
	publisherCalls int
	applyCalls     int
}

func snapshotHTTPChatPayloadState(t *testing.T, manager *jobs.Manager, renderer *chatPayloadHTTPScene, publisher *capturePublisher, at time.Time) httpChatPayloadState {
	t.Helper()
	history, err := manager.RecentEvents("stream-01")
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(history)
	if err != nil {
		t.Fatal(err)
	}
	return httpChatPayloadState{
		status: manager.Status(), metrics: manager.Metrics(), history: string(encoded),
		scene: renderer.Snapshot(at), publisherCalls: len(publisher.events), applyCalls: renderer.applyCalls,
	}
}

func assertHTTPChatPayloadStateUnchanged(t *testing.T, before, after httpChatPayloadState) {
	t.Helper()
	for _, check := range []struct {
		name          string
		before, after any
	}{
		{"Publisher calls", before.publisherCalls, after.publisherCalls},
		{"Scene.Apply calls", before.applyCalls, after.applyCalls},
		{"real Scene chat/conversation", before.scene, after.scene},
		{"history", before.history, after.history},
		{"status/counters", before.status, after.status},
		{"metrics", before.metrics, after.metrics},
	} {
		if !reflect.DeepEqual(check.before, check.after) {
			t.Errorf("rejected HTTP chat changed %s", check.name)
		}
	}
}

func TestDiscordChatHandlerRejectsLegacyFieldsBeforeSideEffects(t *testing.T) {
	handler, manager, renderer, publisher, now := newChatPayloadHandler(t)
	generation := manager.Status().JobGeneration
	if response := postChatPayload(t, handler, "stream-01", generation, "overlay.discord_chat", canonicalHTTPChatPayload("existing-message"), true); response.Code != http.StatusAccepted {
		t.Fatalf("canonical setup chat: status=%d", response.Code)
	}
	before := snapshotHTTPChatPayloadState(t, manager, renderer, publisher, now)
	if len(before.scene.Chat) != 1 || len(before.scene.Conversation) != 1 || before.status.EventCount != 1 {
		t.Fatal("fixture must contain rendered and delivered chat")
	}
	for _, form := range []string{"canonical", "legacy_only"} {
		for _, keys := range [][]string{{"user_id"}, {"text"}, {"user_id", "text"}} {
			for _, value := range []struct {
				name string
				data any
			}{
				{"null", nil}, {"empty", ""}, {"string", "legacy-value"},
				{"bool", true}, {"number", 42}, {"array", []any{"legacy-value"}},
				{"object", map[string]any{"nested": "legacy-value"}},
			} {
				t.Run(form+"/"+strings.Join(keys, "+")+"/"+value.name, func(t *testing.T) {
					payload := map[string]any{}
					if form == "canonical" {
						payload = canonicalHTTPChatPayload("incoming-message")
					}
					for _, key := range keys {
						payload[key] = value.data
					}
					response := postChatPayload(t, handler, "stream-01", generation, "overlay.discord_chat", payload, true)
					if response.Code != http.StatusBadRequest {
						t.Errorf("legacy chat: status=%d, want 400", response.Code)
					}
					var body map[string]any
					if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
						t.Fatal("invalid JSON error response")
					}
					want := map[string]any{"code": "validation_failed", "message": jobs.ErrLegacyDiscordChatFields.Error()}
					if !reflect.DeepEqual(body, want) {
						t.Error("expected stable validation_failed response without payload values")
					}
					assertHTTPChatPayloadStateUnchanged(t, before, snapshotHTTPChatPayloadState(t, manager, renderer, publisher, now))
				})
			}
		}
	}
}

func TestDiscordChatHandlerPreservesAuthStreamAndGenerationBoundaries(t *testing.T) {
	handler, manager, renderer, publisher, now := newChatPayloadHandler(t)
	oldGeneration := manager.Status().JobGeneration
	if err := manager.Stop(t.Context(), "stream-01"); err != nil {
		t.Fatal(err)
	}
	if err := manager.Start(t.Context(), jobs.StreamContext{StreamID: "stream-01"}); err != nil {
		t.Fatal(err)
	}
	generation := manager.Status().JobGeneration
	if response := postChatPayload(t, handler, "stream-01", generation, "overlay.discord_chat", canonicalHTTPChatPayload("existing-message"), true); response.Code != http.StatusAccepted {
		t.Fatalf("canonical setup chat: status=%d", response.Code)
	}
	before := snapshotHTTPChatPayloadState(t, manager, renderer, publisher, now)
	for _, tc := range []struct {
		name, streamID, code string
		generation           uint64
		authorized           bool
		status               int
	}{
		{"invalid_auth", "stream-01", "missing_or_invalid_service_token", generation, false, http.StatusUnauthorized},
		{"other_stream", "stream-02", "invalid_stream_state", generation, true, http.StatusConflict},
		{"stale_generation", "stream-01", "job_generation_mismatch", oldGeneration, true, http.StatusConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := canonicalHTTPChatPayload("incoming-message")
			payload["text"] = nil
			response := postChatPayload(t, handler, tc.streamID, tc.generation, "overlay.discord_chat", payload, tc.authorized)
			if response.Code != tc.status || responseCode(t, response) != tc.code {
				t.Errorf("authentication/stream/generation boundary changed: status=%d", response.Code)
			}
			assertHTTPChatPayloadStateUnchanged(t, before, snapshotHTTPChatPayloadState(t, manager, renderer, publisher, now))
		})
	}
}

func TestDiscordChatHandlerPreservesCanonicalAndNonChatPayloads(t *testing.T) {
	handler, manager, renderer, publisher, now := newChatPayloadHandler(t)
	payload := canonicalHTTPChatPayload("canonical-message")
	payload["content"] = "user_id and text are ordinary words"
	payload["text_channel_id"] = "channel-01"
	payload["metadata"] = map[string]any{"user_id": "nested-user", "text": "nested-text"}
	for index, tc := range []struct {
		eventType string
		payload   map[string]any
	}{
		{"overlay.discord_chat", payload},
		{"overlay.participants", map[string]any{"participants": []any{map[string]any{"user_id": "speaker-01"}}}},
		{"overlay.active_speaker", map[string]any{"user_id": "speaker-01", "speaking": true}},
		{"caption.telop", map[string]any{"text": "caption text", "speaker_user_id": "speaker-01"}},
	} {
		response := postChatPayload(t, handler, "stream-01", manager.Status().JobGeneration, tc.eventType, tc.payload, true)
		if response.Code != http.StatusAccepted {
			t.Fatalf("canonical %s: status=%d", tc.eventType, response.Code)
		}
		if len(publisher.events) != index+1 || renderer.applyCalls != index+1 || manager.Status().EventCount != index+1 {
			t.Fatal("canonical event was not applied, counted and published exactly once")
		}
		var event events.OverlayEvent
		if err := json.Unmarshal(response.Body.Bytes(), &event); err != nil {
			t.Fatal(err)
		}
		history, err := manager.RecentEvents("stream-01")
		if err != nil || len(history) != index+1 || history[index].ID != event.ID || !reflect.DeepEqual(history[index].Payload, tc.payload) {
			t.Fatal("canonical event history was not preserved")
		}
		if publisher.events[index].Type != tc.eventType || publisher.events[index].ID != event.ID || !reflect.DeepEqual(publisher.events[index].Payload, tc.payload) {
			t.Fatal("canonical payload or metadata was changed before publication")
		}
	}
	snapshot := renderer.Snapshot(now)
	if len(snapshot.Chat) != 1 || snapshot.Chat[0].Content != payload["content"] || len(snapshot.Conversation) != 2 {
		t.Fatal("canonical chat or conversation did not reach the real Scene")
	}
	if len(snapshot.Participants) != 1 || snapshot.Participants[0].UserID != "speaker-01" || !snapshot.Participants[0].Speaking || len(snapshot.Captions) != 1 || snapshot.Captions[0].Text != "caption text" || snapshot.Captions[0].SpeakerUserID != "speaker-01" {
		t.Fatal("non-chat user_id, text or speaker_user_id stopped updating the real Scene")
	}
}
