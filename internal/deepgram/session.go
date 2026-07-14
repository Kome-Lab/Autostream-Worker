package deepgram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	ErrUnavailable = errors.New("deepgram transcription unavailable")
	ErrClosed      = errors.New("deepgram transcription session is closed")
	ErrEmptyAudio  = errors.New("opus packet is empty")
)

type Config struct {
	Model          string
	Language       string
	EndpointingMS  int
	InterimResults bool
	SmartFormat    bool
	Delay          time.Duration
}

type AudioPacket struct {
	SSRC       uint32
	UserID     string
	Sequence   uint16
	Timestamp  uint64
	ReceivedAt time.Time
	Opus       []byte
}

type Transcript struct {
	Text          string
	SpeakerUserID string
	Final         bool
	ReceivedAt    time.Time
}

type Handler func(context.Context, Transcript) error

type Session struct {
	config   Config
	options  sessionOptions
	endpoint string
	handler  Handler
	apiKey   []byte

	mu        sync.Mutex
	closed    bool
	conns     map[uint32]*speakerConnection
	dialing   map[uint32]*dialCall
	dialWG    sync.WaitGroup
	loopWG    sync.WaitGroup
	closeOnce sync.Once
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
	text       string
	final      bool
	receivedAt time.Time
}

type resultMessage struct {
	Type    string `json:"type"`
	IsFinal bool   `json:"is_final"`
	Channel struct {
		Alternatives []struct {
			Transcript string `json:"transcript"`
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
	return &Session{
		config:   config,
		options:  options,
		endpoint: endpoint,
		handler:  handler,
		apiKey:   append([]byte(nil), apiKey...),
		conns:    map[uint32]*speakerConnection{},
		dialing:  map[uint32]*dialCall{},
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
	conn, err := s.connection(ctx, packet.SSRC, strings.TrimSpace(packet.UserID))
	if err != nil {
		return err
	}
	if err := conn.writeAudio(ctx, packet.Opus); err != nil {
		s.discard(conn)
		return ErrUnavailable
	}
	return nil
}

func (s *Session) connection(ctx context.Context, ssrc uint32, userID string) (*speakerConnection, error) {
	for {
		var stale *speakerConnection
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			return nil, ErrClosed
		}
		if conn := s.conns[ssrc]; conn != nil {
			if conn.hasDifferentUser(userID) {
				delete(s.conns, ssrc)
				stale = conn
			} else {
				conn.setUserID(userID)
				s.mu.Unlock()
				return conn, nil
			}
		}
		if stale != nil {
			s.mu.Unlock()
			stale.abort()
			continue
		}
		if call := s.dialing[ssrc]; call != nil {
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
		s.dialing[ssrc] = call
		s.dialWG.Add(1)
		s.mu.Unlock()

		conn, err := s.completeDial(dialCtx, call, ssrc, userID)
		cancel()
		s.dialWG.Done()
		return conn, err
	}
}

func (s *Session) completeDial(ctx context.Context, call *dialCall, ssrc uint32, userID string) (*speakerConnection, error) {
	sock, dialErr := s.dial(ctx)
	var conn *speakerConnection
	var closeSocket bool

	s.mu.Lock()
	delete(s.dialing, ssrc)
	switch {
	case dialErr != nil:
		call.err = ErrUnavailable
		closeSocket = sock != nil
	case s.closed:
		call.err = ErrClosed
		closeSocket = true
	default:
		conn = newSpeakerConnection(s, sock, ssrc, userID)
		s.conns[ssrc] = conn
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

func newSpeakerConnection(session *Session, sock socket, ssrc uint32, userID string) *speakerConnection {
	ctx, cancel := context.WithCancel(context.Background())
	return &speakerConnection{
		session:  session,
		socket:   sock,
		ssrc:     ssrc,
		ctx:      ctx,
		cancel:   cancel,
		readDone: make(chan struct{}),
		results:  make(chan queuedTranscript, 32),
		userID:   userID,
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
			return
		}
		if messageType != websocket.MessageText {
			continue
		}
		result, ok := parseResult(payload, c.session.config.InterimResults)
		if !ok {
			continue
		}
		select {
		case c.results <- result:
		case <-c.ctx.Done():
			return
		}
	}
}

func parseResult(payload []byte, interimResults bool) (queuedTranscript, bool) {
	var message resultMessage
	if err := json.Unmarshal(payload, &message); err != nil || message.Type != "Results" || len(message.Channel.Alternatives) == 0 {
		return queuedTranscript{}, false
	}
	transcript := strings.TrimSpace(message.Channel.Alternatives[0].Transcript)
	if transcript == "" || (!message.IsFinal && !interimResults) {
		return queuedTranscript{}, false
	}
	return queuedTranscript{text: transcript, final: message.IsFinal, receivedAt: time.Now().UTC()}, true
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
			if err := c.writeControl(c.ctx, []byte(`{"type":"KeepAlive"}`)); err != nil {
				c.session.discard(c)
				return
			}
		}
	}
}

func (c *speakerConnection) publishLoop() {
	defer c.session.loopWG.Done()
	for {
		select {
		case <-c.ctx.Done():
			return
		case result := <-c.results:
			due := result.receivedAt.Add(c.session.config.Delay)
			if wait := time.Until(due); wait > 0 {
				timer := time.NewTimer(wait)
				select {
				case <-c.ctx.Done():
					timer.Stop()
					return
				case <-timer.C:
				}
			}
			_ = c.session.handler(c.ctx, Transcript{
				Text:          result.text,
				SpeakerUserID: c.currentUserID(),
				Final:         result.final,
				ReceivedAt:    result.receivedAt,
			})
		}
	}
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
	if s.conns[conn.ssrc] == conn {
		delete(s.conns, conn.ssrc)
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
	s.conns = map[uint32]*speakerConnection{}
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
