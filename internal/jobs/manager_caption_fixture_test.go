package jobs

import (
	"context"
	"github.com/example/autostream-worker/internal/deepgram"
	"github.com/example/autostream-worker/internal/encoder"
	"sync"
	"testing"
	"time"
)

type fakePublisher struct {
	events  []encoder.Event
	err     error
	mu      sync.Mutex
	handler func(encoder.Event) error
}

func (f *fakePublisher) Publish(ctx context.Context, event encoder.Event) error {
	if f.handler != nil {
		if err := f.handler(event); err != nil {
			return err
		}
	}
	if f.err != nil {
		return f.err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, event)
	return nil
}

func (f *fakePublisher) snapshot() []encoder.Event {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]encoder.Event(nil), f.events...)
}

type fakeCaptionSession struct {
	packets      []deepgram.AudioPacket
	handler      deepgram.Handler
	closed       int
	err          error
	ingestCalls  int
	ingestErrors []error
	status       deepgram.Status
}

type controlledCaptionSession struct {
	mu      sync.Mutex
	packets []deepgram.AudioPacket
	started chan struct{}
	block   bool
	once    sync.Once
	closed  int
}

type retryCaptionSession struct {
	mu                sync.Mutex
	failuresRemaining int
	failureErr        error
	calls             int
	packets           []deepgram.AudioPacket
	called            chan struct{}
}

type classifiedCaptionProviderError struct {
	errorClass string
	httpStatus int
	retryable  bool
}

func (e classifiedCaptionProviderError) Error() string { return "provider detail must not be reported" }
func (e classifiedCaptionProviderError) Unwrap() error { return deepgram.ErrUnavailable }
func (e classifiedCaptionProviderError) SafeErrorClass() string {
	return e.errorClass
}
func (e classifiedCaptionProviderError) SafeHTTPStatus() int { return e.httpStatus }
func (e classifiedCaptionProviderError) Retryable() bool     { return e.retryable }

type capturedReport struct {
	name       string
	attributes map[string]any
}

type channelReporter struct {
	reports chan capturedReport
}

func (r *channelReporter) Event(_ context.Context, _, name, _ string, attributes map[string]any) error {
	select {
	case r.reports <- capturedReport{name: name, attributes: attributes}:
	default:
	}
	return nil
}

func (*channelReporter) Metric(context.Context, string, string, string, float64, map[string]any) error {
	return nil
}

type closeBlockingCaptionSession struct {
	closeStarted chan struct{}
	releaseClose chan struct{}
	closeOnce    sync.Once
}

func (*closeBlockingCaptionSession) Ingest(context.Context, deepgram.AudioPacket) error {
	return nil
}

func (s *closeBlockingCaptionSession) Close(ctx context.Context) error {
	s.closeOnce.Do(func() { close(s.closeStarted) })
	select {
	case <-s.releaseClose:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *retryCaptionSession) Ingest(_ context.Context, packet deepgram.AudioPacket) error {
	s.mu.Lock()
	s.calls++
	failed := s.failuresRemaining > 0
	if failed {
		s.failuresRemaining--
	} else {
		s.packets = append(s.packets, packet)
	}
	s.mu.Unlock()
	select {
	case s.called <- struct{}{}:
	default:
	}
	if failed {
		if s.failureErr != nil {
			return s.failureErr
		}
		return deepgram.ErrUnavailable
	}
	return nil
}

func (*retryCaptionSession) Close(context.Context) error { return nil }

func (s *retryCaptionSession) snapshot() (int, []deepgram.AudioPacket) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls, append([]deepgram.AudioPacket(nil), s.packets...)
}

func waitForCaptionSessionCalls(t *testing.T, session *retryCaptionSession, want int) {
	t.Helper()
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	for {
		calls, _ := session.snapshot()
		if calls >= want {
			return
		}
		select {
		case <-session.called:
		case <-deadline.C:
			t.Fatalf("caption session calls = %d, want at least %d", calls, want)
		}
	}
}

func (s *controlledCaptionSession) Ingest(ctx context.Context, packet deepgram.AudioPacket) error {
	s.mu.Lock()
	s.packets = append(s.packets, packet)
	s.mu.Unlock()
	s.once.Do(func() { close(s.started) })
	if !s.block {
		return nil
	}
	<-ctx.Done()
	return ctx.Err()
}

func (s *controlledCaptionSession) Close(context.Context) error {
	s.mu.Lock()
	s.closed++
	s.mu.Unlock()
	return nil
}

func (s *controlledCaptionSession) snapshot() ([]deepgram.AudioPacket, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]deepgram.AudioPacket(nil), s.packets...), s.closed
}

func (s *fakeCaptionSession) Ingest(_ context.Context, packet deepgram.AudioPacket) error {
	call := s.ingestCalls
	s.ingestCalls++
	if call < len(s.ingestErrors) && s.ingestErrors[call] != nil {
		return s.ingestErrors[call]
	}
	if s.err != nil {
		return s.err
	}
	s.packets = append(s.packets, packet)
	return nil
}

func (s *fakeCaptionSession) Close(context.Context) error {
	s.closed++
	return nil
}

func (s *fakeCaptionSession) Status() deepgram.Status { return s.status }
