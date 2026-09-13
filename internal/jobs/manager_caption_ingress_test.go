package jobs

import (
	"context"
	"errors"
	"github.com/example/autostream-worker/internal/control"
	"github.com/example/autostream-worker/internal/deepgram"
	"github.com/example/autostream-worker/internal/observability"
	"net/http"
	"testing"
	"time"
)

func TestManagerProcessesNextCaptionPacketAfterSendFailure(t *testing.T) {
	reporter := &fakeReporter{}
	manager := NewManager(&fakePublisher{}, reporter)
	session := &fakeCaptionSession{ingestErrors: []error{errors.New("first send failed")}}
	manager.SetCaptionRuntime(RuntimeSecretResolverFunc(func(_ context.Context, _, secretName string) (control.RuntimeSecret, error) {
		return control.RuntimeSecret{SecretName: secretName, Value: "dg-runtime-key", ExpiresInSec: 300}, nil
	}), CaptionSessionFactoryFunc(func(deepgram.Config, []byte, deepgram.Handler) (CaptionSession, error) {
		return session, nil
	}))
	manager.ApplyRuntimeConfig(captionRuntimeConfig(control.RuntimeProfile{ID: "caption-01", Kind: "caption", Config: captionProfileConfig("ja")}))
	if err := manager.Start(t.Context(), StreamContext{StreamID: "stream-01", CaptionProfileID: "caption-01"}); err != nil {
		t.Fatal(err)
	}

	err := manager.IngestCaptionAudio(t.Context(), "stream-01", []deepgram.AudioPacket{
		{SSRC: 42, Sequence: 1, Opus: []byte{1}},
		{SSRC: 42, Sequence: 2, Opus: []byte{2}},
	})
	if !errors.Is(err, ErrCaptionAudioUnavailable) {
		t.Fatalf("unexpected batch error: %v", err)
	}
	if session.ingestCalls != 2 || len(session.packets) != 1 || session.packets[0].Sequence != 2 {
		t.Fatalf("next packet was not processed after send failure: calls=%d packets=%#v", session.ingestCalls, session.packets)
	}
	for i, name := range reporter.events {
		if name != "worker.caption.audio_failed" || i >= len(reporter.attrs) {
			continue
		}
		attrs := reporter.attrs[i]
		if attrs["error_class"] != "caption_audio_ingest_failed" {
			t.Fatalf("unexpected caption audio error class: %#v", attrs)
		}
		if _, leaked := attrs["error"]; leaked {
			t.Fatalf("raw caption audio error leaked: %#v", attrs)
		}
		return
	}
	t.Fatalf("caption audio failure diagnostic was not reported: events=%#v attrs=%#v", reporter.events, reporter.attrs)
}

func TestManagerCaptionIngressQueueIsBounded(t *testing.T) {
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
	packet := func(sequence uint16) []deepgram.AudioPacket {
		return []deepgram.AudioPacket{{SSRC: 42, UserID: "speaker-42", JobGeneration: generation, ConnectionGeneration: 1, Sequence: sequence, Opus: []byte{1}}}
	}
	if err := manager.EnqueueCaptionAudioForGeneration(t.Context(), "stream-01", generation, packet(1)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-session.started:
	case <-time.After(time.Second):
		t.Fatal("caption provider ingest did not block")
	}
	for i := 0; i < captionAudioQueueBatches; i++ {
		if err := manager.EnqueueCaptionAudioForGeneration(t.Context(), "stream-01", generation, packet(uint16(i+2))); err != nil {
			t.Fatalf("queue rejected batch %d before the configured bound: %v", i+1, err)
		}
	}
	if err := manager.EnqueueCaptionAudioForGeneration(t.Context(), "stream-01", generation, packet(500)); !errors.Is(err, ErrCaptionAudioQueueFull) {
		t.Fatalf("queue did not reject overflow: %v", err)
	}
	status := manager.Status()
	if status.CaptionAudioQueueBatches != captionAudioQueueBatches || status.CaptionAudioQueuePackets != captionAudioQueueBatches || status.CaptionAudioQueueDrops != 1 {
		t.Fatalf("unexpected bounded caption queue status: %#v", status)
	}
	stopCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := manager.Stop(stopCtx, "stream-01"); err != nil {
		t.Fatal(err)
	}
}

func TestManagerCaptionIngressRequiresGenerationAndRetriesOnlyFailedPackets(t *testing.T) {
	session := &retryCaptionSession{failuresRemaining: 1, called: make(chan struct{}, 8)}
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
	packet := []deepgram.AudioPacket{{SSRC: 42, UserID: "speaker-42", ConnectionGeneration: 1, Sequence: 1, Opus: []byte{1}}}
	if err := manager.EnqueueCaptionAudioForGeneration(t.Context(), "stream-01", 0, packet); !errors.Is(err, ErrCaptionAudioGenerationRequired) {
		t.Fatalf("zero generation was not rejected: %v", err)
	}
	generation := manager.Status().JobGeneration
	packet[0].JobGeneration = generation
	if err := manager.EnqueueCaptionAudioForGeneration(t.Context(), "stream-01", generation, packet); err != nil {
		t.Fatal(err)
	}
	waitForCaptionSessionCalls(t, session, 2)
	calls, packets := session.snapshot()
	if calls != 2 || len(packets) != 1 || packets[0].Sequence != 1 || packets[0].JobGeneration != generation {
		t.Fatalf("failed packet retry did not converge exactly once: calls=%d packets=%#v", calls, packets)
	}
	status := manager.Status()
	if status.CaptionAudioRetries != 1 || status.CaptionAudioProviderDrops != 0 {
		t.Fatalf("unexpected caption retry status: %#v", status)
	}
	if err := manager.Stop(t.Context(), "stream-01"); err != nil {
		t.Fatal(err)
	}
}

func TestManagerCaptionIngressCountsFinalProviderDrop(t *testing.T) {
	session := &retryCaptionSession{failuresRemaining: 10, called: make(chan struct{}, 8)}
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
	if err := manager.EnqueueCaptionAudioForGeneration(t.Context(), "stream-01", generation, []deepgram.AudioPacket{{SSRC: 42, UserID: "speaker-42", JobGeneration: generation, ConnectionGeneration: 1, Sequence: 1, Opus: []byte{1}}}); err != nil {
		t.Fatal(err)
	}
	waitForCaptionSessionCalls(t, session, captionAudioRetryMax)
	deadline := time.Now().Add(time.Second)
	for manager.Status().CaptionAudioProviderDrops == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	status := manager.Status()
	if status.CaptionAudioRetries != captionAudioRetryMax-1 || status.CaptionAudioProviderDrops != 1 {
		t.Fatalf("unexpected final caption drop status: %#v", status)
	}
	if err := manager.Stop(t.Context(), "stream-01"); err != nil {
		t.Fatal(err)
	}
}

func TestManagerCaptionIngressStopsRetryingTerminalProviderRejection(t *testing.T) {
	reporter := &channelReporter{reports: make(chan capturedReport, 16)}
	session := &retryCaptionSession{
		failuresRemaining: 100,
		failureErr: classifiedCaptionProviderError{
			errorClass: "provider_auth_rejected",
			httpStatus: http.StatusUnauthorized,
			retryable:  false,
		},
		called: make(chan struct{}, 16),
	}
	manager := NewManager(&fakePublisher{}, reporter)
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
	packets := make([]deepgram.AudioPacket, 5)
	for i := range packets {
		packets[i] = deepgram.AudioPacket{SSRC: 42, UserID: "speaker-42", JobGeneration: generation, ConnectionGeneration: 1, Sequence: uint16(i + 1), Opus: []byte{1}}
	}
	if err := manager.EnqueueCaptionAudioForGeneration(t.Context(), "stream-01", generation, packets); err != nil {
		t.Fatal(err)
	}

	var drop capturedReport
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	for drop.name == "" {
		select {
		case report := <-reporter.reports:
			if report.name == "worker.caption.audio_dropped" {
				drop = report
			}
		case <-deadline.C:
			t.Fatal("terminal provider drop was not reported")
		}
	}
	calls, _ := session.snapshot()
	status := manager.Status()
	if calls != 1 || status.CaptionAudioRetries != 0 || status.CaptionAudioProviderDrops != len(packets) {
		t.Fatalf("terminal failure was amplified: calls=%d status=%#v", calls, status)
	}
	if drop.attributes["error_class"] != "provider_auth_rejected" || drop.attributes["http_status"] != http.StatusUnauthorized || drop.attributes["retryable"] != false || drop.attributes["retry_count"] != 0 {
		t.Fatalf("unexpected safe terminal diagnostic: %#v", drop.attributes)
	}
	if _, leaked := drop.attributes["error"]; leaked {
		t.Fatalf("raw provider error leaked: %#v", drop.attributes)
	}
	if err := manager.Stop(t.Context(), "stream-01"); err != nil {
		t.Fatal(err)
	}
}

func TestManagerCaptionIngressFailsFastAcrossProviderOutageBatch(t *testing.T) {
	session := &retryCaptionSession{failuresRemaining: 100, called: make(chan struct{}, 32)}
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
	packets := make([]deepgram.AudioPacket, 5)
	for i := range packets {
		packets[i] = deepgram.AudioPacket{SSRC: 42, UserID: "speaker-42", JobGeneration: generation, ConnectionGeneration: 1, Sequence: uint16(i + 1), Opus: []byte{1}}
	}
	if err := manager.EnqueueCaptionAudioForGeneration(t.Context(), "stream-01", generation, packets); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for manager.Status().CaptionAudioProviderDrops == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	calls, _ := session.snapshot()
	status := manager.Status()
	if calls != captionAudioRetryMax || status.CaptionAudioRetries != captionAudioRetryMax-1 || status.CaptionAudioProviderDrops != len(packets) {
		t.Fatalf("provider outage retried per packet: calls=%d status=%#v", calls, status)
	}
	if err := manager.Stop(t.Context(), "stream-01"); err != nil {
		t.Fatal(err)
	}
}
