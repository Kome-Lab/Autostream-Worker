package videoout

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/jpeg"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var (
	ErrAlreadyRunning     = errors.New("scene frame output is already running")
	ErrFrameShapeMismatch = errors.New("rendered frame shape does not match the scene frame contract")
	ErrStartupTimeout     = errors.New("scene frame output startup timed out")
	ErrTransportStopped   = errors.New("scene frame output transport stopped")
)

const (
	defaultStartupTimeout = 8 * time.Second
	defaultFrameInterval  = 500 * time.Millisecond
	defaultJPEGQuality    = 90
	defaultAvatarRefresh  = 15 * time.Minute
	maxAvatarRefresh      = 24 * time.Hour
	maxVideoDimension     = 3840
	maxEncodedFrameBytes  = 16 << 20
)

type Config struct {
	StreamID       string
	Generation     uint64
	IngestURL      string
	Passphrase     string
	PBKeylen       int
	Width          int
	Height         int
	StartupTimeout time.Duration
}

type FrameSource interface {
	RenderScene(time.Time) (*image.RGBA, error)
}

type AvatarRefresher interface {
	AvatarRefreshInterval() time.Duration
	RefreshAvatars()
}

type Options struct {
	Dialer         SRTDialer
	StartupTimeout time.Duration
	FrameInterval  time.Duration
	JPEGQuality    int
	Now            func() time.Time
	OnFailure      func(streamID string, generation uint64)
}

type Status struct {
	StreamID  string    `json:"stream_id,omitempty"`
	Running   bool      `json:"running"`
	StartedAt time.Time `json:"started_at,omitempty"`
	BytesSent uint64    `json:"bytes_sent"`
}

type Manager struct {
	source        FrameSource
	dialer        SRTDialer
	now           func() time.Time
	timeout       time.Duration
	frameInterval time.Duration
	jpegQuality   int
	onFailure     func(streamID string, generation uint64)

	lifecycleMu sync.Mutex
	mu          sync.Mutex
	starting    bool
	current     *session
}

type session struct {
	streamID   string
	generation uint64
	started    time.Time
	cancel     context.CancelFunc
	conn       SRTConn
	errors     chan error
	first      chan struct{}
	done       chan struct{}
	bytes      atomic.Uint64
	firstOnce  sync.Once
	stopOnce   sync.Once
}

func NewManager(source FrameSource, options Options) *Manager {
	dialer := options.Dialer
	if dialer == nil {
		dialer = newGoSRTDialer()
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}
	timeout := options.StartupTimeout
	if timeout <= 0 {
		timeout = defaultStartupTimeout
	}
	interval := options.FrameInterval
	if interval <= 0 {
		interval = defaultFrameInterval
	}
	quality := options.JPEGQuality
	if quality < 1 || quality > 100 {
		quality = defaultJPEGQuality
	}
	return &Manager{
		source: source, dialer: dialer, now: now, timeout: timeout,
		frameInterval: interval, jpegQuality: quality, onFailure: options.OnFailure,
	}
}

func (m *Manager) Start(ctx context.Context, config Config) error {
	m.lifecycleMu.Lock()
	defer m.lifecycleMu.Unlock()

	m.mu.Lock()
	if m.starting || m.current != nil {
		m.mu.Unlock()
		return ErrAlreadyRunning
	}
	m.starting = true
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		m.starting = false
		m.mu.Unlock()
	}()

	config, err := m.normalizeConfig(config)
	if err != nil {
		return err
	}
	destination, err := parseSRTDestination(config.IngestURL, config.StreamID, config.Passphrase, config.PBKeylen, config.StartupTimeout)
	if err != nil {
		return err
	}
	startupCtx, cancelStartup := context.WithTimeout(ctx, config.StartupTimeout)
	defer cancelStartup()
	conn, err := m.dialer.Dial(startupCtx, destination.address, destination.options)
	if err != nil {
		return errors.New("scene frame SRT connection failed")
	}
	runCtx, cancelRun := context.WithCancel(context.Background())
	s := &session{
		streamID: config.StreamID, generation: config.Generation, started: m.now().UTC(),
		cancel: cancelRun, conn: conn, errors: make(chan error, 2), first: make(chan struct{}), done: make(chan struct{}),
	}
	go m.renderFrames(runCtx, s, config)
	go m.refreshAvatars(runCtx)

	select {
	case <-s.first:
		m.mu.Lock()
		m.current = s
		m.mu.Unlock()
		go m.monitor(s)
		return nil
	case err := <-s.errors:
		_ = s.shutdown(context.Background())
		return err
	case <-startupCtx.Done():
		_ = s.shutdown(context.Background())
		if errors.Is(startupCtx.Err(), context.DeadlineExceeded) {
			return ErrStartupTimeout
		}
		return startupCtx.Err()
	}
}

func (m *Manager) refreshAvatars(ctx context.Context) {
	refresher, ok := m.source.(AvatarRefresher)
	if !ok {
		return
	}
	interval := refresher.AvatarRefreshInterval()
	if interval <= 0 || interval > maxAvatarRefresh {
		interval = defaultAvatarRefresh
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			refresher.RefreshAvatars()
		}
	}
}

func (m *Manager) Stop(ctx context.Context) error {
	m.lifecycleMu.Lock()
	defer m.lifecycleMu.Unlock()
	m.mu.Lock()
	s := m.current
	m.current = nil
	m.mu.Unlock()
	if s == nil {
		return nil
	}
	return s.shutdown(ctx)
}

func (m *Manager) Status() Status {
	m.mu.Lock()
	s := m.current
	m.mu.Unlock()
	if s == nil {
		return Status{}
	}
	return Status{StreamID: s.streamID, Running: true, StartedAt: s.started, BytesSent: s.bytes.Load()}
}

func (m *Manager) monitor(s *session) {
	select {
	case <-s.done:
	case <-s.errors:
	}
	m.lifecycleMu.Lock()
	m.mu.Lock()
	failed := m.current == s
	if m.current == s {
		m.current = nil
	}
	m.mu.Unlock()
	_ = s.shutdown(context.Background())
	m.lifecycleMu.Unlock()
	if failed && m.onFailure != nil {
		go m.onFailure(s.streamID, s.generation)
	}
}

func (m *Manager) normalizeConfig(config Config) (Config, error) {
	config.StreamID = strings.TrimSpace(config.StreamID)
	if config.StreamID == "" {
		return Config{}, errors.New("scene frame stream_id is required")
	}
	if m.source == nil {
		return Config{}, errors.New("scene frame source is unavailable")
	}
	if config.Width <= 0 || config.Height <= 0 || config.Width > maxVideoDimension || config.Height > maxVideoDimension || config.Width%2 != 0 || config.Height%2 != 0 {
		return Config{}, errors.New("scene frame dimensions must be positive, even, and at most 3840")
	}
	if config.StartupTimeout <= 0 {
		config.StartupTimeout = m.timeout
	}
	return config, nil
}

func (m *Manager) renderFrames(ctx context.Context, s *session, config Config) {
	defer close(s.done)
	if err := m.renderFrame(s, config); err != nil {
		s.signalError(err)
		return
	}
	ticker := time.NewTicker(m.frameInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := m.renderFrame(s, config); err != nil {
				s.signalError(err)
				return
			}
		}
	}
}

func (m *Manager) renderFrame(s *session, config Config) error {
	frame, err := m.source.RenderScene(m.now().UTC())
	if err != nil || frame == nil {
		return errors.New("scene frame render failed")
	}
	if frame.Bounds().Dx() != config.Width || frame.Bounds().Dy() != config.Height {
		return ErrFrameShapeMismatch
	}
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, frame, &jpeg.Options{Quality: m.jpegQuality}); err != nil {
		return errors.New("scene frame JPEG encoding failed")
	}
	if encoded.Len() <= 0 || encoded.Len() > maxEncodedFrameBytes {
		return errors.New("scene frame JPEG size is outside the supported range")
	}
	if err := writeFull(s.conn, encoded.Bytes()); err != nil {
		return errors.New("scene frame SRT write failed")
	}
	s.bytes.Add(uint64(encoded.Len()))
	s.firstOnce.Do(func() { close(s.first) })
	return nil
}

func (s *session) signalError(err error) {
	if err == nil {
		return
	}
	select {
	case s.errors <- err:
	default:
	}
}

func (s *session) shutdown(ctx context.Context) error {
	var shutdownErr error
	s.stopOnce.Do(func() {
		s.cancel()
		if err := s.conn.Close(); err != nil {
			shutdownErr = err
		}
		select {
		case <-s.done:
		case <-ctx.Done():
			if shutdownErr == nil {
				shutdownErr = ctx.Err()
			}
		}
	})
	return shutdownErr
}

func writeFull(writer io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := writer.Write(data)
		if err != nil {
			return err
		}
		if n <= 0 || n > len(data) {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}
