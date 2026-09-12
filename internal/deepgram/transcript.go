package deepgram

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

type Transcript struct {
	Text               string
	SpeakerUserID      string
	UtteranceID        string
	Revision           int
	Final              bool
	StartedAt          time.Time
	UpdatedAt          time.Time
	EndedAt            time.Time
	Confidence         float64
	FinalizationReason string
	Source             string
	AudioReceivedAt    time.Time
	ReceivedAt         time.Time
}

type Handler func(context.Context, Transcript) error

type queuedTranscript struct {
	text         string
	final        bool
	speechFinal  bool
	utteranceEnd bool
	receivedAt   time.Time
	startedAt    time.Time
	confidence   float64
}

type resultMessage struct {
	Type        string  `json:"type"`
	IsFinal     bool    `json:"is_final"`
	SpeechFinal bool    `json:"speech_final"`
	Start       float64 `json:"start"`
	Duration    float64 `json:"duration"`
	Channel     struct {
		Alternatives []struct {
			Transcript string  `json:"transcript"`
			Confidence float64 `json:"confidence"`
		} `json:"alternatives"`
	} `json:"channel"`
}

func parseResult(payload []byte, interimResults bool) (queuedTranscript, bool) {
	result, ok := parseMessage(payload, interimResults)
	if !ok || result.utteranceEnd {
		return queuedTranscript{}, false
	}
	return result, true
}

func parseMessage(payload []byte, interimResults bool) (queuedTranscript, bool) {
	var message resultMessage
	var envelope struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return queuedTranscript{}, false
	}
	if envelope.Type == "UtteranceEnd" {
		return queuedTranscript{utteranceEnd: true, receivedAt: time.Now().UTC()}, true
	}
	if envelope.Type != "Results" || json.Unmarshal(payload, &message) != nil || len(message.Channel.Alternatives) == 0 {
		return queuedTranscript{}, false
	}
	transcript := strings.TrimSpace(message.Channel.Alternatives[0].Transcript)
	if transcript == "" || (!message.IsFinal && !interimResults) {
		return queuedTranscript{}, false
	}
	receivedAt := time.Now().UTC()
	startedAt := receivedAt
	if message.Duration > 0 {
		startedAt = receivedAt.Add(-time.Duration(message.Duration * float64(time.Second)))
	}
	return queuedTranscript{
		text:        transcript,
		final:       message.IsFinal,
		speechFinal: message.SpeechFinal,
		receivedAt:  receivedAt,
		startedAt:   startedAt,
		confidence:  message.Channel.Alternatives[0].Confidence,
	}, true
}

func (c *speakerConnection) publishLoop() {
	defer c.session.loopWG.Done()
	var ticker *time.Ticker
	var tick <-chan time.Time
	if c.session.config.LocalFinalize > 0 || c.session.config.SpeakerIdleClose > 0 {
		ticker = time.NewTicker(100 * time.Millisecond)
		tick = ticker.C
		defer ticker.Stop()
	}
	for {
		select {
		case <-c.ctx.Done():
			return
		case result := <-c.results:
			if transcript, ok := c.applyResult(result); ok {
				c.publishTranscript(transcript)
			}
		case now := <-tick:
			if transcript, ok := c.localFinalizeDue(now.UTC()); ok {
				c.publishTranscript(transcript)
			}
			if c.idleDue(now.UTC()) {
				c.session.discard(c)
				return
			}
		}
	}
}

func (c *speakerConnection) applyResult(result queuedTranscript) (Transcript, bool) {
	now := result.receivedAt
	if now.IsZero() {
		now = time.Now().UTC()
	}
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	c.lastResultAt = now
	if result.utteranceEnd {
		if c.utteranceText == "" {
			return Transcript{}, false
		}
		transcript := c.buildTranscriptLocked(now, true, "utterance_end", c.utteranceText, c.lastConfidence)
		c.resetUtteranceLocked()
		return transcript, true
	}
	if result.text == "" {
		return Transcript{}, false
	}
	if c.utteranceID == "" {
		c.utteranceSeq++
		c.utteranceID = fmt.Sprintf("%s-%d-%d", c.currentUserID(), c.ssrc, c.utteranceSeq)
		c.utteranceStarted = result.startedAt
		if c.utteranceStarted.IsZero() {
			c.utteranceStarted = now
		}
	}
	if result.startedAt.Before(c.utteranceStarted) && !result.startedAt.IsZero() {
		c.utteranceStarted = result.startedAt
	}
	c.lastConfidence = result.confidence
	changed := result.text != c.utteranceText
	c.utteranceText = result.text
	if result.final || result.speechFinal {
		reason := "is_final"
		if result.speechFinal {
			reason = "speech_final"
		}
		transcript := c.buildTranscriptLocked(now, true, reason, result.text, result.confidence)
		c.resetUtteranceLocked()
		return transcript, true
	}
	if !changed {
		return Transcript{}, false
	}
	c.utteranceRevision++
	return c.buildTranscriptLocked(now, false, "", result.text, result.confidence), true
}

func (c *speakerConnection) localFinalizeDue(now time.Time) (Transcript, bool) {
	if c.session.config.LocalFinalize <= 0 {
		return Transcript{}, false
	}
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	if c.utteranceText == "" || c.lastResultAt.IsZero() || now.Sub(c.lastResultAt) < c.session.config.LocalFinalize {
		return Transcript{}, false
	}
	if !c.lastAudioAt.IsZero() && now.Sub(c.lastAudioAt) < c.session.config.LocalFinalize {
		return Transcript{}, false
	}
	transcript := c.buildTranscriptLocked(now, true, "local_finalize", c.utteranceText, c.lastConfidence)
	c.resetUtteranceLocked()
	return transcript, true
}

func (c *speakerConnection) buildTranscriptLocked(now time.Time, final bool, reason, text string, confidence float64) Transcript {
	if final {
		c.utteranceRevision++
	}
	return Transcript{
		Text:          text,
		SpeakerUserID: c.currentUserID(),
		UtteranceID:   c.utteranceID,
		Revision:      c.utteranceRevision,
		Final:         final,
		StartedAt:     c.utteranceStarted,
		UpdatedAt:     now,
		EndedAt: func() time.Time {
			if final {
				return now
			}
			return time.Time{}
		}(),
		Confidence:         confidence,
		FinalizationReason: reason,
		Source:             "discord_voice",
		AudioReceivedAt:    c.lastAudioAt,
		ReceivedAt:         now,
	}
}

func (c *speakerConnection) resetUtteranceLocked() {
	c.utteranceID = ""
	c.utteranceText = ""
	c.utteranceRevision = 0
	c.utteranceStarted = time.Time{}
	c.lastConfidence = 0
}

func (c *speakerConnection) publishTranscript(transcript Transcript) {
	due := transcript.ReceivedAt.Add(c.session.config.Delay)
	if wait := time.Until(due); wait > 0 {
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case <-c.ctx.Done():
			return
		case <-timer.C:
		}
	}
	_ = c.session.handler(c.ctx, transcript)
}
