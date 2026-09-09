package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/example/autostream-worker/internal/encoder"
	"github.com/example/autostream-worker/internal/events"
	"github.com/example/autostream-worker/internal/scene"
	"golang.org/x/image/font/gofont/goregular"
)

type chatPayloadScene struct {
	*scene.Scene
	applyCalls int
}

func (s *chatPayloadScene) Apply(generation uint64, event events.OverlayEvent) error {
	s.applyCalls++
	return s.Scene.Apply(generation, event)
}

func newChatPayloadManager(t *testing.T) (*Manager, *chatPayloadScene, *fakePublisher, *fakeReporter) {
	t.Helper()
	fontPath := filepath.Join(t.TempDir(), "test.ttf")
	if err := os.WriteFile(fontPath, goregular.TTF, 0o600); err != nil {
		t.Fatal(err)
	}
	realScene, err := scene.New(scene.Config{FontFile: fontPath, Now: testTime})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(realScene.Close)
	renderer := &chatPayloadScene{Scene: realScene}
	publisher := &fakePublisher{}
	reporter := &fakeReporter{}
	manager := NewManager(publisher, reporter)
	manager.SetSceneRenderer(renderer)
	if err := manager.Start(t.Context(), StreamContext{StreamID: "stream-01"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := manager.Stop(context.Background(), "stream-01"); err != nil {
			t.Error(err)
		}
	})
	// Join the retry loop before installing fixed pending state. These tests
	// exercise synchronous publication without retries changing the pre-state.
	manager.stopEventDelivery()
	return manager, renderer, publisher, reporter
}

func canonicalChatPayload(messageID string) map[string]any {
	return map[string]any{"message_id": messageID, "author_id": "author-01", "content": "original chat"}
}

func seedChatPayloadState(t *testing.T, manager *Manager) {
	t.Helper()
	if _, err := manager.CustomOverlay(t.Context(), "stream-01", "overlay.discord_chat", canonicalChatPayload("delivered-message"), testTime()); err != nil {
		t.Fatal(err)
	}
	pending := encoder.Event{
		ID: "pending-event", StreamID: "stream-01", Type: "overlay.discord_chat",
		Payload: canonicalChatPayload("pending-message"), Timestamp: testTime(),
		Generation: manager.Status().JobGeneration, Attempt: 1,
	}
	key := workerEventKey(pending)
	manager.mu.Lock()
	manager.pendingEvents[key] = pendingWorkerEvent{
		event: pending, key: key, attempts: 1, queuedAt: testTime(), nextTryAt: testTime().Add(time.Minute),
	}
	manager.latestEventByKey[key] = pending.ID
	manager.mu.Unlock()
}

type chatPayloadState struct {
	status         Status
	metrics        map[string]float64
	history        string
	scene          scene.Snapshot
	pending        map[string]pendingWorkerEvent
	latest         map[string]string
	publisherCalls int
	applyCalls     int
	reports        int
}

func snapshotChatPayloadState(t *testing.T, manager *Manager, renderer *chatPayloadScene, publisher *fakePublisher, reporter *fakeReporter) chatPayloadState {
	t.Helper()
	history, err := manager.RecentEvents("stream-01")
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(history)
	if err != nil {
		t.Fatal(err)
	}
	state := chatPayloadState{
		status: manager.Status(), metrics: manager.Metrics(), history: string(encoded),
		scene: renderer.Snapshot(testTime()), publisherCalls: len(publisher.snapshot()),
		applyCalls: renderer.applyCalls, reports: len(reporter.events),
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	state.pending = maps.Clone(manager.pendingEvents)
	for key, pending := range state.pending {
		pending.event.Payload = maps.Clone(pending.event.Payload)
		state.pending[key] = pending
	}
	state.latest = maps.Clone(manager.latestEventByKey)
	return state
}

func assertChatPayloadStateUnchanged(t *testing.T, before, after chatPayloadState) {
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
		{"pending events", before.pending, after.pending},
		{"latest events", before.latest, after.latest},
		{"reports", before.reports, after.reports},
	} {
		if !reflect.DeepEqual(check.before, check.after) {
			t.Errorf("rejected chat changed %s", check.name)
		}
	}
}

func TestManagerDiscordChatRejectsLegacyFieldsBeforeSideEffects(t *testing.T) {
	for _, entry := range []string{"generation", "direct"} {
		t.Run(entry, func(t *testing.T) {
			manager, renderer, publisher, reporter := newChatPayloadManager(t)
			seedChatPayloadState(t, manager)
			before := snapshotChatPayloadState(t, manager, renderer, publisher, reporter)
			if len(before.pending) != 1 || len(before.scene.Chat) != 1 || len(before.scene.Conversation) != 1 || before.status.EventCount != 1 {
				t.Fatal("fixture must contain pending, rendered and delivered chat")
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
								payload = canonicalChatPayload("pending-message")
							}
							for _, key := range keys {
								payload[key] = value.data
							}
							var event events.OverlayEvent
							var err error
							if entry == "direct" {
								event, err = manager.CustomOverlay(t.Context(), "stream-01", "overlay.discord_chat", payload, testTime())
							} else {
								event, err = manager.CustomOverlayForGeneration(t.Context(), "stream-01", before.status.JobGeneration, "overlay.discord_chat", payload, testTime())
							}
							if !errors.Is(err, ErrLegacyDiscordChatFields) {
								t.Error("expected stable legacy-chat validation error")
							}
							if err != nil && err.Error() != ErrLegacyDiscordChatFields.Error() {
								t.Error("validation error must not contain payload values")
							}
							if !reflect.DeepEqual(event, events.OverlayEvent{}) {
								t.Error("rejected chat returned an event")
							}
							assertChatPayloadStateUnchanged(t, before, snapshotChatPayloadState(t, manager, renderer, publisher, reporter))
						})
					}
				}
			}
		})
	}
}

func TestManagerDiscordChatPreservesStreamAndGenerationBoundaries(t *testing.T) {
	manager, renderer, publisher, reporter := newChatPayloadManager(t)
	oldGeneration := manager.Status().JobGeneration
	if err := manager.Stop(t.Context(), "stream-01"); err != nil {
		t.Fatal(err)
	}
	if err := manager.Start(t.Context(), StreamContext{StreamID: "stream-01"}); err != nil {
		t.Fatal(err)
	}
	manager.stopEventDelivery()
	seedChatPayloadState(t, manager)
	before := snapshotChatPayloadState(t, manager, renderer, publisher, reporter)
	for _, tc := range []struct {
		name, streamID, message string
		generation              uint64
	}{
		{"other_stream", "stream-02", "stream_id does not match current job", before.status.JobGeneration},
		{"stale_generation", "stream-01", ErrJobGenerationMismatch.Error(), oldGeneration},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := canonicalChatPayload("pending-message")
			payload["user_id"] = nil
			_, err := manager.CustomOverlayForGeneration(t.Context(), tc.streamID, tc.generation, "overlay.discord_chat", payload, testTime())
			if err == nil || err.Error() != tc.message || errors.Is(err, ErrLegacyDiscordChatFields) {
				t.Error("stream/generation boundary must precede payload validation")
			}
			assertChatPayloadStateUnchanged(t, before, snapshotChatPayloadState(t, manager, renderer, publisher, reporter))
		})
	}
}

func TestManagerDiscordChatPreservesCanonicalAndNonChatPayloads(t *testing.T) {
	manager, renderer, publisher, _ := newChatPayloadManager(t)
	payload := canonicalChatPayload("canonical-message")
	payload["content"] = "user_id and text are ordinary words"
	payload["text_channel_id"] = "channel-01"
	payload["metadata"] = map[string]any{"user_id": "nested-user", "text": "nested-text"}
	event, err := manager.CustomOverlayForGeneration(t.Context(), "stream-01", manager.Status().JobGeneration, "overlay.discord_chat", payload, testTime())
	if err != nil {
		t.Fatal(err)
	}
	snapshot := renderer.Snapshot(testTime())
	if len(snapshot.Chat) != 1 || snapshot.Chat[0].Content != payload["content"] || len(snapshot.Conversation) != 1 || snapshot.Conversation[0].Text != payload["content"] {
		t.Fatal("canonical chat did not reach the real Scene and conversation")
	}
	history, err := manager.RecentEvents("stream-01")
	if err != nil || len(history) != 1 || history[0].ID != event.ID || !reflect.DeepEqual(history[0].Payload, payload) {
		t.Fatal("canonical chat history was not preserved")
	}
	forwarded := publisher.snapshot()
	if len(forwarded) != 1 || forwarded[0].ID != event.ID || !reflect.DeepEqual(forwarded[0].Payload, payload) || renderer.applyCalls != 1 || manager.Status().EventCount != 1 {
		t.Fatal("canonical chat was not applied, counted and published exactly once with metadata intact")
	}
	if _, err := manager.Participants(t.Context(), "stream-01", []events.Participant{{UserID: "speaker-01"}}, testTime()); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.ActiveSpeakerState(t.Context(), "stream-01", "speaker-01", "Speaker", true, testTime()); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Caption(t.Context(), "stream-01", "caption text", "speaker-01", testTime()); err != nil {
		t.Fatal(err)
	}
	snapshot = renderer.Snapshot(testTime())
	if len(snapshot.Participants) != 1 || snapshot.Participants[0].UserID != "speaker-01" || !snapshot.Participants[0].Speaking || len(snapshot.Captions) != 1 || snapshot.Captions[0].Text != "caption text" || snapshot.Captions[0].SpeakerUserID != "speaker-01" {
		t.Fatal("non-chat user_id, text or speaker_user_id stopped updating the real Scene")
	}
	if len(publisher.snapshot()) != 4 || renderer.applyCalls != 4 || manager.Status().EventCount != 4 {
		t.Fatal("non-chat events were not applied, counted and published exactly once")
	}
}
