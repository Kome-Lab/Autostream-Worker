package deepgram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
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

type Config struct {
	Model             string
	Language          string
	EndpointingMS     int
	UtteranceEndMS    int
	LocalFinalize     time.Duration
	SpeakerIdleClose  time.Duration
	KeepAliveInterval time.Duration
	InterimResults    bool
	SmartFormat       bool
	Keyterms          []string
	MIPOptOut         bool
	ReplayBufferMax   time.Duration
	Delay             time.Duration
}

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
}

// A user and connection generation identify a speaker. SSRC is only retained
// for packets that arrive before Discord has resolved the speaking user.
type connectionKey struct {
	UserID         string
	Generation     uint64
	UnresolvedSSRC uint32
}

type sessionOptions struct {
	endpoint          string
	dialer            socketDialer
	keepAliveInterval time.Duration
	dialTimeout       time.Duration
	writeTimeout      time.Duration
	closeTimeout      time.Duration
	maxConnections    int
}

type socketDialer interface {
	Dial(context.Context, string, http.Header) (socket, error)
}

type socket interface {
	Read(context.Context) (websocket.MessageType, []byte, error)
	Write(context.Context, websocket.MessageType, []byte) error
	Close(websocket.StatusCode, string) error
	CloseNow() error
	SetReadLimit(int64)
}

type coderDialer struct {
	client *http.Client
}

type dialCall struct {
	done   chan struct{}
	cancel context.CancelFunc
	conn   *speakerConnection
	err    error
}

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

func (o sessionOptions) withDefaults() sessionOptions {
	if strings.TrimSpace(o.endpoint) == "" {
		o.endpoint = Endpoint
	}
	if o.dialer == nil {
		o.dialer = coderDialer{client: &http.Client{
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}}
	}
	if o.keepAliveInterval <= 0 {
		o.keepAliveInterval = defaultKeepAliveInterval
	}
	if o.dialTimeout <= 0 {
		o.dialTimeout = defaultDialTimeout
	}
	if o.writeTimeout <= 0 {
		o.writeTimeout = defaultWriteTimeout
	}
	if o.closeTimeout <= 0 {
		o.closeTimeout = defaultCloseTimeout
	}
	if o.maxConnections <= 0 {
		o.maxConnections = defaultMaxConnections
	}
	return o
}

func ListenURL(config Config) (string, error) {
	return buildListenURL(Endpoint, config)
}

func buildListenURL(endpoint string, config Config) (string, error) {
	if err := validateConfig(config); err != nil {
		return "", err
	}
	parsed, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil || (parsed.Scheme != "wss" && parsed.Scheme != "ws") || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("deepgram endpoint is invalid")
	}
	query := parsed.Query()
	query.Set("encoding", "opus")
	query.Set("sample_rate", "48000")
	query.Set("channels", "2")
	query.Set("model", config.Model)
	query.Set("language", config.Language)
	query.Set("interim_results", strconv.FormatBool(config.InterimResults))
	query.Set("smart_format", strconv.FormatBool(config.SmartFormat))
	query.Set("endpointing", strconv.Itoa(config.EndpointingMS))
	if config.UtteranceEndMS > 0 {
		query.Set("utterance_end_ms", strconv.Itoa(config.UtteranceEndMS))
	}
	if config.MIPOptOut {
		query.Set("mip_opt_out", "true")
	}
	for _, keyterm := range config.Keyterms {
		keyterm = strings.TrimSpace(keyterm)
		if keyterm != "" {
			query.Add("keyterm", keyterm)
		}
	}
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}

func validateConfig(config Config) error {
	if strings.TrimSpace(config.Model) == "" || len(config.Model) > 64 {
		return errors.New("deepgram model is invalid")
	}
	if strings.TrimSpace(config.Language) == "" || len(config.Language) > 32 {
		return errors.New("deepgram language is invalid")
	}
	if config.EndpointingMS < 10 || config.EndpointingMS > 5000 {
		return errors.New("deepgram endpointing_ms is invalid")
	}
	if config.Delay < 0 || config.Delay > 10*time.Second {
		return errors.New("deepgram delay is invalid")
	}
	if config.UtteranceEndMS < 0 || config.UtteranceEndMS > 10000 {
		return errors.New("deepgram utterance_end_ms is invalid")
	}
	if config.LocalFinalize < 0 || config.LocalFinalize > 10*time.Second || config.SpeakerIdleClose < 0 || config.SpeakerIdleClose > 2*time.Minute || config.KeepAliveInterval < 0 || config.KeepAliveInterval > time.Minute {
		return errors.New("deepgram session timing is invalid")
	}
	if len(config.Keyterms) > 20 {
		return errors.New("deepgram keyterms are invalid")
	}
	for _, keyterm := range config.Keyterms {
		if keyterm = strings.TrimSpace(keyterm); keyterm == "" || len(keyterm) > 128 {
			return errors.New("deepgram keyterms are invalid")
		}
	}
	return nil
}

func (d coderDialer) Dial(ctx context.Context, endpoint string, header http.Header) (socket, error) {
	conn, _, err := websocket.Dial(ctx, endpoint, &websocket.DialOptions{
		HTTPClient: d.client,
		HTTPHeader: header,
	})
	if err != nil {
		return nil, err
	}
	return conn, nil
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
		s.recordProviderError("provider_write_failed")
		s.discard(conn)
		return ErrUnavailable
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
	if s == nil || strings.TrimSpace(errorClass) == "" {
		return
	}
	s.mu.Lock()
	s.providerErrors++
	s.lastErrorClass = errorClass
	s.mu.Unlock()
}

func connectionKeyFor(packet AudioPacket) connectionKey {
	if packet.UserID != "" {
		return connectionKey{UserID: packet.UserID, Generation: packet.ConnectionGeneration}
	}
	return connectionKey{Generation: packet.ConnectionGeneration, UnresolvedSSRC: packet.SSRC}
}

func (s *Session) connection(ctx context.Context, packet AudioPacket) (*speakerConnection, error) {
	key := connectionKeyFor(packet)
	for {
		var stale []*speakerConnection
		var staleDials []*dialCall
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			return nil, ErrClosed
		}
		if s.latestConnectionGeneration > 0 && packet.ConnectionGeneration < s.latestConnectionGeneration {
			s.mu.Unlock()
			return nil, ErrStaleConnectionGeneration
		}
		if packet.ConnectionGeneration > s.latestConnectionGeneration {
			s.latestConnectionGeneration = packet.ConnectionGeneration
			for candidateKey, call := range s.dialing {
				if candidateKey.Generation < packet.ConnectionGeneration {
					delete(s.dialing, candidateKey)
					staleDials = append(staleDials, call)
				}
			}
			for candidateKey, conn := range s.conns {
				if candidateKey.Generation < packet.ConnectionGeneration {
					delete(s.conns, candidateKey)
					stale = append(stale, conn)
				}
			}
		}
		if len(stale) > 0 || len(staleDials) > 0 {
			s.mu.Unlock()
			for _, call := range staleDials {
				call.cancel()
			}
			for _, conn := range stale {
				conn.abort()
			}
			continue
		}
		if conn := s.conns[key]; conn != nil {
			s.mu.Unlock()
			return conn, nil
		}
		if packet.UserID != "" || packet.ConnectionGeneration != 0 {
			for candidateKey, conn := range s.conns {
				if candidateKey == key {
					continue
				}
				sameUser := packet.UserID != "" && candidateKey.UserID == packet.UserID
				sameSSRC := packet.SSRC != 0 && conn.ssrc == packet.SSRC
				if !sameUser && !sameSSRC {
					continue
				}
				delete(s.conns, candidateKey)
				stale = append(stale, conn)
			}
		}
		if len(stale) > 0 {
			s.mu.Unlock()
			for _, conn := range stale {
				conn.abort()
			}
			continue
		}
		if call := s.dialing[key]; call != nil {
			done := call.done
			s.mu.Unlock()
			select {
			case <-ctx.Done():
				return nil, ErrUnavailable
			case <-done:
				if call.err != nil {
					return nil, call.err
				}
				if call.conn != nil {
					continue
				}
				return nil, ErrUnavailable
			}
		}
		if len(s.conns)+len(s.dialing) >= s.options.maxConnections {
			s.mu.Unlock()
			return nil, ErrUnavailable
		}
		dialCtx, cancel := context.WithTimeout(ctx, s.options.dialTimeout)
		call := &dialCall{done: make(chan struct{}), cancel: cancel}
		s.dialing[key] = call
		s.dialWG.Add(1)
		s.mu.Unlock()

		conn, err := s.completeDial(dialCtx, call, key, packet)
		cancel()
		s.dialWG.Done()
		return conn, err
	}
}

func (s *Session) completeDial(ctx context.Context, call *dialCall, key connectionKey, packet AudioPacket) (*speakerConnection, error) {
	sock, dialErr := s.dial(ctx)
	if dialErr != nil && ctx.Err() == nil {
		s.recordProviderError("provider_dial_failed")
	}
	var conn *speakerConnection
	var closeSocket bool

	s.mu.Lock()
	delete(s.dialing, key)
	switch {
	case dialErr != nil:
		call.err = ErrUnavailable
		closeSocket = sock != nil
	case s.closed:
		call.err = ErrClosed
		closeSocket = true
	case s.latestConnectionGeneration > 0 && key.Generation < s.latestConnectionGeneration:
		call.err = ErrStaleConnectionGeneration
		closeSocket = true
	default:
		conn = newSpeakerConnection(s, sock, key, packet)
		s.conns[key] = conn
		s.loopWG.Add(3)
		call.conn = conn
	}
	close(call.done)
	s.mu.Unlock()

	if closeSocket && sock != nil {
		_ = sock.CloseNow()
	}
	if conn != nil {
		conn.start()
		return conn, nil
	}
	return nil, call.err
}

func (s *Session) dial(ctx context.Context) (socket, error) {
	authorization := make([]byte, len("Token ")+len(s.apiKey))
	copy(authorization, "Token ")
	copy(authorization[len("Token "):], s.apiKey)
	header := make(http.Header)
	header.Set("Authorization", string(authorization))
	conn, err := s.options.dialer.Dial(ctx, s.endpoint, header)
	header.Del("Authorization")
	zeroBytes(authorization)
	if err != nil || conn == nil {
		return nil, ErrUnavailable
	}
	conn.SetReadLimit(maxResultMessageBytes)
	return conn, nil
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

func classifyProviderReadError(err error) string {
	switch websocket.CloseStatus(err) {
	case websocket.StatusPolicyViolation:
		return "provider_audio_rejected"
	case websocket.StatusInternalError:
		return "provider_unavailable"
	default:
		return "provider_read_closed"
	}
}

func isProviderErrorMessage(payload []byte) bool {
	var envelope struct {
		Type string `json:"type"`
	}
	return json.Unmarshal(payload, &envelope) == nil && strings.EqualFold(strings.TrimSpace(envelope.Type), "error")
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

func (s *Session) discard(conn *speakerConnection) {
	s.mu.Lock()
	if s.conns[conn.key] == conn {
		delete(s.conns, conn.key)
	}
	s.mu.Unlock()
	conn.abort()
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
