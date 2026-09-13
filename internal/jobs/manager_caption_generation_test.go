package jobs

import (
	"context"
	"errors"
	"github.com/example/autostream-worker/internal/control"
	"github.com/example/autostream-worker/internal/deepgram"
	"github.com/example/autostream-worker/internal/observability"
	"testing"
	"time"
)

func TestManagerCancelsQueuedCaptionAudioAcrossStopAndRearm(t *testing.T) {
	oldSession := &controlledCaptionSession{started: make(chan struct{}), block: true}
	newSession := &controlledCaptionSession{started: make(chan struct{})}
	sessions := []*controlledCaptionSession{oldSession, newSession}
	factoryCalls := 0
	manager := NewManager(&fakePublisher{}, observability.Client{})
	manager.SetCaptionRuntime(RuntimeSecretResolverFunc(func(_ context.Context, _, secretName string) (control.RuntimeSecret, error) {
		return control.RuntimeSecret{SecretName: secretName, Value: "dg-runtime-key", ExpiresInSec: 300}, nil
	}), CaptionSessionFactoryFunc(func(deepgram.Config, []byte, deepgram.Handler) (CaptionSession, error) {
		session := sessions[factoryCalls]
		factoryCalls++
		return session, nil
	}))
	manager.ApplyRuntimeConfig(captionRuntimeConfig(control.RuntimeProfile{ID: "caption-01", Kind: "caption", Config: captionProfileConfig("ja")}))
	if err := manager.Start(t.Context(), StreamContext{StreamID: "stream-01", CaptionProfileID: "caption-01"}); err != nil {
		t.Fatal(err)
	}
	oldGeneration := manager.Status().JobGeneration
	oldFirst := []deepgram.AudioPacket{{SSRC: 42, UserID: "speaker-42", JobGeneration: oldGeneration, ConnectionGeneration: 1, Sequence: 1, Opus: []byte{1}}}
	oldQueued := []deepgram.AudioPacket{{SSRC: 42, UserID: "speaker-42", JobGeneration: oldGeneration, ConnectionGeneration: 1, Sequence: 2, Opus: []byte{2}}}
	if err := manager.EnqueueCaptionAudioForGeneration(t.Context(), "stream-01", oldGeneration, oldFirst); err != nil {
		t.Fatal(err)
	}
	select {
	case <-oldSession.started:
	case <-time.After(time.Second):
		t.Fatal("old generation did not enter provider ingest")
	}
	if err := manager.EnqueueCaptionAudioForGeneration(t.Context(), "stream-01", oldGeneration, oldQueued); err != nil {
		t.Fatal(err)
	}
	stopCtx, stopCancel := context.WithTimeout(context.Background(), time.Second)
	if err := manager.Stop(stopCtx, "stream-01"); err != nil {
		stopCancel()
		t.Fatal(err)
	}
	stopCancel()
	if err := manager.Start(t.Context(), StreamContext{StreamID: "stream-01", CaptionProfileID: "caption-01"}); err != nil {
		t.Fatal(err)
	}
	newGeneration := manager.Status().JobGeneration
	if newGeneration == oldGeneration {
		t.Fatalf("rearm did not advance generation: old=%d new=%d", oldGeneration, newGeneration)
	}
	if err := manager.EnqueueCaptionAudioForGeneration(t.Context(), "stream-01", oldGeneration, oldQueued); !errors.Is(err, ErrJobGenerationMismatch) {
		t.Fatalf("old generation was not rejected after rearm: %v", err)
	}
	newPacket := []deepgram.AudioPacket{{SSRC: 84, UserID: "speaker-84", JobGeneration: newGeneration, ConnectionGeneration: 1, Sequence: 3, Opus: []byte{3}}}
	if err := manager.EnqueueCaptionAudioForGeneration(t.Context(), "stream-01", newGeneration, newPacket); err != nil {
		t.Fatal(err)
	}
	select {
	case <-newSession.started:
	case <-time.After(time.Second):
		t.Fatal("new generation caption audio was not processed")
	}
	oldPackets, oldClosed := oldSession.snapshot()
	newPackets, _ := newSession.snapshot()
	if len(oldPackets) != 1 || oldPackets[0].Sequence != 1 || oldClosed != 1 {
		t.Fatalf("queued old generation crossed the stop boundary: packets=%#v closed=%d", oldPackets, oldClosed)
	}
	if len(newPackets) != 1 || newPackets[0].Sequence != 3 {
		t.Fatalf("new generation received unexpected packets: %#v", newPackets)
	}
	if err := manager.Stop(t.Context(), "stream-01"); err != nil {
		t.Fatal(err)
	}
}

func TestManagerSupersedesOlderConnectionGenerationQueue(t *testing.T) {
	session := &controlledCaptionSession{started: make(chan struct{}), block: true}
	manager := NewManager(&fakePublisher{}, observability.Client{})
	manager.SetCaptionRuntime(RuntimeSecretResolverFunc(func(_ context.Context, _, secretName string) (control.RuntimeSecret, error) {
		return control.RuntimeSecret{SecretName: secretName, Value: "dg-runtime-key", ExpiresInSec: 300}, nil
	}), CaptionSessionFactoryFunc(func(deepgram.Config, []byte, deepgram.Handler) (CaptionSession, error) {
		return session, nil
	}))
	manager.ApplyRuntimeConfig(captionRuntimeConfig(control.RuntimeProfile{ID: "caption-01", Kind: "caption", Config: captionProfileConfig("ja")}))
	if err := manager.Start(t.Context(), StreamContext{StreamID: "stream-01", CaptionProfileID: "caption-01"}); err != nil {
		t.Fatal(err)
	}
	generation := manager.Status().JobGeneration
	packet := func(connectionGeneration uint64, sequence uint16) []deepgram.AudioPacket {
		return []deepgram.AudioPacket{{
			SSRC:                 42,
			UserID:               "speaker-42",
			JobGeneration:        generation,
			ConnectionGeneration: connectionGeneration,
			Sequence:             sequence,
			Opus:                 []byte{1},
		}}
	}
	if err := manager.EnqueueCaptionAudioForGeneration(t.Context(), "stream-01", generation, packet(1, 1)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-session.started:
	case <-time.After(time.Second):
		t.Fatal("old connection generation did not enter provider ingest")
	}
	if err := manager.EnqueueCaptionAudioForGeneration(t.Context(), "stream-01", generation, packet(1, 2)); err != nil {
		t.Fatal(err)
	}
	if err := manager.EnqueueCaptionAudioForGeneration(t.Context(), "stream-01", generation, packet(2, 3)); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		packets, _ := session.snapshot()
		if len(packets) >= 2 {
			if packets[0].Sequence != 1 || packets[1].Sequence != 3 {
				t.Fatalf("superseded queue reached provider: %#v", packets)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("new connection generation did not reach provider: %#v", packets)
		}
		time.Sleep(time.Millisecond)
	}
	if err := manager.EnqueueCaptionAudioForGeneration(t.Context(), "stream-01", generation, packet(1, 4)); !errors.Is(err, ErrCaptionAudioConnectionGenerationStale) {
		t.Fatalf("older connection generation was not rejected: %v", err)
	}
	status := manager.Status()
	if status.CaptionAudioConnectionGeneration != 2 || status.CaptionAudioSupersededDrops != 1 {
		t.Fatalf("unexpected connection generation status: %#v", status)
	}
	stopCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := manager.Stop(stopCtx, "stream-01"); err != nil {
		t.Fatal(err)
	}
}

func TestClassifyCaptionAudioErrorUsesSafeStableClasses(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{name: "canceled", err: context.Canceled, want: "context_canceled"},
		{name: "deadline", err: context.DeadlineExceeded, want: "context_deadline"},
		{name: "empty audio", err: deepgram.ErrEmptyAudio, want: "invalid_audio"},
		{name: "closed", err: deepgram.ErrClosed, want: "deepgram_session_closed"},
		{name: "stale connection generation", err: deepgram.ErrStaleConnectionGeneration, want: "connection_generation_stale"},
		{name: "unavailable", err: deepgram.ErrUnavailable, want: "deepgram_unavailable"},
		{name: "unknown", err: errors.New("secret endpoint must not be logged"), want: "caption_audio_ingest_failed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classifyCaptionAudioError(tt.err); got != tt.want {
				t.Fatalf("classifyCaptionAudioError() = %q, want %q", got, tt.want)
			}
		})
	}
}
