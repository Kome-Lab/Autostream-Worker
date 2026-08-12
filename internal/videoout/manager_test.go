package videoout

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/jpeg"
	"io"
	"sync"
	"testing"
	"time"
)

func TestManagerSendsJPEGSceneFramesDirectlyWithoutVideoEncoding(t *testing.T) {
	conn := &recordingSRTConn{}
	manager := NewManager(staticFrameSource{frame: image.NewRGBA(image.Rect(0, 0, 4, 2))}, Options{
		Dialer: staticDialer{conn: conn}, StartupTimeout: time.Second, FrameInterval: time.Hour,
	})
	if err := manager.Start(t.Context(), validTestConfig()); err != nil {
		t.Fatal(err)
	}
	data := conn.Data()
	if len(data) < 4 || data[0] != 0xff || data[1] != 0xd8 || data[len(data)-2] != 0xff || data[len(data)-1] != 0xd9 {
		t.Fatalf("SRT payload is not one complete JPEG frame: %x", data)
	}
	decoded, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode scene frame: %v", err)
	}
	if decoded.Bounds().Dx() != 4 || decoded.Bounds().Dy() != 2 {
		t.Fatalf("invalid scene frame bounds: %v", decoded.Bounds())
	}
	if status := manager.Status(); !status.Running || status.BytesSent != uint64(len(data)) {
		t.Fatalf("unexpected status: %#v", status)
	}
	if err := manager.Stop(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !conn.Closed() {
		t.Fatal("SRT connection was not closed")
	}
}

func TestManagerStartFailsIfFrameShapeDoesNotMatchContract(t *testing.T) {
	conn := &recordingSRTConn{}
	manager := NewManager(staticFrameSource{frame: image.NewRGBA(image.Rect(0, 0, 3, 2))}, Options{
		Dialer: staticDialer{conn: conn}, StartupTimeout: time.Second,
	})
	err := manager.Start(t.Context(), validTestConfig())
	if !errors.Is(err, ErrFrameShapeMismatch) {
		t.Fatalf("error = %v, want ErrFrameShapeMismatch", err)
	}
	if !conn.Closed() {
		t.Fatal("failed startup did not close SRT")
	}
}

func TestManagerRejectsConcurrentStart(t *testing.T) {
	manager := NewManager(staticFrameSource{frame: image.NewRGBA(image.Rect(0, 0, 4, 2))}, Options{
		Dialer: staticDialer{conn: &recordingSRTConn{}}, StartupTimeout: time.Second, FrameInterval: time.Hour,
	})
	if err := manager.Start(t.Context(), validTestConfig()); err != nil {
		t.Fatal(err)
	}
	defer manager.Stop(context.Background())
	if err := manager.Start(t.Context(), validTestConfig()); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("error = %v, want ErrAlreadyRunning", err)
	}
}

func TestManagerReportsUnexpectedSRTFailureOnce(t *testing.T) {
	conn := &recordingSRTConn{failAfterWrites: 1}
	failures := make(chan string, 2)
	manager := NewManager(staticFrameSource{frame: image.NewRGBA(image.Rect(0, 0, 4, 2))}, Options{
		Dialer: staticDialer{conn: conn}, StartupTimeout: time.Second, FrameInterval: time.Millisecond,
		OnFailure: func(streamID string, generation uint64) { failures <- streamID },
	})
	if err := manager.Start(t.Context(), validTestConfig()); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-failures:
		if got != "stream-01" {
			t.Fatalf("failure stream = %q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("unexpected SRT failure was not reported")
	}
	select {
	case got := <-failures:
		t.Fatalf("failure reported twice for %q", got)
	case <-time.After(20 * time.Millisecond):
	}
}

func TestManagerDoesNotReportRequestedStopAsFailure(t *testing.T) {
	failures := make(chan string, 1)
	manager := NewManager(staticFrameSource{frame: image.NewRGBA(image.Rect(0, 0, 4, 2))}, Options{
		Dialer: staticDialer{conn: &recordingSRTConn{}}, StartupTimeout: time.Second, FrameInterval: time.Hour,
		OnFailure: func(streamID string, _ uint64) { failures <- streamID },
	})
	if err := manager.Start(t.Context(), validTestConfig()); err != nil {
		t.Fatal(err)
	}
	if err := manager.Stop(t.Context()); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-failures:
		t.Fatalf("requested stop reported as failure for %q", got)
	case <-time.After(20 * time.Millisecond):
	}
}

func validTestConfig() Config {
	return Config{StreamID: "stream-01", Generation: 42, IngestURL: "srt://encoder.example:10080", Passphrase: "job-scoped-passphrase-32-bytes-ok", PBKeylen: 32, Width: 4, Height: 2}
}

type staticFrameSource struct{ frame *image.RGBA }

func (s staticFrameSource) RenderScene(time.Time) (*image.RGBA, error) { return s.frame, nil }

type staticDialer struct {
	conn SRTConn
	err  error
}

func (d staticDialer) Dial(context.Context, string, SRTOptions) (SRTConn, error) {
	return d.conn, d.err
}

type recordingSRTConn struct {
	mu              sync.Mutex
	data            bytes.Buffer
	closed          bool
	writes          int
	failAfterWrites int
}

func (c *recordingSRTConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || (c.failAfterWrites > 0 && c.writes >= c.failAfterWrites) {
		return 0, io.ErrClosedPipe
	}
	c.writes++
	return c.data.Write(p)
}

func (c *recordingSRTConn) Close() error {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	return nil
}

func (c *recordingSRTConn) Data() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]byte(nil), c.data.Bytes()...)
}

func (c *recordingSRTConn) Closed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}
