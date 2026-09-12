package deepgram

import (
	"context"
	"sync"
	"time"

	"github.com/coder/websocket"
)

type speakerConnection struct {
	session *Session
	socket  socket
	ssrc    uint32
	key     connectionKey

	stateMu           sync.Mutex
	utteranceSeq      uint64
	utteranceID       string
	utteranceText     string
	utteranceRevision int
	utteranceStarted  time.Time
	lastAudioAt       time.Time
	lastResultAt      time.Time
	lastConfidence    float64

	ctx       context.Context
	cancel    context.CancelFunc
	closeOnce sync.Once
	readDone  chan struct{}
	results   chan queuedTranscript
	writeMu   sync.Mutex
	userMu    sync.RWMutex
	userID    string
}

func newSpeakerConnection(session *Session, sock socket, key connectionKey, packet AudioPacket) *speakerConnection {
	ctx, cancel := context.WithCancel(context.Background())
	return &speakerConnection{
		session:  session,
		socket:   sock,
		ssrc:     packet.SSRC,
		key:      key,
		ctx:      ctx,
		cancel:   cancel,
		readDone: make(chan struct{}),
		results:  make(chan queuedTranscript, 32),
		userID:   packet.UserID,
	}
}

func (c *speakerConnection) start() {
	go c.readLoop()
	go c.keepAliveLoop()
	go c.publishLoop()
}

func (c *speakerConnection) readLoop() {
	defer c.session.loopWG.Done()
	defer c.session.discard(c)
	defer close(c.readDone)
	for {
		messageType, payload, err := c.socket.Read(c.ctx)
		if err != nil {
			if c.ctx.Err() == nil {
				c.session.recordProviderError(classifyProviderReadError(err))
			}
			return
		}
		if messageType != websocket.MessageText {
			continue
		}
		if isProviderErrorMessage(payload) {
			c.session.recordProviderError("provider_response_error")
			return
		}
		result, ok := parseMessage(payload, c.session.config.InterimResults)
		if !ok {
			continue
		}
		if !result.utteranceEnd {
			c.session.recordTranscriptMessage()
		}
		select {
		case c.results <- result:
		case <-c.ctx.Done():
			return
		}
	}
}

func (c *speakerConnection) keepAliveLoop() {
	defer c.session.loopWG.Done()
	ticker := time.NewTicker(c.session.options.keepAliveInterval)
	defer ticker.Stop()
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-ticker.C:
			if c.hasRecentAudio() {
				continue
			}
			if err := c.writeControl(c.ctx, []byte(`{"type":"KeepAlive"}`)); err != nil {
				c.session.discard(c)
				return
			}
		}
	}
}

func (c *speakerConnection) markAudio(receivedAt time.Time) {
	if receivedAt.IsZero() {
		receivedAt = time.Now().UTC()
	}
	c.stateMu.Lock()
	c.lastAudioAt = receivedAt.UTC()
	c.stateMu.Unlock()
}

func (c *speakerConnection) hasRecentAudio() bool {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	if c.lastAudioAt.IsZero() {
		return false
	}
	return time.Since(c.lastAudioAt) < c.session.options.keepAliveInterval
}

func (c *speakerConnection) idleDue(now time.Time) bool {
	if c.session.config.SpeakerIdleClose <= 0 {
		return false
	}
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	return !c.lastAudioAt.IsZero() && now.Sub(c.lastAudioAt) >= c.session.config.SpeakerIdleClose
}

func (c *speakerConnection) writeAudio(ctx context.Context, payload []byte) error {
	if len(payload) == 0 {
		return ErrEmptyAudio
	}
	writeCtx, cancel := context.WithTimeout(ctx, c.session.options.writeTimeout)
	defer cancel()
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if err := c.socket.Write(writeCtx, websocket.MessageBinary, payload); err != nil {
		return ErrUnavailable
	}
	return nil
}

func (c *speakerConnection) writeControl(ctx context.Context, payload []byte) error {
	writeCtx, cancel := context.WithTimeout(ctx, c.session.options.writeTimeout)
	defer cancel()
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if err := c.socket.Write(writeCtx, websocket.MessageText, payload); err != nil {
		return ErrUnavailable
	}
	return nil
}

func (c *speakerConnection) hasDifferentUser(userID string) bool {
	if userID == "" {
		return false
	}
	c.userMu.RLock()
	defer c.userMu.RUnlock()
	return c.userID != "" && c.userID != userID
}

func (c *speakerConnection) setUserID(userID string) {
	if userID == "" {
		return
	}
	c.userMu.Lock()
	if c.userID == "" {
		c.userID = userID
	}
	c.userMu.Unlock()
}

func (c *speakerConnection) currentUserID() string {
	c.userMu.RLock()
	defer c.userMu.RUnlock()
	return c.userID
}

func (c *speakerConnection) abort() {
	c.closeOnce.Do(func() {
		c.cancel()
		_ = c.socket.CloseNow()
	})
}

func (c *speakerConnection) gracefulClose() {
	c.closeOnce.Do(func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), c.session.options.writeTimeout)
		_ = c.writeControl(closeCtx, []byte(`{"type":"Finalize"}`))
		err := c.writeControl(closeCtx, []byte(`{"type":"CloseStream"}`))
		cancel()
		if err == nil {
			timer := time.NewTimer(c.session.options.closeTimeout)
			select {
			case <-c.readDone:
				timer.Stop()
			case <-timer.C:
			}
		}
		c.cancel()
		_ = c.socket.CloseNow()
	})
}
