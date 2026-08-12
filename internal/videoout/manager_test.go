package videoout

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestManagerStartWaitsForInitialFrameToReachSRTAndDoesNotLeakSecretsToProcess(t *testing.T) {
	secret := "job-scoped-passphrase-32-bytes-ok"
	ingestURL := "srt://encoder.example:10080"
	conn := &recordingSRTConn{}
	process := newEchoProcess([]byte{0x47, 0x40, 0x00, 0x10})
	factory := &recordingProcessFactory{process: process}
	manager := NewManager(staticFrameSource{frame: image.NewRGBA(image.Rect(0, 0, 4, 2))}, Options{
		Dialer:         staticDialer{conn: conn},
		ProcessFactory: factory,
		StartupTimeout: time.Second,
	})

	err := manager.Start(t.Context(), Config{
		StreamID:    "stream-01",
		IngestURL:   ingestURL,
		Passphrase:  secret,
		PBKeylen:    32,
		Width:       4,
		Height:      2,
		FPS:         30,
		BitrateKbps: 1000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if conn.BytesWritten() == 0 {
		t.Fatal("Start returned before MPEG-TS reached the SRT connection")
	}
	joined := strings.Join(factory.Args(), " ")
	for _, forbidden := range []string{secret, ingestURL, "encoder.example"} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("process argv leaked %q: %s", forbidden, joined)
		}
	}
	if status := manager.Status(); !status.Running || status.StreamID != "stream-01" {
		t.Fatalf("unexpected status: %#v", status)
	}
	if err := manager.Stop(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !conn.Closed() || !process.KilledOrExited() {
		t.Fatalf("transport resources were not closed: conn=%v process=%v", conn.Closed(), process.KilledOrExited())
	}
}

func TestManagerStartFailsIfFrameShapeDoesNotMatchContract(t *testing.T) {
	conn := &recordingSRTConn{}
	process := newEchoProcess([]byte{0x47})
	manager := NewManager(staticFrameSource{frame: image.NewRGBA(image.Rect(0, 0, 3, 2))}, Options{
		Dialer:         staticDialer{conn: conn},
		ProcessFactory: &recordingProcessFactory{process: process},
		StartupTimeout: time.Second,
	})
	err := manager.Start(t.Context(), validTestConfig())
	if !errors.Is(err, ErrFrameShapeMismatch) {
		t.Fatalf("error = %v, want ErrFrameShapeMismatch", err)
	}
	if status := manager.Status(); status.Running {
		t.Fatalf("failed transport remained active: %#v", status)
	}
	if !conn.Closed() || !process.KilledOrExited() {
		t.Fatal("failed startup did not close transport resources")
	}
}

func TestManagerStartFailsClosedWhenEncoderProducesNoOutput(t *testing.T) {
	conn := &recordingSRTConn{}
	process := newSilentProcess()
	manager := NewManager(staticFrameSource{frame: image.NewRGBA(image.Rect(0, 0, 4, 2))}, Options{
		Dialer:         staticDialer{conn: conn},
		ProcessFactory: &recordingProcessFactory{process: process},
		StartupTimeout: 40 * time.Millisecond,
	})
	err := manager.Start(t.Context(), validTestConfig())
	if !errors.Is(err, ErrStartupTimeout) {
		t.Fatalf("error = %v, want ErrStartupTimeout", err)
	}
	if conn.BytesWritten() != 0 || !conn.Closed() || !process.KilledOrExited() {
		t.Fatalf("silent process did not fail closed: bytes=%d conn_closed=%v process_done=%v", conn.BytesWritten(), conn.Closed(), process.KilledOrExited())
	}
}

func TestManagerRejectsConcurrentStart(t *testing.T) {
	manager := NewManager(staticFrameSource{frame: image.NewRGBA(image.Rect(0, 0, 4, 2))}, Options{
		Dialer:         staticDialer{conn: &recordingSRTConn{}},
		ProcessFactory: &recordingProcessFactory{process: newEchoProcess([]byte{0x47})},
		StartupTimeout: time.Second,
	})
	if err := manager.Start(t.Context(), validTestConfig()); err != nil {
		t.Fatal(err)
	}
	defer manager.Stop(context.Background())
	if err := manager.Start(t.Context(), validTestConfig()); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("error = %v, want ErrAlreadyRunning", err)
	}
}

func TestManagerReportsUnexpectedTransportFailureOnce(t *testing.T) {
	process := newEchoProcess([]byte{0x47})
	failures := make(chan string, 2)
	manager := NewManager(staticFrameSource{frame: image.NewRGBA(image.Rect(0, 0, 4, 2))}, Options{
		Dialer:         staticDialer{conn: &recordingSRTConn{}},
		ProcessFactory: &recordingProcessFactory{process: process},
		StartupTimeout: time.Second,
		OnFailure: func(streamID string, generation uint64) {
			failures <- fmt.Sprintf("%s:%d", streamID, generation)
		},
	})
	config := validTestConfig()
	config.Generation = 42
	if err := manager.Start(t.Context(), config); err != nil {
		t.Fatal(err)
	}
	if err := process.Kill(); err != nil {
		t.Fatal(err)
	}
	select {
	case streamID := <-failures:
		if streamID != "stream-01:42" {
			t.Fatalf("failure identity = %q", streamID)
		}
	case <-time.After(time.Second):
		t.Fatal("unexpected transport exit was not reported")
	}
	select {
	case streamID := <-failures:
		t.Fatalf("failure reported more than once for %q", streamID)
	case <-time.After(20 * time.Millisecond):
	}
	if status := manager.Status(); status.Running {
		t.Fatalf("failed transport remained active: %#v", status)
	}
}

func TestManagerDoesNotReportRequestedStopAsFailure(t *testing.T) {
	failures := make(chan string, 1)
	manager := NewManager(staticFrameSource{frame: image.NewRGBA(image.Rect(0, 0, 4, 2))}, Options{
		Dialer:         staticDialer{conn: &recordingSRTConn{}},
		ProcessFactory: &recordingProcessFactory{process: newEchoProcess([]byte{0x47})},
		StartupTimeout: time.Second,
		OnFailure: func(streamID string, _ uint64) {
			failures <- streamID
		},
	})
	if err := manager.Start(t.Context(), validTestConfig()); err != nil {
		t.Fatal(err)
	}
	if err := manager.Stop(t.Context()); err != nil {
		t.Fatal(err)
	}
	select {
	case streamID := <-failures:
		t.Fatalf("requested stop reported as failure for %q", streamID)
	case <-time.After(20 * time.Millisecond):
	}
}

func TestManagerRefreshesAvatarCacheOnlyWhileOutputContextIsActive(t *testing.T) {
	source := &refreshingFrameSource{staticFrameSource: staticFrameSource{frame: image.NewRGBA(image.Rect(0, 0, 4, 2))}, interval: 5 * time.Millisecond}
	manager := NewManager(source, Options{})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		manager.refreshAvatars(ctx)
		close(done)
	}()
	deadline := time.Now().Add(time.Second)
	for source.refreshes.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if source.refreshes.Load() == 0 {
		t.Fatal("avatar refresh did not run")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("avatar refresh did not stop with the output context")
	}
}

func validTestConfig() Config {
	return Config{
		StreamID:    "stream-01",
		IngestURL:   "srt://encoder.example:10080",
		Passphrase:  "job-scoped-passphrase-32-bytes-ok",
		PBKeylen:    32,
		Width:       4,
		Height:      2,
		FPS:         30,
		BitrateKbps: 1000,
	}
}

type staticFrameSource struct{ frame *image.RGBA }

func (s staticFrameSource) RenderScene(time.Time) (*image.RGBA, error) { return s.frame, nil }

type refreshingFrameSource struct {
	staticFrameSource
	interval  time.Duration
	refreshes atomic.Int32
}

func (s *refreshingFrameSource) AvatarRefreshInterval() time.Duration { return s.interval }
func (s *refreshingFrameSource) RefreshAvatars()                      { s.refreshes.Add(1) }

type staticDialer struct {
	conn SRTConn
	err  error
}

func (d staticDialer) Dial(context.Context, string, SRTOptions) (SRTConn, error) {
	return d.conn, d.err
}

type recordingSRTConn struct {
	mu     sync.Mutex
	data   bytes.Buffer
	closed bool
}

func (c *recordingSRTConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return 0, io.ErrClosedPipe
	}
	return c.data.Write(p)
}

func (c *recordingSRTConn) Close() error {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	return nil
}

func (c *recordingSRTConn) BytesWritten() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.data.Len()
}

func (c *recordingSRTConn) Closed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

type recordingProcessFactory struct {
	mu      sync.Mutex
	process Process
	args    []string
}

func (f *recordingProcessFactory) Start(_ string, args []string) (Process, error) {
	f.mu.Lock()
	f.args = append([]string(nil), args...)
	f.mu.Unlock()
	return f.process, nil
}

func (f *recordingProcessFactory) Args() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.args...)
}

type fakeProcess struct {
	stdin     io.WriteCloser
	stdout    io.ReadCloser
	stderr    io.ReadCloser
	wait      chan error
	kill      func() error
	completed atomic.Bool
}

func (p *fakeProcess) Stdin() io.WriteCloser { return p.stdin }
func (p *fakeProcess) Stdout() io.ReadCloser { return p.stdout }
func (p *fakeProcess) Stderr() io.ReadCloser { return p.stderr }
func (p *fakeProcess) Wait() error {
	err := <-p.wait
	p.completed.Store(true)
	return err
}
func (p *fakeProcess) Kill() error {
	err := p.kill()
	p.completed.Store(true)
	return err
}
func (p *fakeProcess) KilledOrExited() bool { return p.completed.Load() }

func newEchoProcess(output []byte) *fakeProcess {
	stdoutReader, stdoutWriter := io.Pipe()
	closed := make(chan struct{})
	var closeOnce sync.Once
	stdin := &callbackWriteCloser{
		write: func(p []byte) (int, error) {
			if _, err := stdoutWriter.Write(output); err != nil {
				return 0, err
			}
			return len(p), nil
		},
		close: func() error {
			closeOnce.Do(func() {
				close(closed)
				_ = stdoutWriter.Close()
			})
			return nil
		},
	}
	wait := make(chan error, 1)
	go func() {
		<-closed
		wait <- nil
	}()
	return &fakeProcess{
		stdin: stdin, stdout: stdoutReader, stderr: io.NopCloser(strings.NewReader("")), wait: wait,
		kill: stdin.Close,
	}
}

func newSilentProcess() *fakeProcess {
	stdoutReader, stdoutWriter := io.Pipe()
	closed := make(chan struct{})
	var closeOnce sync.Once
	stdin := &callbackWriteCloser{
		write: func(p []byte) (int, error) { return len(p), nil },
		close: func() error {
			closeOnce.Do(func() {
				close(closed)
				_ = stdoutWriter.Close()
			})
			return nil
		},
	}
	wait := make(chan error, 1)
	go func() {
		<-closed
		wait <- nil
	}()
	return &fakeProcess{
		stdin: stdin, stdout: stdoutReader, stderr: io.NopCloser(strings.NewReader("")), wait: wait,
		kill: stdin.Close,
	}
}

type callbackWriteCloser struct {
	write func([]byte) (int, error)
	close func() error
}

func (w *callbackWriteCloser) Write(p []byte) (int, error) { return w.write(p) }
func (w *callbackWriteCloser) Close() error                { return w.close() }
