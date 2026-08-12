package videoout

import (
	"context"
	"errors"
	"fmt"
	"image"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var (
	ErrAlreadyRunning     = errors.New("video output is already running")
	ErrFrameShapeMismatch = errors.New("rendered frame shape does not match the video contract")
	ErrStartupTimeout     = errors.New("video output startup timed out")
	ErrTransportStopped   = errors.New("video output transport stopped")
)

const (
	defaultStartupTimeout = 8 * time.Second
	defaultFFmpegBinary   = "ffmpeg"
	defaultAvatarRefresh  = 15 * time.Minute
	maxAvatarRefresh      = 24 * time.Hour
	maxVideoDimension     = 3840
	maxVideoFPS           = 60
)

type Config struct {
	StreamID       string
	Generation     uint64
	IngestURL      string
	Passphrase     string
	PBKeylen       int
	Width          int
	Height         int
	FPS            int
	BitrateKbps    int
	FFmpegBinary   string
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
	ProcessFactory ProcessFactory
	StartupTimeout time.Duration
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
	source    FrameSource
	dialer    SRTDialer
	procs     ProcessFactory
	now       func() time.Time
	timeout   time.Duration
	onFailure func(streamID string, generation uint64)

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
	process    Process
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
	processFactory := options.ProcessFactory
	if processFactory == nil {
		processFactory = execProcessFactory{}
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}
	timeout := options.StartupTimeout
	if timeout <= 0 {
		timeout = defaultStartupTimeout
	}
	return &Manager{
		source: source, dialer: dialer, procs: processFactory, now: now, timeout: timeout,
		onFailure: options.OnFailure,
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
		return errors.New("video SRT connection failed")
	}
	process, err := m.procs.Start(config.FFmpegBinary, buildFFmpegArgs(config))
	if err != nil {
		_ = conn.Close()
		return errors.New("video encoder process failed to start")
	}
	runCtx, cancelRun := context.WithCancel(context.Background())
	s := &session{
		streamID:   config.StreamID,
		generation: config.Generation,
		started:    m.now().UTC(),
		cancel:     cancelRun,
		process:    process,
		conn:       conn,
		errors:     make(chan error, 4),
		first:      make(chan struct{}),
		done:       make(chan struct{}),
	}
	go s.waitProcess(runCtx)
	go s.discardStderr()
	go s.pumpMPEGTS(runCtx)
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
		return Config{}, errors.New("video stream_id is required")
	}
	if m.source == nil {
		return Config{}, errors.New("video frame source is unavailable")
	}
	if config.Width <= 0 || config.Height <= 0 || config.Width > maxVideoDimension || config.Height > maxVideoDimension || config.Width%2 != 0 || config.Height%2 != 0 {
		return Config{}, errors.New("video dimensions must be positive, even, and at most 3840")
	}
	if config.FPS <= 0 || config.FPS > maxVideoFPS {
		return Config{}, errors.New("video fps must be between 1 and 60")
	}
	if config.BitrateKbps <= 0 {
		config.BitrateKbps = defaultBitrateKbps(config.Width, config.Height)
	}
	if config.BitrateKbps < 100 || config.BitrateKbps > 50000 {
		return Config{}, errors.New("video bitrate is outside the supported range")
	}
	config.FFmpegBinary = strings.TrimSpace(config.FFmpegBinary)
	if config.FFmpegBinary == "" {
		config.FFmpegBinary = defaultFFmpegBinary
	}
	if config.StartupTimeout <= 0 {
		config.StartupTimeout = m.timeout
	}
	return config, nil
}

func (m *Manager) renderFrames(ctx context.Context, s *session, config Config) {
	interval := time.Second / time.Duration(config.FPS)
	if err := m.renderFrame(s.process.Stdin(), config); err != nil {
		s.signalError(err)
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := m.renderFrame(s.process.Stdin(), config); err != nil {
				s.signalError(err)
				return
			}
		}
	}
}

func (m *Manager) renderFrame(writer io.Writer, config Config) error {
	frame, err := m.source.RenderScene(m.now().UTC())
	if err != nil || frame == nil {
		return errors.New("video scene render failed")
	}
	if frame.Bounds().Dx() != config.Width || frame.Bounds().Dy() != config.Height {
		return ErrFrameShapeMismatch
	}
	rowBytes := config.Width * 4
	for y := frame.Bounds().Min.Y; y < frame.Bounds().Max.Y; y++ {
		offset := frame.PixOffset(frame.Bounds().Min.X, y)
		if offset < 0 || offset+rowBytes > len(frame.Pix) {
			return ErrFrameShapeMismatch
		}
		if err := writeFull(writer, frame.Pix[offset:offset+rowBytes]); err != nil {
			return errors.New("video frame pipe failed")
		}
	}
	return nil
}

func (s *session) waitProcess(ctx context.Context) {
	err := s.process.Wait()
	close(s.done)
	if ctx.Err() != nil {
		return
	}
	if err == nil {
		err = ErrTransportStopped
	} else {
		err = errors.New("video encoder process stopped")
	}
	s.signalError(err)
}

func (s *session) discardStderr() {
	_, _ = io.Copy(io.Discard, s.process.Stderr())
}

func (s *session) pumpMPEGTS(ctx context.Context) {
	buffer := make([]byte, 32*1024)
	for {
		n, readErr := s.process.Stdout().Read(buffer)
		if n > 0 {
			if err := writeFull(s.conn, buffer[:n]); err != nil {
				if ctx.Err() == nil {
					s.signalError(errors.New("video SRT write failed"))
				}
				return
			}
			s.bytes.Add(uint64(n))
			s.firstOnce.Do(func() { close(s.first) })
		}
		if readErr != nil {
			if ctx.Err() == nil {
				s.signalError(ErrTransportStopped)
			}
			return
		}
	}
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
		_ = s.process.Stdin().Close()
		_ = s.conn.Close()
		_ = s.process.Stdout().Close()
		_ = s.process.Stderr().Close()
		if err := s.process.Kill(); err != nil {
			shutdownErr = fmt.Errorf("stop video encoder: %w", err)
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
