package deepgram

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

type fakeDialResult struct {
	socket *fakeSocket
	err    error
}

type fakeDialRecord struct {
	endpoint string
	header   http.Header
}

type fakeDialer struct {
	mu      sync.Mutex
	results []fakeDialResult
	records []fakeDialRecord
}

func (d *fakeDialer) Dial(_ context.Context, endpoint string, header http.Header) (socket, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.records = append(d.records, fakeDialRecord{endpoint: endpoint, header: header.Clone()})
	if len(d.results) == 0 {
		return nil, errors.New("no fake socket configured")
	}
	result := d.results[0]
	d.results = d.results[1:]
	return result.socket, result.err
}

func (d *fakeDialer) snapshot() []fakeDialRecord {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]fakeDialRecord(nil), d.records...)
}

type fakeRead struct {
	messageType websocket.MessageType
	payload     []byte
	err         error
}

type fakeWrite struct {
	messageType websocket.MessageType
	payload     []byte
}

type fakeSocket struct {
	reads chan fakeRead

	mu                    sync.Mutex
	writes                []fakeWrite
	failNextBinary        error
	readLimit             int64
	closeAfterCloseStream bool
	closed                chan struct{}
	closeOnce             sync.Once
}

func newFakeSocket() *fakeSocket {
	return &fakeSocket{reads: make(chan fakeRead, 8), closed: make(chan struct{})}
}

func (s *fakeSocket) Read(ctx context.Context) (websocket.MessageType, []byte, error) {
	select {
	case <-ctx.Done():
		return 0, nil, ctx.Err()
	case <-s.closed:
		return 0, nil, errors.New("fake socket closed")
	case result := <-s.reads:
		return result.messageType, append([]byte(nil), result.payload...), result.err
	}
}

func (s *fakeSocket) Write(_ context.Context, messageType websocket.MessageType, payload []byte) error {
	s.mu.Lock()
	if messageType == websocket.MessageBinary && s.failNextBinary != nil {
		err := s.failNextBinary
		s.failNextBinary = nil
		s.mu.Unlock()
		return err
	}
	s.writes = append(s.writes, fakeWrite{messageType: messageType, payload: append([]byte(nil), payload...)})
	closeAfterWrite := s.closeAfterCloseStream && messageType == websocket.MessageText && string(payload) == `{"type":"CloseStream"}`
	s.mu.Unlock()
	if closeAfterWrite {
		s.close()
	}
	return nil
}

func (s *fakeSocket) Close(websocket.StatusCode, string) error {
	s.close()
	return nil
}

func (s *fakeSocket) CloseNow() error {
	s.close()
	return nil
}

func (s *fakeSocket) SetReadLimit(limit int64) {
	s.mu.Lock()
	s.readLimit = limit
	s.mu.Unlock()
}

func (s *fakeSocket) close() {
	s.closeOnce.Do(func() { close(s.closed) })
}

func (s *fakeSocket) snapshotWrites() []fakeWrite {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]fakeWrite, len(s.writes))
	copy(out, s.writes)
	return out
}

func TestListenURLUsesRawDiscordOpusSettings(t *testing.T) {
	rawURL, err := ListenURL(testConfig(0))
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Scheme != "wss" || parsed.Host != "api.deepgram.com" || parsed.Path != "/v1/listen" {
		t.Fatalf("unexpected listen endpoint: %s", rawURL)
	}
	want := map[string]string{
		"encoding":        "opus",
		"sample_rate":     "48000",
		"channels":        "2",
		"model":           "nova-3",
		"language":        "ja",
		"interim_results": "true",
		"smart_format":    "true",
		"endpointing":     "300",
	}
	for key, value := range want {
		if parsed.Query().Get(key) != value {
			t.Fatalf("query %s = %q, want %q", key, parsed.Query().Get(key), value)
		}
	}
	if len(parsed.Query()) != len(want) {
		t.Fatalf("unexpected query values: %v", parsed.Query())
	}
}

func TestSessionLazyDialsAndSendsBinaryAudioAndTextKeepAlive(t *testing.T) {
	sock := newFakeSocket()
	sock.closeAfterCloseStream = true
	dialer := &fakeDialer{results: []fakeDialResult{{socket: sock}}}
	session, err := newSession(testConfig(0), []byte("dg-runtime-key"), func(context.Context, Transcript) error { return nil }, testOptions(dialer))
	if err != nil {
		t.Fatal(err)
	}
	if len(dialer.snapshot()) != 0 {
		t.Fatal("session dialed before the first audio packet")
	}
	if err := session.Ingest(t.Context(), AudioPacket{SSRC: 42}); !errors.Is(err, ErrEmptyAudio) {
		t.Fatalf("unexpected empty audio error: %v", err)
	}
	if len(dialer.snapshot()) != 0 {
		t.Fatal("empty audio packet triggered a dial")
	}

	opus := []byte{0xf8, 0xff, 0xfe}
	if err := session.Ingest(t.Context(), AudioPacket{SSRC: 42, UserID: "user-42", Opus: opus}); err != nil {
		t.Fatal(err)
	}
	records := dialer.snapshot()
	if len(records) != 1 {
		t.Fatalf("dial count = %d, want 1", len(records))
	}
	if records[0].header.Get("Authorization") != "Token dg-runtime-key" {
		t.Fatalf("unexpected authorization header: %q", records[0].header.Get("Authorization"))
	}
	if strings.Contains(records[0].endpoint, "dg-runtime-key") {
		t.Fatalf("api key leaked into URL: %s", records[0].endpoint)
	}

	writes := sock.snapshotWrites()
	if len(writes) == 0 || writes[0].messageType != websocket.MessageBinary || string(writes[0].payload) != string(opus) {
		t.Fatalf("first websocket message was not the opus packet: %#v", writes)
	}
	waitFor(t, time.Second, func() bool {
		for _, write := range sock.snapshotWrites() {
			if write.messageType == websocket.MessageText && string(write.payload) == `{"type":"KeepAlive"}` {
				return true
			}
		}
		return false
	})
	if err := session.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestSessionPublishesInterimAndFinalResultsWithSpeakerAndDelay(t *testing.T) {
	sock := newFakeSocket()
	sock.closeAfterCloseStream = true
	dialer := &fakeDialer{results: []fakeDialResult{{socket: sock}}}
	results := make(chan Transcript, 2)
	delay := 40 * time.Millisecond
	session, err := newSession(testConfig(delay), []byte("dg-runtime-key"), func(_ context.Context, transcript Transcript) error {
		results <- transcript
		return nil
	}, testOptions(dialer))
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close(t.Context())
	if err := session.Ingest(t.Context(), AudioPacket{SSRC: 42, UserID: "speaker-123", Opus: []byte{1}}); err != nil {
		t.Fatal(err)
	}

	startedAt := time.Now()
	sock.reads <- fakeRead{messageType: websocket.MessageText, payload: []byte(`{"type":"Results","is_final":false,"channel":{"alternatives":[{"transcript":"途中"}]}}`)}
	select {
	case result := <-results:
		t.Fatalf("result ignored delay: %#v", result)
	case <-time.After(15 * time.Millisecond):
	}
	interim := receiveTranscript(t, results)
	if interim.Text != "途中" || interim.Final || interim.SpeakerUserID != "speaker-123" {
		t.Fatalf("unexpected interim transcript: %#v", interim)
	}
	if elapsed := time.Since(startedAt); elapsed < 30*time.Millisecond {
		t.Fatalf("delay was not respected: %s", elapsed)
	}

	sock.reads <- fakeRead{messageType: websocket.MessageText, payload: []byte(`{"type":"Results","is_final":true,"channel":{"alternatives":[{"transcript":"確定"}]}}`)}
	final := receiveTranscript(t, results)
	if final.Text != "確定" || !final.Final || final.SpeakerUserID != "speaker-123" {
		t.Fatalf("unexpected final transcript: %#v", final)
	}
}

func TestSessionReconnectsAfterAudioWriteFailureWithoutLeakingDetails(t *testing.T) {
	first := newFakeSocket()
	first.failNextBinary = errors.New("write failed dg-runtime-key endpointing=300")
	second := newFakeSocket()
	second.closeAfterCloseStream = true
	dialer := &fakeDialer{results: []fakeDialResult{{socket: first}, {socket: second}}}
	session, err := newSession(testConfig(0), []byte("dg-runtime-key"), func(context.Context, Transcript) error { return nil }, testOptions(dialer))
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close(t.Context())

	err = session.Ingest(t.Context(), AudioPacket{SSRC: 42, Opus: []byte{1}})
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("unexpected write error: %v", err)
	}
	if strings.Contains(err.Error(), "dg-runtime-key") || strings.Contains(err.Error(), "endpointing") {
		t.Fatalf("write error leaked sensitive connection details: %v", err)
	}
	if err := session.Ingest(t.Context(), AudioPacket{SSRC: 42, Opus: []byte{2}}); err != nil {
		t.Fatalf("next packet did not reconnect: %v", err)
	}
	if len(dialer.snapshot()) != 2 {
		t.Fatalf("dial count = %d, want 2", len(dialer.snapshot()))
	}
}

func TestSessionReconnectsOnNextPacketAfterReadFailure(t *testing.T) {
	first := newFakeSocket()
	second := newFakeSocket()
	second.closeAfterCloseStream = true
	dialer := &fakeDialer{results: []fakeDialResult{{socket: first}, {socket: second}}}
	session, err := newSession(testConfig(0), []byte("dg-runtime-key"), func(context.Context, Transcript) error { return nil }, testOptions(dialer))
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close(t.Context())
	if err := session.Ingest(t.Context(), AudioPacket{SSRC: 42, Opus: []byte{1}}); err != nil {
		t.Fatal(err)
	}
	first.reads <- fakeRead{err: errors.New("read failed with endpointing=300 and dg-runtime-key")}
	select {
	case <-first.closed:
	case <-time.After(time.Second):
		t.Fatal("failed connection was not discarded")
	}
	if err := session.Ingest(t.Context(), AudioPacket{SSRC: 42, Opus: []byte{2}}); err != nil {
		t.Fatalf("next packet did not reconnect: %v", err)
	}
	if len(dialer.snapshot()) != 2 {
		t.Fatalf("dial count = %d, want 2", len(dialer.snapshot()))
	}
}

func TestSessionSeparatesReusedSSRCWhenSpeakerChanges(t *testing.T) {
	first := newFakeSocket()
	second := newFakeSocket()
	second.closeAfterCloseStream = true
	dialer := &fakeDialer{results: []fakeDialResult{{socket: first}, {socket: second}}}
	session, err := newSession(testConfig(0), []byte("dg-runtime-key"), func(context.Context, Transcript) error { return nil }, testOptions(dialer))
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close(t.Context())
	if err := session.Ingest(t.Context(), AudioPacket{SSRC: 42, UserID: "user-one", Opus: []byte{1}}); err != nil {
		t.Fatal(err)
	}
	if err := session.Ingest(t.Context(), AudioPacket{SSRC: 42, UserID: "user-two", Opus: []byte{2}}); err != nil {
		t.Fatal(err)
	}
	if len(dialer.snapshot()) != 2 {
		t.Fatalf("speaker change reused the previous Deepgram connection: dials=%d", len(dialer.snapshot()))
	}
	select {
	case <-first.closed:
	default:
		t.Fatal("previous speaker connection was not discarded")
	}
}

func TestSessionCloseSendsCloseStreamForEverySSRCAndZerosKey(t *testing.T) {
	first := newFakeSocket()
	first.closeAfterCloseStream = true
	second := newFakeSocket()
	second.closeAfterCloseStream = true
	dialer := &fakeDialer{results: []fakeDialResult{{socket: first}, {socket: second}}}
	session, err := newSession(testConfig(0), []byte("dg-runtime-key"), func(context.Context, Transcript) error { return nil }, testOptions(dialer))
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Ingest(t.Context(), AudioPacket{SSRC: 42, UserID: "user-42", Opus: []byte{1}}); err != nil {
		t.Fatal(err)
	}
	if err := session.Ingest(t.Context(), AudioPacket{SSRC: 84, UserID: "user-84", Opus: []byte{2}}); err != nil {
		t.Fatal(err)
	}
	keyMemory := session.apiKey
	if err := session.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	for index, sock := range []*fakeSocket{first, second} {
		var sawCloseStream bool
		for _, write := range sock.snapshotWrites() {
			if write.messageType == websocket.MessageText && string(write.payload) == `{"type":"CloseStream"}` {
				sawCloseStream = true
			}
		}
		if !sawCloseStream {
			t.Fatalf("socket %d did not receive CloseStream", index)
		}
		select {
		case <-sock.closed:
		default:
			t.Fatalf("socket %d was not closed", index)
		}
	}
	for _, value := range keyMemory {
		if value != 0 {
			t.Fatalf("api key memory was not zeroed: %v", keyMemory)
		}
	}
	if session.apiKey != nil {
		t.Fatalf("api key reference was retained: %v", session.apiKey)
	}
}

func TestParseResultIgnoresInterimWhenDisabled(t *testing.T) {
	_, ok := parseResult([]byte(`{"type":"Results","is_final":false,"channel":{"alternatives":[{"transcript":"interim"}]}}`), false)
	if ok {
		t.Fatal("interim result was accepted while disabled")
	}
	result, ok := parseResult([]byte(`{"type":"Results","is_final":true,"channel":{"alternatives":[{"transcript":"final"}]}}`), false)
	if !ok || !result.final || result.text != "final" {
		t.Fatalf("final result was not accepted: %#v", result)
	}
}

func testConfig(delay time.Duration) Config {
	return Config{
		Model:          "nova-3",
		Language:       "ja",
		EndpointingMS:  300,
		InterimResults: true,
		SmartFormat:    true,
		Delay:          delay,
	}
}

func testOptions(dialer socketDialer) sessionOptions {
	return sessionOptions{
		endpoint:          "ws://deepgram.test/v1/listen",
		dialer:            dialer,
		keepAliveInterval: 10 * time.Millisecond,
		dialTimeout:       time.Second,
		writeTimeout:      time.Second,
		closeTimeout:      50 * time.Millisecond,
		maxConnections:    10,
	}
}

func receiveTranscript(t *testing.T, results <-chan Transcript) Transcript {
	t.Helper()
	select {
	case result := <-results:
		return result
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for transcript")
		return Transcript{}
	}
}

func waitFor(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition was not satisfied before timeout")
}
