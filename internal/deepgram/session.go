package deepgram

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"time"
)

const (
	Endpoint                 = "wss://api.deepgram.com/v1/listen"
	defaultKeepAliveInterval = 4 * time.Second
	defaultDialTimeout       = 10 * time.Second
	defaultWriteTimeout      = 5 * time.Second
	defaultCloseTimeout      = 2 * time.Second
	defaultMaxConnections    = 100
	maxResultMessageBytes    = 1 << 20
)

var (
	ErrUnavailable               = errors.New("deepgram transcription unavailable")
	ErrClosed                    = errors.New("deepgram transcription session is closed")
	ErrEmptyAudio                = errors.New("opus packet is empty")
	ErrStaleConnectionGeneration = errors.New("deepgram connection generation is stale")
)

type AudioPacket struct {
	SSRC                 uint32
	UserID               string
	JobGeneration        uint64
	ConnectionGeneration uint64
	Sequence             uint16
	Timestamp            uint64
	ReceivedAt           time.Time
	Opus                 []byte
}

type Session struct {
	config   Config
	options  sessionOptions
	endpoint string
	handler  Handler
	apiKey   []byte

	mu                         sync.Mutex
	closed                     bool
	latestConnectionGeneration uint64
	conns                      map[connectionKey]*speakerConnection
	dialing                    map[connectionKey]*dialCall
	dialWG                     sync.WaitGroup
	loopWG                     sync.WaitGroup
	closeOnce                  sync.Once
	audioPacketsSent           uint64
	transcriptMessages         uint64
	providerErrors             uint64
	lastErrorClass             string
	lastHTTPStatus             int
	terminalProviderErr        error
}

// Status exposes only bounded counters and stable error classes. Provider
// payloads, close reasons, endpoints, and credentials are intentionally never
// retained here.
type Status struct {
	ActiveConnections  int    `json:"active_connections"`
	AudioPacketsSent   uint64 `json:"audio_packets_sent"`
	TranscriptMessages uint64 `json:"transcript_messages"`
	ProviderErrors     uint64 `json:"provider_errors"`
	LastErrorClass     string `json:"last_error_class,omitempty"`
	LastHTTPStatus     int    `json:"last_http_status,omitempty"`
}

func NewSession(config Config, apiKey []byte, handler Handler) (*Session, error) {
	return newSession(config, apiKey, handler, sessionOptions{})
}

func newSession(config Config, apiKey []byte, handler Handler, options sessionOptions) (*Session, error) {
	if err := validateConfig(config); err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(apiKey)) == 0 {
		return nil, errors.New("deepgram api key is required")
	}
	if handler == nil {
		return nil, errors.New("deepgram transcript handler is required")
	}
	options = options.withDefaults()
	endpoint, err := buildListenURL(options.endpoint, config)
	if err != nil {
		return nil, errors.New("deepgram listen configuration is invalid")
	}
	if config.KeepAliveInterval > 0 {
		options.keepAliveInterval = config.KeepAliveInterval
	}
	return &Session{
		config:   config,
		options:  options,
		endpoint: endpoint,
		handler:  handler,
		apiKey:   append([]byte(nil), apiKey...),
		conns:    map[connectionKey]*speakerConnection{},
		dialing:  map[connectionKey]*dialCall{},
	}, nil
}

func (s *Session) Ingest(ctx context.Context, packet AudioPacket) error {
	if len(packet.Opus) == 0 {
		return ErrEmptyAudio
	}
	packet.UserID = strings.TrimSpace(packet.UserID)
	conn, err := s.connection(ctx, packet)
	if err != nil {
		return err
	}
	if err := conn.writeAudio(ctx, packet.Opus); err != nil {
		providerErr := newProviderError("provider_write_failed", 0, true)
		s.recordProviderErrorWithStatus("provider_write_failed", 0)
		s.discard(conn)
		return providerErr
	}
	conn.markAudio(packet.ReceivedAt)
	s.recordAudioPacketSent()
	return nil
}

func (s *Session) Status() Status {
	if s == nil {
		return Status{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return Status{
		ActiveConnections:  len(s.conns),
		AudioPacketsSent:   s.audioPacketsSent,
		TranscriptMessages: s.transcriptMessages,
		ProviderErrors:     s.providerErrors,
		LastErrorClass:     s.lastErrorClass,
		LastHTTPStatus:     s.lastHTTPStatus,
	}
}

func (s *Session) recordAudioPacketSent() {
	s.mu.Lock()
	s.audioPacketsSent++
	s.mu.Unlock()
}

func (s *Session) recordTranscriptMessage() {
	s.mu.Lock()
	s.transcriptMessages++
	s.mu.Unlock()
}

func (s *Session) recordProviderError(errorClass string) {
	s.recordProviderErrorWithStatus(errorClass, 0)
}

func (s *Session) recordProviderErrorWithStatus(errorClass string, httpStatus int) {
	if s == nil || strings.TrimSpace(errorClass) == "" {
		return
	}
	s.mu.Lock()
	s.providerErrors++
	s.lastErrorClass = errorClass
	s.lastHTTPStatus = safeHTTPStatus(httpStatus)
	s.mu.Unlock()
}

func (s *Session) Close(context.Context) error {
	s.closeOnce.Do(s.close)
	return nil
}

func (s *Session) close() {
	s.mu.Lock()
	s.closed = true
	connections := make([]*speakerConnection, 0, len(s.conns))
	for _, conn := range s.conns {
		connections = append(connections, conn)
	}
	s.conns = map[connectionKey]*speakerConnection{}
	for _, call := range s.dialing {
		call.cancel()
	}
	s.mu.Unlock()

	var closeWG sync.WaitGroup
	closeWG.Add(len(connections))
	for _, conn := range connections {
		go func() {
			defer closeWG.Done()
			conn.gracefulClose()
		}()
	}
	s.dialWG.Wait()
	closeWG.Wait()
	s.loopWG.Wait()

	s.mu.Lock()
	zeroBytes(s.apiKey)
	s.apiKey = nil
	s.mu.Unlock()
}

func zeroBytes(value []byte) {
	for i := range value {
		value[i] = 0
	}
}
