package events

import (
	"crypto/rand"
	"encoding/hex"
	"strings"
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

func FinalCaptionEvent(streamID, text, speakerUserID string, now time.Time) OverlayEvent {
	return newEvent(streamID, "caption.final", map[string]any{"text": text, "speaker_user_id": speakerUserID}, now)
}

// CaptionTranscriptEvent is the versioned caption contract used by the live
// Deepgram path. The compact CaptionEvent/FinalCaptionEvent constructors stay
// available for older HTTP callers.
func CaptionTranscriptEvent(streamID, text, speakerUserID, utteranceID, source string, revision int, final bool, startedAt, updatedAt, endedAt time.Time, confidence float64, finalizationReason string, now time.Time) OverlayEvent {
	payload := map[string]any{
		"version":             2,
		"schema_version":      2,
		"utterance_id":        utteranceID,
		"revision":            revision,
		"source":              source,
		"speaker_user_id":     speakerUserID,
		"text":                text,
		"is_final":            final,
		"confidence":          confidence,
		"finalization_reason": finalizationReason,
	}
	if !startedAt.IsZero() {
		payload["started_at"] = startedAt.UTC().Format(time.RFC3339Nano)
	}
	if !updatedAt.IsZero() {
		payload["updated_at"] = updatedAt.UTC().Format(time.RFC3339Nano)
	}
	if !endedAt.IsZero() {
		payload["ended_at"] = endedAt.UTC().Format(time.RFC3339Nano)
	}
	if strings.TrimSpace(source) == "" || strings.EqualFold(strings.TrimSpace(source), "deepgram") {
		// The public event describes the application source, not the provider
		// implementation. Keep the legacy version field above while emitting the
		// v2 contract name used by downstream scene/archive consumers.
		payload["source"] = "discord_voice"
	}
	if strings.TrimSpace(finalizationReason) == "" {
		delete(payload, "finalization_reason")
	}
	return newEvent(streamID, mapCaptionEventType(final), payload, now)
}

func mapCaptionEventType(final bool) string {
	if final {
		return "caption.final"
	}
	return "caption.telop"
}

func ParticipantListEvent(streamID string, participants []Participant, now time.Time) OverlayEvent {
	payloadParticipants := make([]map[string]any, 0, len(participants))
	for _, participant := range participants {
		payloadParticipants = append(payloadParticipants, map[string]any{
			"user_id":      participant.UserID,
			"display_name": participant.DisplayName,
			"avatar_url":   participant.AvatarURL,
			"is_bot":       participant.IsBot,
			"speaking":     participant.Speaking,
		})
	}
	return newEvent(streamID, "overlay.participants", map[string]any{"participants": payloadParticipants}, now)
}

func ActiveSpeakerEvent(streamID, userID, displayName string, now time.Time) OverlayEvent {
	return ActiveSpeakerStateEvent(streamID, userID, displayName, true, now)
}

func ActiveSpeakerStateEvent(streamID, userID, displayName string, speaking bool, now time.Time) OverlayEvent {
	return newEvent(streamID, "overlay.active_speaker", map[string]any{"user_id": userID, "display_name": displayName, "speaking": speaking}, now)
}

func CustomOverlayEvent(streamID, eventType string, payload map[string]any, now time.Time) OverlayEvent {
	return newEvent(streamID, eventType, payload, now)
}

type Participant struct {
	UserID      string `json:"user_id"`
	DisplayName string `json:"display_name,omitempty"`
	AvatarURL   string `json:"avatar_url,omitempty"`
	IsBot       bool   `json:"is_bot,omitempty"`
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
