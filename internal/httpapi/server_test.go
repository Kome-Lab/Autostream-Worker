package httpapi

import (
	"context"
	"github.com/example/autostream-worker/internal/deepgram"
	"github.com/example/autostream-worker/internal/encoder"
	"sync"
)

type capturePublisher struct {
	events []encoder.Event
}

func (p *capturePublisher) Publish(ctx context.Context, event encoder.Event) error {
	p.events = append(p.events, event)
	return nil
}

type captureCaptionSession struct {
	mu       sync.Mutex
	packets  []deepgram.AudioPacket
	closed   int
	err      error
	ingested chan struct{}
}

func (s *captureCaptionSession) Ingest(_ context.Context, packet deepgram.AudioPacket) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	s.packets = append(s.packets, packet)
	select {
	case s.ingested <- struct{}{}:
	default:
	}
	return nil
}

func (s *captureCaptionSession) Close(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed++
	return nil
}

func (s *captureCaptionSession) snapshotPackets() []deepgram.AudioPacket {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]deepgram.AudioPacket(nil), s.packets...)
}

func (s *captureCaptionSession) snapshotClosed() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

type blockingCaptionSession struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *blockingCaptionSession) Ingest(ctx context.Context, _ deepgram.AudioPacket) error {
	s.once.Do(func() { close(s.started) })
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.release:
		return nil
	}
}

func (*blockingCaptionSession) Close(context.Context) error { return nil }
