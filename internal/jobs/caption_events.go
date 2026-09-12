package jobs

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/example/autostream-worker/internal/deepgram"
	"github.com/example/autostream-worker/internal/events"
)

func (m *Manager) captionTranscriptHandler(streamID string, jobGeneration, captionSessionGeneration uint64) deepgram.Handler {
	return func(resultCtx context.Context, transcript deepgram.Transcript) error {
		m.report(resultCtx, streamID, "worker.caption.transcript_received", captionTranscriptStatus(transcript), captionTranscriptAttributes(transcript))
		_, publishErr := m.CaptionTranscriptDetailedForSessionGeneration(resultCtx, streamID, jobGeneration, captionSessionGeneration, transcript)
		if publishErr != nil {
			attributes := captionTranscriptAttributes(transcript)
			attributes["error_class"] = classifyCaptionTranscriptError(publishErr)
			m.report(resultCtx, streamID, "worker.caption.transcript_publish_failed", "failed", attributes)
		}
		return publishErr
	}
}

func captionTranscriptStatus(transcript deepgram.Transcript) string {
	if transcript.Final {
		return "final"
	}
	return "interim"
}

func captionTranscriptAttributes(transcript deepgram.Transcript) map[string]any {
	return map[string]any{
		"final":               transcript.Final,
		"text_length":         len([]rune(strings.TrimSpace(transcript.Text))),
		"has_speaker_user_id": strings.TrimSpace(transcript.SpeakerUserID) != "",
		"has_utterance_id":    strings.TrimSpace(transcript.UtteranceID) != "",
		"revision":            transcript.Revision,
	}
}

func classifyCaptionTranscriptError(err error) string {
	switch {
	case errors.Is(err, ErrCaptionSessionGenerationMismatch):
		return "caption_session_generation_mismatch"
	case errors.Is(err, ErrJobGenerationMismatch):
		return "job_generation_mismatch"
	case errors.Is(err, ErrStreamStopping):
		return "stream_stopping"
	case errors.Is(err, ErrNoActiveStreamJob):
		return "no_active_stream"
	case errors.Is(err, ErrStreamIDDoesNotMatchJob):
		return "stream_id_mismatch"
	default:
		return "event_publish_failed"
	}
}

func (m *Manager) Caption(ctx context.Context, streamID, text, speakerUserID string, now time.Time) (events.OverlayEvent, error) {
	return m.CaptionForGeneration(ctx, streamID, 0, text, speakerUserID, now)
}

func (m *Manager) CaptionForGeneration(ctx context.Context, streamID string, generation uint64, text, speakerUserID string, now time.Time) (events.OverlayEvent, error) {
	if strings.TrimSpace(text) == "" {
		return events.OverlayEvent{}, errors.New("caption text is required")
	}
	return m.publishWithGeneration(ctx, events.CaptionEvent(streamID, text, speakerUserID, now), generation)
}

func (m *Manager) CaptionTranscript(ctx context.Context, streamID, text, speakerUserID string, final bool, now time.Time) (events.OverlayEvent, error) {
	if strings.TrimSpace(text) == "" {
		return events.OverlayEvent{}, errors.New("caption text is required")
	}
	if final {
		return m.publish(ctx, events.FinalCaptionEvent(streamID, text, speakerUserID, now))
	}
	return m.publish(ctx, events.CaptionEvent(streamID, text, speakerUserID, now))
}

func (m *Manager) CaptionTranscriptDetailed(ctx context.Context, streamID string, transcript deepgram.Transcript) (events.OverlayEvent, error) {
	return m.CaptionTranscriptDetailedForGeneration(ctx, streamID, 0, transcript)
}

// CaptionTranscriptDetailedForGeneration keeps a delayed result from a closed
// Deepgram session out of a later rearm that happens to reuse the same stream
// ID.
func (m *Manager) CaptionTranscriptDetailedForGeneration(ctx context.Context, streamID string, generation uint64, transcript deepgram.Transcript) (events.OverlayEvent, error) {
	return m.captionTranscriptDetailedForGenerations(ctx, streamID, generation, 0, transcript)
}

// CaptionTranscriptDetailedForSessionGeneration additionally fences delayed
// results from a replaced Deepgram session within the same stream job.
func (m *Manager) CaptionTranscriptDetailedForSessionGeneration(ctx context.Context, streamID string, jobGeneration, captionSessionGeneration uint64, transcript deepgram.Transcript) (events.OverlayEvent, error) {
	return m.captionTranscriptDetailedForGenerations(ctx, streamID, jobGeneration, captionSessionGeneration, transcript)
}

func (m *Manager) captionTranscriptDetailedForGenerations(ctx context.Context, streamID string, jobGeneration, captionSessionGeneration uint64, transcript deepgram.Transcript) (events.OverlayEvent, error) {
	if strings.TrimSpace(transcript.Text) == "" {
		return events.OverlayEvent{}, errors.New("caption text is required")
	}
	now := transcript.UpdatedAt
	if now.IsZero() {
		now = transcript.ReceivedAt
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	return m.publishWithGenerations(ctx, events.CaptionTranscriptEvent(
		streamID,
		transcript.Text,
		transcript.SpeakerUserID,
		transcript.UtteranceID,
		transcript.Source,
		transcript.Revision,
		transcript.Final,
		transcript.StartedAt,
		now,
		transcript.EndedAt,
		transcript.Confidence,
		transcript.FinalizationReason,
		now,
	), jobGeneration, captionSessionGeneration)
}
