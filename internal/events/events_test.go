package events

import (
	"testing"
	"time"
)

func TestCurrentTimeEvent(t *testing.T) {
	ev := CurrentTimeEvent("s1", time.Date(2026, 5, 28, 0, 0, 0, 0, time.UTC))
	if ev.Type != "overlay.current_time" || ev.StreamID != "s1" {
		t.Fatalf("unexpected event: %#v", ev)
	}
	if ev.ID == "" || ev.Timestamp.IsZero() {
		t.Fatalf("event metadata was not set: %#v", ev)
	}
	if ev.Payload["jst"] != "2026-05-28T09:00:00+09:00" {
		t.Fatalf("unexpected JST payload: %#v", ev.Payload)
	}
}

func TestFinalCaptionEvent(t *testing.T) {
	ev := FinalCaptionEvent("s1", "確定字幕", "user-01", time.Date(2026, 5, 28, 0, 0, 0, 0, time.UTC))
	if ev.Type != "caption.final" || ev.Payload["text"] != "確定字幕" || ev.Payload["speaker_user_id"] != "user-01" {
		t.Fatalf("unexpected final caption event: %#v", ev)
	}
}

func TestParticipantListEvent(t *testing.T) {
	ev := ParticipantListEvent("s1", []Participant{{UserID: "u1", DisplayName: "alice", AvatarURL: "https://cdn.discordapp.com/avatars/u1/a.png", IsBot: true, Speaking: true}}, time.Date(2026, 5, 28, 0, 0, 0, 0, time.UTC))
	if ev.Type != "overlay.participants" {
		t.Fatalf("unexpected type: %s", ev.Type)
	}
	participants, ok := ev.Payload["participants"].([]map[string]any)
	if !ok || len(participants) != 1 || participants[0]["user_id"] != "u1" || participants[0]["avatar_url"] == "" || participants[0]["is_bot"] != true || participants[0]["speaking"] != true {
		t.Fatalf("unexpected participants payload: %#v", ev.Payload)
	}
}

func TestActiveSpeakerStateEventCarriesSpeakingEdge(t *testing.T) {
	ev := ActiveSpeakerStateEvent("s1", "u1", "alice", false, time.Date(2026, 5, 28, 0, 0, 0, 0, time.UTC))
	if ev.Type != "overlay.active_speaker" || ev.Payload["user_id"] != "u1" || ev.Payload["speaking"] != false {
		t.Fatalf("unexpected active speaker state payload: %#v", ev)
	}
}
