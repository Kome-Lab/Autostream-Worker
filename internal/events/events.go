package events

import (
	"crypto/rand"
	"encoding/hex"
	"time"
)

type OverlayEvent struct {
	ID        string         `json:"id"`
	StreamID  string         `json:"stream_id"`
	Type      string         `json:"type"`
	Payload   map[string]any `json:"payload"`
	Timestamp time.Time      `json:"timestamp"`
}

func CurrentTimeEvent(streamID string, now time.Time) OverlayEvent {
	return newEvent(streamID, "overlay.current_time", map[string]any{"jst": now.In(time.FixedZone("JST", 9*60*60)).Format(time.RFC3339)}, now)
}

func CaptionEvent(streamID, text, speakerUserID string, now time.Time) OverlayEvent {
	return newEvent(streamID, "caption.telop", map[string]any{"text": text, "speaker_user_id": speakerUserID}, now)
}

func ParticipantListEvent(streamID string, participants []Participant, now time.Time) OverlayEvent {
	payloadParticipants := make([]map[string]any, 0, len(participants))
	for _, participant := range participants {
		payloadParticipants = append(payloadParticipants, map[string]any{
			"user_id":      participant.UserID,
			"display_name": participant.DisplayName,
			"speaking":     participant.Speaking,
		})
	}
	return newEvent(streamID, "overlay.participants", map[string]any{"participants": payloadParticipants}, now)
}

func ActiveSpeakerEvent(streamID, userID, displayName string, now time.Time) OverlayEvent {
	return newEvent(streamID, "overlay.active_speaker", map[string]any{"user_id": userID, "display_name": displayName}, now)
}

func CustomOverlayEvent(streamID, eventType string, payload map[string]any, now time.Time) OverlayEvent {
	return newEvent(streamID, eventType, payload, now)
}

type Participant struct {
	UserID      string `json:"user_id"`
	DisplayName string `json:"display_name,omitempty"`
	Speaking    bool   `json:"speaking"`
}

func newEvent(streamID, eventType string, payload map[string]any, now time.Time) OverlayEvent {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if payload == nil {
		payload = map[string]any{}
	}
	return OverlayEvent{ID: randomID(), StreamID: streamID, Type: eventType, Payload: payload, Timestamp: now.UTC()}
}

func randomID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return hex.EncodeToString([]byte(time.Now().UTC().Format(time.RFC3339Nano)))
	}
	return hex.EncodeToString(b[:])
}
