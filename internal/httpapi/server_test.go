package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/example/autostream-worker/internal/control"
	"github.com/example/autostream-worker/internal/deepgram"
	"github.com/example/autostream-worker/internal/encoder"
	"github.com/example/autostream-worker/internal/ingesttoken"
	"github.com/example/autostream-worker/internal/jobs"
	"github.com/example/autostream-worker/internal/observability"
	"github.com/example/autostream-worker/internal/version"
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

func TestUpdaterVersionEndpointIsUnauthenticatedAndReturnsIdentityBoundProbe(t *testing.T) {
	previousVersion := version.Version
	version.Version = "v1.1.1"
	t.Setenv("SERVICE_VERSION", "v9.9.9")
	t.Setenv("SERVICE_ID", "wrong-fallback")
	t.Setenv("AUTOSTREAM_CONFIG_REVISION", "7")
	t.Cleanup(func() { version.Version = previousVersion })

	configPath := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(configPath, []byte(`panel:
  url: "https://panel.example.com"
node:
  id: "worker-probe-01"
  name: "Worker Probe"
  type: "worker"
api:
  host: "127.0.0.1"
  port: 8084
  ssl_enabled: false
auth:
  token: "runtime-token"
`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AUTOSTREAM_NODE_CONFIG", configPath)

	server := httptest.NewServer(NewServer(control.ServiceType, nil, TokenVerifier{PlainToken: "expected"}))
	defer server.Close()

	res, err := http.Get(server.URL + "/updater/version")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("expected unauthenticated updater version request to return 200, got %d", res.StatusCode)
	}
	if got := res.Header.Get("Content-Type"); got != "application/json" {
		t.Fatalf("expected application/json, got %q", got)
	}
	var body map[string]any
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if len(body) != 4 ||
		body["version"] != version.Current() ||
		body["service_id"] != "worker-probe-01" ||
		body["service_type"] != control.ServiceType ||
		body["config_revision"] != float64(7) {
		t.Fatalf("unexpected updater version response: %#v", body)
	}
}

func TestStatusEndpointUsesAuthoritativeNodeConfigServiceID(t *testing.T) {
	t.Setenv("SERVICE_ID", "")
	configPath := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(configPath, []byte(`panel:
  url: "https://panel.example.com"
node:
  id: "worker-status-01"
  name: "Worker Status"
  type: "worker"
api:
  host: "127.0.0.1"
  port: 8084
  ssl_enabled: false
auth:
  token: "runtime-token"
`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AUTOSTREAM_NODE_CONFIG", configPath)

	server := httptest.NewServer(NewServer(control.ServiceType, nil, TokenVerifier{}))
	defer server.Close()
	res, err := http.Get(server.URL + "/status")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status endpoint returned %d", res.StatusCode)
	}
	var body Status
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.ServiceID != "worker-status-01" {
		t.Fatalf("status service ID = %q, want authoritative node ID", body.ServiceID)
	}
}

func TestConfigRevisionFromEnvValidatesPositiveInteger(t *testing.T) {
	for _, tt := range []struct {
		name    string
		value   string
		want    int64
		wantErr bool
	}{
		{name: "default", value: "", want: 1},
		{name: "one", value: "1", want: 1},
		{name: "higher", value: "27", want: 27},
		{name: "zero", value: "0", wantErr: true},
		{name: "leading zero", value: "01", wantErr: true},
		{name: "negative", value: "-1", wantErr: true},
		{name: "fraction", value: "1.5", wantErr: true},
		{name: "padded", value: " 1 ", wantErr: true},
		{name: "text", value: "next", wantErr: true},
		{name: "overflow", value: "9223372036854775808", wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("AUTOSTREAM_CONFIG_REVISION", tt.value)
			got, err := ConfigRevisionFromEnv()
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ConfigRevisionFromEnv() accepted %q", tt.value)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("revision = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestStopJobKeepsStoppedTargetIdempotentWithoutStoppingSuccessor(t *testing.T) {
	manager := jobs.NewManager(encoder.NoopPublisher{}, observability.Client{})
	if err := manager.Start(t.Context(), jobs.StreamContext{StreamID: "stream-a"}); err != nil {
		t.Fatal(err)
	}
	if err := manager.Stop(t.Context(), "stream-a"); err != nil {
		t.Fatal(err)
	}
	handler := NewServer("worker", manager, TokenVerifier{PlainToken: "service-token"})
	stop := func(streamID string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/jobs/"+streamID+"/stop", nil)
		req.Header.Set("Authorization", "Bearer service-token")
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, req)
		return res
	}

	if res := stop("stream-a"); res.Code != http.StatusAccepted || !strings.Contains(res.Body.String(), "already_stopped") {
		t.Fatalf("stopped target before successor status=%d body=%s", res.Code, res.Body.String())
	}
	if res := stop("unknown-stream"); res.Code != http.StatusConflict || responseCode(t, res) != "no_active_stream_job" {
		t.Fatalf("unknown target without active stream status=%d body=%s", res.Code, res.Body.String())
	}
	if err := manager.Start(t.Context(), jobs.StreamContext{StreamID: "stream-b"}); err != nil {
		t.Fatal(err)
	}
	if res := stop("stream-a"); res.Code != http.StatusAccepted || !strings.Contains(res.Body.String(), "already_stopped") {
		t.Fatalf("stopped target after successor status=%d body=%s", res.Code, res.Body.String())
	}
	if got := manager.CurrentStreamID(); got != "stream-b" {
		t.Fatalf("delayed stop changed successor: current stream = %q", got)
	}
	if res := stop("unknown-stream"); res.Code != http.StatusConflict || responseCode(t, res) != "stream_id_mismatch" {
		t.Fatalf("unknown target with successor status=%d body=%s", res.Code, res.Body.String())
	}
	if got := manager.CurrentStreamID(); got != "stream-b" {
		t.Fatalf("unknown stop changed successor: current stream = %q", got)
	}
}

func TestStopJobKeepsStoppedTargetIdempotentAfterWorkerRestart(t *testing.T) {
	receiptPath := filepath.Join(t.TempDir(), "stopped-target-receipts.json")
	manager, err := jobs.NewManagerWithStoppedTargetReceiptFile(encoder.NoopPublisher{}, observability.Client{}, receiptPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Start(t.Context(), jobs.StreamContext{StreamID: "stream-a"}); err != nil {
		t.Fatal(err)
	}
	if err := manager.Stop(t.Context(), "stream-a"); err != nil {
		t.Fatal(err)
	}

	restartedManager, err := jobs.NewManagerWithStoppedTargetReceiptFile(encoder.NoopPublisher{}, observability.Client{}, receiptPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := restartedManager.Start(t.Context(), jobs.StreamContext{StreamID: "stream-b"}); err != nil {
		t.Fatal(err)
	}
	handler := NewServer("worker", restartedManager, TokenVerifier{PlainToken: "service-token"})
	stop := func(streamID string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/jobs/"+streamID+"/stop", nil)
		req.Header.Set("Authorization", "Bearer service-token")
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, req)
		return res
	}

	if res := stop("stream-a"); res.Code != http.StatusAccepted || !strings.Contains(res.Body.String(), "already_stopped") {
		t.Fatalf("stopped target after restart status=%d body=%s", res.Code, res.Body.String())
	}
	if got := restartedManager.CurrentStreamID(); got != "stream-b" {
		t.Fatalf("restart retry changed successor: current stream = %q", got)
	}
	if res := stop("unknown-stream"); res.Code != http.StatusConflict || responseCode(t, res) != "stream_id_mismatch" {
		t.Fatalf("unknown target after restart status=%d body=%s", res.Code, res.Body.String())
	}
}

func TestStopJobReturnsSafeReceiptPersistenceFailure(t *testing.T) {
	receiptPath := filepath.Join(t.TempDir(), "stopped-target-receipts.json")
	manager, err := jobs.NewManagerWithStoppedTargetReceiptFile(encoder.NoopPublisher{}, observability.Client{}, receiptPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Start(t.Context(), jobs.StreamContext{StreamID: "stream-a"}); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(receiptPath, 0o700); err != nil {
		t.Fatal(err)
	}

	handler := NewServer("worker", manager, TokenVerifier{PlainToken: "service-token"})
	req := httptest.NewRequest(http.MethodPost, "/jobs/stream-a/stop", nil)
	req.Header.Set("Authorization", "Bearer service-token")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)

	if res.Code != http.StatusServiceUnavailable || responseCode(t, res) != "stopped_target_receipt_unavailable" {
		t.Fatalf("receipt persistence failure status=%d body=%s", res.Code, res.Body.String())
	}
	if strings.Contains(res.Body.String(), "regular non-symlink") {
		t.Fatalf("receipt persistence detail leaked in response: %s", res.Body.String())
	}
	if got := manager.CurrentStreamID(); got != "stream-a" {
		t.Fatalf("receipt persistence failure changed current stream: %q", got)
	}
}

func responseCode(t *testing.T, res *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode JSON response: %v; body=%s", err, res.Body.String())
	}
	return body.Code
}

func TestNewServerFailsClosedOnInvalidConfigRevision(t *testing.T) {
	t.Setenv("AUTOSTREAM_CONFIG_REVISION", "0")
	defer func() {
		if recover() == nil {
			t.Fatal("NewServer must reject an invalid AUTOSTREAM_CONFIG_REVISION")
		}
	}()
	_ = NewServer(control.ServiceType, nil, TokenVerifier{})
}

func TestProtectedEndpointsRejectMissingToken(t *testing.T) {
	server := httptest.NewServer(NewServer("worker", jobs.NewManager(encoder.NoopPublisher{}, observability.Client{}), TokenVerifier{PlainToken: "expected"}))
	defer server.Close()

	res, err := http.Post(server.URL+"/jobs/start", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", res.StatusCode)
	}
}

func TestStartAndGenerateCurrentTimeEvent(t *testing.T) {
	server := httptest.NewServer(NewServer("worker", jobs.NewManager(encoder.NoopPublisher{}, observability.Client{}), TokenVerifier{PlainToken: "expected"}))
	defer server.Close()

	post := func(path, body string) *http.Response {
		req, err := http.NewRequest(http.MethodPost, server.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer expected")
		req.Header.Set("Content-Type", "application/json")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}

	res := post("/jobs/start", `{"stream_id":"stream-01","stream_name":"Test"}`)
	defer res.Body.Close()
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("expected 202, got %d", res.StatusCode)
	}
	res = post("/streams/stream-01/events/current-time", `{}`)
	defer res.Body.Close()
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("expected event 202, got %d", res.StatusCode)
	}
}

func TestDiscordChatOverlayEventIsAcceptedAndForwarded(t *testing.T) {
	publisher := &capturePublisher{}
	server := httptest.NewServer(NewServer("worker", jobs.NewManager(publisher, observability.Client{}), TokenVerifier{PlainToken: "expected"}))
	defer server.Close()

	post := func(path, body string) *http.Response {
		req, err := http.NewRequest(http.MethodPost, server.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer expected")
		req.Header.Set("Content-Type", "application/json")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}

	res := post("/jobs/start", `{"stream_id":"stream-01","stream_name":"Chat Test"}`)
	defer res.Body.Close()
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("expected start 202, got %d", res.StatusCode)
	}

	res = post("/streams/stream-01/events/overlay", `{"type":"overlay.discord_chat","payload":{"message_id":"msg-01","user_id":"user-01","display_name":"alice","text":"こんにちは","text_channel_id":"text-01","created_at":"2026-07-01T12:00:00Z"}}`)
	defer res.Body.Close()
	if res.StatusCode != http.StatusAccepted {
		var body bytes.Buffer
		_, _ = body.ReadFrom(res.Body)
		t.Fatalf("expected discord chat overlay 202, got %d body=%s", res.StatusCode, body.String())
	}
	var response encoder.Event
	if err := json.NewDecoder(res.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if response.Type != "overlay.discord_chat" || response.Payload["text_channel_id"] != "text-01" {
		t.Fatalf("unexpected response event: %#v", response)
	}
	if len(publisher.events) != 1 {
		t.Fatalf("expected one forwarded event, got %#v", publisher.events)
	}
	forwarded := publisher.events[0]
	if forwarded.Type != "overlay.discord_chat" || forwarded.StreamID != "stream-01" || forwarded.Payload["message_id"] != "msg-01" || forwarded.Payload["user_id"] != "user-01" || forwarded.Payload["display_name"] != "alice" || forwarded.Payload["text"] != "こんにちは" || forwarded.Payload["text_channel_id"] != "text-01" || forwarded.Payload["created_at"] != "2026-07-01T12:00:00Z" {
		t.Fatalf("discord chat overlay was not forwarded intact: %#v", forwarded)
	}
}

func TestWorkerEventEndpointAcceptsSignedDiscordBotToken(t *testing.T) {
	const signingKey = "test-signing-key"
	publisher := &capturePublisher{}
	manager := jobs.NewManager(publisher, observability.Client{})
	server := httptest.NewServer(NewServer("worker", manager, TokenVerifier{PlainToken: "expected", IngestTokenSigningKey: signingKey}))
	defer server.Close()

	startReq, err := http.NewRequest(http.MethodPost, server.URL+"/jobs/start", strings.NewReader(`{"stream_id":"stream-01","stream_name":"Chat Test"}`))
	if err != nil {
		t.Fatal(err)
	}
	startReq.Header.Set("Authorization", "Bearer expected")
	startReq.Header.Set("Content-Type", "application/json")
	startRes, err := http.DefaultClient.Do(startReq)
	if err != nil {
		t.Fatal(err)
	}
	defer startRes.Body.Close()
	if startRes.StatusCode != http.StatusAccepted {
		t.Fatalf("expected start 202, got %d", startRes.StatusCode)
	}

	token, err := ingesttoken.Issue(signingKey, ingesttoken.Claims{
		StreamID:    "stream-01",
		ServiceID:   "discord-01",
		ServiceType: "discord_bot",
		Purpose:     "worker_events",
		Audience:    "worker",
		ExpiresAt:   time.Now().Add(time.Hour).Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}
	eventReq, err := http.NewRequest(http.MethodPost, server.URL+"/streams/stream-01/events/overlay", strings.NewReader(`{"type":"overlay.discord_chat","payload":{"message_id":"msg-01","user_id":"user-01","text":"hello"}}`))
	if err != nil {
		t.Fatal(err)
	}
	eventReq.Header.Set("Authorization", "Bearer "+token)
	eventReq.Header.Set("Content-Type", "application/json")
	eventRes, err := http.DefaultClient.Do(eventReq)
	if err != nil {
		t.Fatal(err)
	}
	defer eventRes.Body.Close()
	if eventRes.StatusCode != http.StatusAccepted {
		var body bytes.Buffer
		_, _ = body.ReadFrom(eventRes.Body)
		t.Fatalf("expected signed worker event 202, got %d body=%s", eventRes.StatusCode, body.String())
	}
	if len(publisher.events) != 1 || publisher.events[0].Type != "overlay.discord_chat" {
		t.Fatalf("signed worker event was not published: %#v", publisher.events)
	}

	wrongStreamToken, err := ingesttoken.Issue(signingKey, ingesttoken.Claims{
		StreamID:    "stream-02",
		ServiceID:   "discord-01",
		ServiceType: "discord_bot",
		Purpose:     "worker_events",
		Audience:    "worker",
		ExpiresAt:   time.Now().Add(time.Hour).Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}
	rejectReq, err := http.NewRequest(http.MethodPost, server.URL+"/streams/stream-01/events/overlay", strings.NewReader(`{"type":"overlay.discord_chat","payload":{"message_id":"msg-02"}}`))
	if err != nil {
		t.Fatal(err)
	}
	rejectReq.Header.Set("Authorization", "Bearer "+wrongStreamToken)
	rejectReq.Header.Set("Content-Type", "application/json")
	rejectRes, err := http.DefaultClient.Do(rejectReq)
	if err != nil {
		t.Fatal(err)
	}
	defer rejectRes.Body.Close()
	if rejectRes.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected wrong stream token 401, got %d", rejectRes.StatusCode)
	}
}

func TestWorkerEventEndpointRejectsOldJobGenerationAfterRearm(t *testing.T) {
	publisher := &capturePublisher{}
	manager := jobs.NewManager(publisher, observability.Client{})
	if err := manager.Start(t.Context(), jobs.StreamContext{StreamID: "stream-01"}); err != nil {
		t.Fatal(err)
	}
	oldGeneration := manager.Status().JobGeneration
	if err := manager.Stop(t.Context(), "stream-01"); err != nil {
		t.Fatal(err)
	}
	if err := manager.Start(t.Context(), jobs.StreamContext{StreamID: "stream-01"}); err != nil {
		t.Fatal(err)
	}
	newGeneration := manager.Status().JobGeneration
	if newGeneration == oldGeneration {
		t.Fatalf("rearm did not advance job generation: old=%d new=%d", oldGeneration, newGeneration)
	}
	handler := NewServer("worker", manager, TokenVerifier{PlainToken: "service-token"})
	request := httptest.NewRequest(http.MethodPost, "/streams/stream-01/events/participants", strings.NewReader(`{"job_generation":`+strconv.FormatUint(oldGeneration, 10)+`,"participants":[]}`))
	request.Header.Set("Authorization", "Bearer service-token")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusConflict || responseCode(t, response) != "job_generation_mismatch" {
		t.Fatalf("old generation was not rejected: status=%d body=%s", response.Code, response.Body.String())
	}
	if len(publisher.events) != 0 {
		t.Fatalf("old generation reached Encoder publisher: %#v", publisher.events)
	}
}

func TestCaptionAudioVerifierRequiresStrictSignedClaims(t *testing.T) {
	const signingKey = "caption-signing-key"
	verifier := TokenVerifier{PlainToken: "service-token", IngestTokenSigningKey: signingKey}
	issue := func(claims ingesttoken.Claims) string {
		t.Helper()
		claims.ExpiresAt = time.Now().Add(time.Hour).Unix()
		token, err := ingesttoken.Issue(signingKey, claims)
		if err != nil {
			t.Fatal(err)
		}
		return "Bearer " + token
	}
	base := ingesttoken.Claims{StreamID: "stream-01", ServiceID: "discord-01", ServiceType: "discord_bot", Purpose: "caption_audio", Audience: "worker"}
	wrongStream := base
	wrongStream.StreamID = "stream-02"
	wrongServiceType := base
	wrongServiceType.ServiceType = "worker"
	wrongPurpose := base
	wrongPurpose.Purpose = "worker_events"
	wrongAudience := base
	wrongAudience.Audience = "encoder_recorder"

	tests := []struct {
		name   string
		header string
		want   bool
	}{
		{name: "signed caption token", header: issue(base), want: true},
		{name: "generic service token", header: "Bearer service-token"},
		{name: "wrong stream", header: issue(wrongStream)},
		{name: "wrong service type", header: issue(wrongServiceType)},
		{name: "wrong purpose", header: issue(wrongPurpose)},
		{name: "wrong audience", header: issue(wrongAudience)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := verifier.VerifyCaptionAudio(tc.header, "stream-01"); got != tc.want {
				t.Fatalf("VerifyCaptionAudio() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCaptionAudioEndpointAcceptsContractPayloadWithSignedToken(t *testing.T) {
	const signingKey = "caption-signing-key"
	manager, captionSession := captionReadyManager(t)
	server := httptest.NewServer(NewServer("worker", manager, TokenVerifier{IngestTokenSigningKey: signingKey}))
	defer server.Close()
	token, err := ingesttoken.Issue(signingKey, ingesttoken.Claims{
		StreamID:    "stream-01",
		ServiceID:   "discord-01",
		ServiceType: "discord_bot",
		Purpose:     "caption_audio",
		Audience:    "worker",
		ExpiresAt:   time.Now().Add(time.Hour).Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}
	opus := []byte{0xf8, 0xff, 0xfe}
	body := `{"stream_id":"stream-01","source":"discord","packets":[{"ssrc":42,"user_id":"user-42","job_generation":` + strconv.FormatUint(manager.Status().JobGeneration, 10) + `,"connection_generation":1,"sequence":7,"timestamp":960,"received_at":"2026-07-14T00:00:00Z","opus_base64":"` + base64.StdEncoding.EncodeToString(opus) + `"}]}`
	res := postJSON(t, server.URL+"/streams/stream-01/audio/opus", "Bearer "+token, body)
	defer res.Body.Close()
	if res.StatusCode != http.StatusAccepted {
		var response bytes.Buffer
		_, _ = response.ReadFrom(res.Body)
		t.Fatalf("expected caption audio 202, got %d body=%s", res.StatusCode, response.String())
	}
	select {
	case <-captionSession.ingested:
	case <-time.After(time.Second):
		t.Fatal("caption packet was not forwarded")
	}
	packets := captionSession.snapshotPackets()
	if len(packets) != 1 {
		t.Fatalf("packet was not forwarded exactly once: %#v", packets)
	}
	packet := packets[0]
	if packet.SSRC != 42 || packet.UserID != "user-42" || packet.Sequence != 7 || packet.Timestamp != 960 || !packet.ReceivedAt.Equal(time.Date(2026, 7, 14, 0, 0, 0, 0, time.UTC)) || !bytes.Equal(packet.Opus, opus) {
		t.Fatalf("unexpected forwarded packet: %#v", packet)
	}
}

func TestCaptionAudioEndpointRequiresNonzeroJobGeneration(t *testing.T) {
	const signingKey = "caption-generation-signing-key"
	manager, _ := captionReadyManager(t)
	server := httptest.NewServer(NewServer("worker", manager, TokenVerifier{IngestTokenSigningKey: signingKey}))
	defer server.Close()
	body := `{"stream_id":"stream-01","source":"discord","packets":[{"ssrc":42,"user_id":"user-42","sequence":7,"timestamp":960,"received_at":"2026-07-14T00:00:00Z","opus_base64":"AQ=="}]}`
	res := postJSON(t, server.URL+"/streams/stream-01/audio/opus", signedCaptionAudioAuthorization(t, signingKey, "stream-01"), body)
	defer res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing generation status = %d, want 400", res.StatusCode)
	}
	var response map[string]string
	if err := json.NewDecoder(res.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if response["code"] != "job_generation_required" {
		t.Fatalf("unexpected missing generation response: %#v", response)
	}
}

func TestCaptionAudioEndpointRequiresNonzeroConnectionGeneration(t *testing.T) {
	const signingKey = "caption-connection-generation-signing-key"
	manager, _ := captionReadyManager(t)
	server := httptest.NewServer(NewServer("worker", manager, TokenVerifier{IngestTokenSigningKey: signingKey}))
	defer server.Close()
	body := `{"stream_id":"stream-01","source":"discord","packets":[{"ssrc":42,"user_id":"user-42","job_generation":1,"sequence":7,"timestamp":960,"received_at":"2026-07-14T00:00:00Z","opus_base64":"AQ=="}]}`
	res := postJSON(t, server.URL+"/streams/stream-01/audio/opus", signedCaptionAudioAuthorization(t, signingKey, "stream-01"), body)
	defer res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing connection generation status = %d, want 400", res.StatusCode)
	}
	var response map[string]string
	if err := json.NewDecoder(res.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if response["code"] != "connection_generation_required" {
		t.Fatalf("unexpected missing connection generation response: %#v", response)
	}
}

func TestCaptionAudioEndpointRejectsOlderConnectionGeneration(t *testing.T) {
	const signingKey = "caption-stale-connection-generation-signing-key"
	manager, _ := captionReadyManager(t)
	server := httptest.NewServer(NewServer("worker", manager, TokenVerifier{IngestTokenSigningKey: signingKey}))
	defer server.Close()
	authorization := signedCaptionAudioAuthorization(t, signingKey, "stream-01")
	newerBody := strings.Replace(validCaptionAudioBody(), `"connection_generation":1`, `"connection_generation":2`, 1)
	accepted := postJSON(t, server.URL+"/streams/stream-01/audio/opus", authorization, newerBody)
	accepted.Body.Close()
	if accepted.StatusCode != http.StatusAccepted {
		t.Fatalf("newer connection generation status = %d, want 202", accepted.StatusCode)
	}
	stale := postJSON(t, server.URL+"/streams/stream-01/audio/opus", authorization, validCaptionAudioBody())
	defer stale.Body.Close()
	if stale.StatusCode != http.StatusConflict {
		t.Fatalf("stale connection generation status = %d, want 409", stale.StatusCode)
	}
	var response map[string]string
	if err := json.NewDecoder(stale.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if response["code"] != "connection_generation_stale" {
		t.Fatalf("unexpected stale connection generation response: %#v", response)
	}
}

func TestCaptionAudioEndpointAcceptsBeforeProviderIngestCompletes(t *testing.T) {
	const signingKey = "caption-async-signing-key"
	session := &blockingCaptionSession{started: make(chan struct{}), release: make(chan struct{})}
	manager := jobs.NewManager(encoder.NoopPublisher{}, observability.Client{})
	manager.SetCaptionRuntime(jobs.RuntimeSecretResolverFunc(func(_ context.Context, _, secretName string) (control.RuntimeSecret, error) {
		return control.RuntimeSecret{SecretName: secretName, Value: "dg-runtime-key", ExpiresInSec: 300}, nil
	}), jobs.CaptionSessionFactoryFunc(func(deepgram.Config, []byte, deepgram.Handler) (jobs.CaptionSession, error) {
		return session, nil
	}))
	manager.ApplyRuntimeConfig(captionRuntimeConfigForHTTP())
	if err := manager.Start(t.Context(), jobs.StreamContext{StreamID: "stream-01", CaptionProfileID: "caption-01"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		select {
		case <-session.release:
		default:
			close(session.release)
		}
		_ = manager.Close(context.Background())
	})

	server := httptest.NewServer(NewServer("worker", manager, TokenVerifier{IngestTokenSigningKey: signingKey}))
	defer func() {
		select {
		case <-session.release:
		default:
			close(session.release)
		}
		server.Close()
	}()
	req, err := http.NewRequest(http.MethodPost, server.URL+"/streams/stream-01/audio/opus", strings.NewReader(validCaptionAudioBody()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", signedCaptionAudioAuthorization(t, signingKey, "stream-01"))
	req.Header.Set("Content-Type", "application/json")
	type responseResult struct {
		response *http.Response
		err      error
	}
	result := make(chan responseResult, 1)
	go func() {
		response, requestErr := http.DefaultClient.Do(req)
		result <- responseResult{response: response, err: requestErr}
	}()

	select {
	case <-session.started:
	case <-time.After(time.Second):
		t.Fatal("caption provider ingest did not start")
	}
	select {
	case got := <-result:
		if got.err != nil {
			t.Fatal(got.err)
		}
		defer got.response.Body.Close()
		if got.response.StatusCode != http.StatusAccepted {
			t.Fatalf("expected asynchronous caption acceptance 202, got %d", got.response.StatusCode)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("caption HTTP response waited for the provider ingest")
	}
}

func TestCaptionAudioEndpointRejectsUnknownFieldsAndOversizedBodies(t *testing.T) {
	const signingKey = "caption-validation-signing-key"
	server := httptest.NewServer(NewServer("worker", jobs.NewManager(encoder.NoopPublisher{}, observability.Client{}), TokenVerifier{IngestTokenSigningKey: signingKey}))
	defer server.Close()
	authorization := signedCaptionAudioAuthorization(t, signingKey, "stream-01")
	validPacket := `{"ssrc":42,"sequence":7,"timestamp":960,"received_at":"2026-07-14T00:00:00Z","opus_base64":"AQ=="}`
	tests := []struct {
		name string
		body string
	}{
		{name: "top-level unknown field", body: `{"stream_id":"stream-01","source":"discord","packets":[` + validPacket + `],"unknown":true}`},
		{name: "packet unknown field", body: `{"stream_id":"stream-01","source":"discord","packets":[{"ssrc":42,"sequence":7,"timestamp":960,"received_at":"2026-07-14T00:00:00Z","opus_base64":"AQ==","unknown":true}]}`},
		{name: "multiple json values", body: `{"stream_id":"stream-01","source":"discord","packets":[` + validPacket + `]} {}`},
		{name: "missing required packet field", body: `{"stream_id":"stream-01","source":"discord","packets":[{"ssrc":42,"timestamp":960,"received_at":"2026-07-14T00:00:00Z","opus_base64":"AQ=="}]}`},
		{name: "invalid base64", body: `{"stream_id":"stream-01","source":"discord","packets":[{"ssrc":42,"sequence":7,"timestamp":960,"received_at":"2026-07-14T00:00:00Z","opus_base64":"not-base64"}]}`},
		{name: "stream mismatch", body: `{"stream_id":"stream-02","source":"discord","packets":[` + validPacket + `]}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res := postJSON(t, server.URL+"/streams/stream-01/audio/opus", authorization, tc.body)
			defer res.Body.Close()
			if res.StatusCode != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d", res.StatusCode)
			}
		})
	}

	oversized := `{"stream_id":"stream-01","source":"discord","packets":[{"ssrc":42,"sequence":7,"timestamp":960,"received_at":"2026-07-14T00:00:00Z","opus_base64":"` + strings.Repeat("A", int(maxCaptionAudioBodyBytes)) + `"}]}`
	res := postJSON(t, server.URL+"/streams/stream-01/audio/opus", authorization, oversized)
	defer res.Body.Close()
	if res.StatusCode != http.StatusRequestEntityTooLarge {
		var response bytes.Buffer
		_, _ = response.ReadFrom(res.Body)
		t.Fatalf("expected 413, got %d body=%s", res.StatusCode, response.String())
	}
}

func TestCaptionAudioEndpointReturnsConflictWhenCaptionProfileIsNotSelected(t *testing.T) {
	const signingKey = "caption-disabled-signing-key"
	manager := jobs.NewManager(encoder.NoopPublisher{}, observability.Client{})
	if err := manager.Start(t.Context(), jobs.StreamContext{StreamID: "stream-01"}); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(NewServer("worker", manager, TokenVerifier{IngestTokenSigningKey: signingKey}))
	defer server.Close()

	res := postJSON(t, server.URL+"/streams/stream-01/audio/opus", signedCaptionAudioAuthorization(t, signingKey, "stream-01"), validCaptionAudioBody())
	defer res.Body.Close()
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("expected disabled caption 409, got %d", res.StatusCode)
	}
	var body map[string]string
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body["code"] != "caption_audio_disabled" {
		t.Fatalf("unexpected response: %#v", body)
	}
}

func TestCaptionAudioEndpointRejectsGenericServiceToken(t *testing.T) {
	server := httptest.NewServer(NewServer("worker", jobs.NewManager(encoder.NoopPublisher{}, observability.Client{}), TokenVerifier{PlainToken: "service-token", IngestTokenSigningKey: "caption-signing-key"}))
	defer server.Close()

	res := postJSON(t, server.URL+"/streams/stream-01/audio/opus", "Bearer service-token", validCaptionAudioBody())
	defer res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("generic service token must not authorize caption audio, got %d", res.StatusCode)
	}
}

func TestJobStartFailsClosedWhenCaptionSecretResolutionFails(t *testing.T) {
	manager := jobs.NewManager(encoder.NoopPublisher{}, observability.Client{})
	resolveCalls := 0
	manager.SetCaptionRuntime(jobs.RuntimeSecretResolverFunc(func(context.Context, string, string) (control.RuntimeSecret, error) {
		resolveCalls++
		return control.RuntimeSecret{}, errors.New("resolve failed with dg-secret-must-not-leak")
	}), nil)
	runtimeConfig := captionRuntimeConfigForHTTP()
	server := httptest.NewServer(NewServerWithRuntimeConfig("worker", manager, TokenVerifier{PlainToken: "service-token"}, func(context.Context) (control.RuntimeConfig, error) {
		return runtimeConfig, nil
	}))
	defer server.Close()

	res := postJSON(t, server.URL+"/jobs/start", "Bearer service-token", `{"stream_id":"stream-01","caption_profile_id":"caption-01"}`)
	defer res.Body.Close()
	var response bytes.Buffer
	_, _ = response.ReadFrom(res.Body)
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected caption initialization 503, got %d body=%s", res.StatusCode, response.String())
	}
	if strings.Contains(response.String(), "dg-secret-must-not-leak") {
		t.Fatalf("runtime secret leaked in response: %s", response.String())
	}
	if manager.CurrentStreamID() != "" || resolveCalls != 1 {
		t.Fatalf("job was partially started: status=%#v resolve_calls=%d", manager.Status(), resolveCalls)
	}
}

func TestCaptionRuntimeSettingsRefreshesConfigAndReplacesRunningSession(t *testing.T) {
	manager := jobs.NewManager(encoder.NoopPublisher{}, observability.Client{})
	resolver := jobs.RuntimeSecretResolverFunc(func(_ context.Context, _, secretName string) (control.RuntimeSecret, error) {
		return control.RuntimeSecret{SecretName: secretName, Value: "runtime-key", ExpiresInSec: 300}, nil
	})
	var sessions []*captureCaptionSession
	var languages []string
	manager.SetCaptionRuntime(resolver, jobs.CaptionSessionFactoryFunc(func(config deepgram.Config, _ []byte, _ deepgram.Handler) (jobs.CaptionSession, error) {
		session := &captureCaptionSession{ingested: make(chan struct{}, 1)}
		sessions = append(sessions, session)
		languages = append(languages, config.Language)
		return session, nil
	}))
	initialConfig := captionRuntimeConfigForHTTP()
	manager.ApplyRuntimeConfig(initialConfig)
	if err := manager.Start(t.Context(), jobs.StreamContext{StreamID: "stream-01", CaptionProfileID: "caption-01"}); err != nil {
		t.Fatal(err)
	}
	updatedConfig := captionRuntimeConfigForHTTP()
	updatedConfig.Profiles["caption"][0].Config["language"] = "en"
	providerCalls := 0
	server := httptest.NewServer(NewServerWithRuntimeConfig("worker", manager, TokenVerifier{PlainToken: "service-token"}, func(context.Context) (control.RuntimeConfig, error) {
		providerCalls++
		return updatedConfig, nil
	}))
	defer server.Close()

	req, err := http.NewRequest(http.MethodPut, server.URL+"/jobs/stream-01/caption-runtime-settings", strings.NewReader(`{"caption_profile_id":"caption-01"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer service-token")
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("caption runtime update status = %d", res.StatusCode)
	}
	if providerCalls != 1 || len(sessions) != 2 || len(languages) != 2 || languages[0] != "ja" || languages[1] != "en" {
		t.Fatalf("runtime refresh did not replace the configured session: provider=%d sessions=%d languages=%#v", providerCalls, len(sessions), languages)
	}
	if sessions[0].snapshotClosed() != 1 || sessions[1].snapshotClosed() != 0 {
		t.Fatalf("caption session close state = old:%d new:%d", sessions[0].snapshotClosed(), sessions[1].snapshotClosed())
	}
}

func TestCaptionRuntimeSettingsKeepsSessionWhenRuntimeConfigRefreshFails(t *testing.T) {
	manager, session := captionReadyManager(t)
	server := httptest.NewServer(NewServerWithRuntimeConfig("worker", manager, TokenVerifier{PlainToken: "service-token"}, func(context.Context) (control.RuntimeConfig, error) {
		return control.RuntimeConfig{}, errors.New("upstream detail with secret-token")
	}))
	defer server.Close()

	req, err := http.NewRequest(http.MethodPut, server.URL+"/jobs/stream-01/caption-runtime-settings", strings.NewReader(`{"caption_profile_id":"caption-01"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer service-token")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("caption runtime update status = %d", res.StatusCode)
	}
	var body map[string]string
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(body["message"], "secret-token") || strings.Contains(body["code"], "secret-token") {
		t.Fatal("runtime config failure leaked upstream details")
	}
	if session.snapshotClosed() != 0 || !manager.Status().CaptionSessionActive {
		t.Fatalf("running caption session was not preserved: closed=%d status=%#v", session.snapshotClosed(), manager.Status())
	}
}

func TestStartRejectsUnassignedStreamWhenRuntimePolicyIsEnforced(t *testing.T) {
	manager := jobs.NewManager(encoder.NoopPublisher{}, observability.Client{})
	manager.SetAssignmentPolicy(jobs.AssignmentPolicy{
		Enforce:        true,
		PrimaryStreams: map[string]bool{"stream-01": true},
	})
	server := httptest.NewServer(NewServer("worker", manager, TokenVerifier{PlainToken: "expected"}))
	defer server.Close()

	req, err := http.NewRequest(http.MethodPost, server.URL+"/jobs/start", strings.NewReader(`{"stream_id":"stream-02","stream_name":"Wrong"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer expected")
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("expected unassigned stream to be rejected with 409, got %d", res.StatusCode)
	}
}

func TestStartRefreshesRuntimeConfigBeforeAssignmentCheck(t *testing.T) {
	manager := jobs.NewManager(encoder.NoopPublisher{}, observability.Client{})
	manager.SetAssignmentPolicy(jobs.AssignmentPolicy{
		Enforce:        true,
		PrimaryStreams: map[string]bool{"stale-stream": true},
	})
	calls := 0
	provider := func(context.Context) (control.RuntimeConfig, error) {
		calls++
		return control.RuntimeConfig{
			Service: control.RegisteredService{ServiceID: "worker-01"},
			Assignments: []control.StreamServiceAssignment{
				{StreamID: "stream-02", ServiceID: "worker-01", ServiceType: control.ServiceType, AssignmentRole: "primary"},
			},
		}, nil
	}
	server := httptest.NewServer(NewServerWithRuntimeConfig("worker", manager, TokenVerifier{PlainToken: "expected"}, provider))
	defer server.Close()

	req, err := http.NewRequest(http.MethodPost, server.URL+"/jobs/start", strings.NewReader(`{"stream_id":"stream-02","stream_name":"Fresh"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer expected")
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("expected refreshed runtime config assignment to allow start, got %d", res.StatusCode)
	}
	if calls != 1 {
		t.Fatalf("expected one runtime config refresh, got %d", calls)
	}
	if got := manager.CurrentStreamID(); got != "stream-02" {
		t.Fatalf("expected stream-02 to start after runtime config refresh, got %q", got)
	}
}

func TestStartFailsClosedWhenRuntimeConfigRefreshFails(t *testing.T) {
	manager := jobs.NewManager(encoder.NoopPublisher{}, observability.Client{})
	manager.SetAssignmentPolicy(jobs.AssignmentPolicy{
		Enforce:        true,
		PrimaryStreams: map[string]bool{"stream-01": true},
	})
	provider := func(context.Context) (control.RuntimeConfig, error) {
		return control.RuntimeConfig{}, errors.New("upstream unavailable with token secret-token")
	}
	server := httptest.NewServer(NewServerWithRuntimeConfig("worker", manager, TokenVerifier{PlainToken: "expected"}, provider))
	defer server.Close()

	req, err := http.NewRequest(http.MethodPost, server.URL+"/jobs/start", strings.NewReader(`{"stream_id":"stream-01","stream_name":"Should not start"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer expected")
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(res.Body); err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected runtime config failure to return 503, got %d body=%s", res.StatusCode, buf.String())
	}
	if strings.Contains(buf.String(), "secret-token") {
		t.Fatalf("runtime config failure leaked provider error: %s", buf.String())
	}
	if got := manager.CurrentStreamID(); got != "" {
		t.Fatalf("runtime config failure must not start a stream, got %q", got)
	}
}

func TestSHA256TokenVerifier(t *testing.T) {
	sum := sha256.Sum256([]byte("expected"))
	verifier := TokenVerifier{SHA256Hex: hex.EncodeToString(sum[:])}
	if !verifier.Verify("Bearer expected") {
		t.Fatal("expected token to verify")
	}
	if verifier.Verify("Bearer wrong") {
		t.Fatal("wrong token verified")
	}
}

func TestTokenVerifierFromEnvRejectsControlPanelTokenFallbackInProduction(t *testing.T) {
	t.Setenv("CONTROL_PANEL_TOKEN", "control-panel-token")
	t.Setenv("AUTOSTREAM_ENV", "production")
	verifier := TokenVerifierFromEnv()
	if verifier.Verify("Bearer control-panel-token") {
		t.Fatal("CONTROL_PANEL_TOKEN must not authorize inbound Worker control requests in production")
	}
}

func TestTokenVerifierFromEnvRejectsControlPanelTokenFallbackWhenRuntimeConfigRequired(t *testing.T) {
	t.Setenv("CONTROL_PANEL_TOKEN", "control-panel-token")
	t.Setenv("AUTOSTREAM_REQUIRE_CONTROL_PANEL_RUNTIME_CONFIG", "true")
	verifier := TokenVerifierFromEnv()
	if verifier.Verify("Bearer control-panel-token") {
		t.Fatal("CONTROL_PANEL_TOKEN must not authorize inbound Worker control requests when runtime config is required")
	}
}

func TestTokenVerifierFromEnvAllowsControlPanelTokenFallbackOutsideProduction(t *testing.T) {
	t.Setenv("CONTROL_PANEL_TOKEN", "control-panel-token")
	verifier := TokenVerifierFromEnv()
	if !verifier.Verify("Bearer control-panel-token") {
		t.Fatal("expected local compatibility CONTROL_PANEL_TOKEN fallback outside production")
	}
}

func TestTokenVerifierReadsNodeRuntimeTokenAfterStartup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	t.Setenv("AUTOSTREAM_NODE_CONFIG", path)
	t.Setenv("CONTROL_PANEL_TOKEN", "")
	verifier := TokenVerifierFromEnv()
	if verifier.Verify("Bearer runtime-secret") {
		t.Fatal("runtime token should not verify before config exists")
	}
	writeNodeConfigForVerifierTest(t, path, "worker")
	if !verifier.Verify("Bearer runtime-secret") {
		t.Fatal("runtime token should verify after config is written")
	}
	signedToken, err := ingesttoken.Issue("node-config-signing-key", ingesttoken.Claims{StreamID: "stream-01", ServiceID: "discord-bot-01", ServiceType: "discord_bot", Purpose: "worker_events", Audience: "worker", ExpiresAt: time.Now().Add(time.Hour).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	if !verifier.VerifyWorkerEvents("Bearer "+signedToken, "stream-01") {
		t.Fatal("stream ingest signing key should be reloaded from node config")
	}
}

func TestErrorDoesNotEchoBearerToken(t *testing.T) {
	server := httptest.NewServer(NewServer("worker", jobs.NewManager(encoder.NoopPublisher{}, observability.Client{}), TokenVerifier{PlainToken: "secret-token"}))
	defer server.Close()

	req, err := http.NewRequest(http.MethodPost, server.URL+"/jobs/start", strings.NewReader(`{"stream_id":""}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer secret-token")
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(res.Body); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "secret-token") {
		t.Fatalf("token leaked in response: %s", buf.String())
	}
}

func captionReadyManager(t *testing.T) (*jobs.Manager, *captureCaptionSession) {
	t.Helper()
	manager := jobs.NewManager(encoder.NoopPublisher{}, observability.Client{})
	session := &captureCaptionSession{ingested: make(chan struct{}, 1)}
	manager.SetCaptionRuntime(jobs.RuntimeSecretResolverFunc(func(_ context.Context, _, secretName string) (control.RuntimeSecret, error) {
		return control.RuntimeSecret{SecretName: secretName, Value: "dg-runtime-key", ExpiresInSec: 300}, nil
	}), jobs.CaptionSessionFactoryFunc(func(deepgram.Config, []byte, deepgram.Handler) (jobs.CaptionSession, error) {
		return session, nil
	}))
	manager.ApplyRuntimeConfig(captionRuntimeConfigForHTTP())
	if err := manager.Start(t.Context(), jobs.StreamContext{StreamID: "stream-01", CaptionProfileID: "caption-01"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { manager.Close(context.Background()) })
	return manager, session
}

func captionRuntimeConfigForHTTP() control.RuntimeConfig {
	return control.RuntimeConfig{
		Service: control.RegisteredService{ServiceID: "worker-01", ServiceType: control.ServiceType},
		Assignments: []control.StreamServiceAssignment{{
			StreamID:       "stream-01",
			ServiceID:      "worker-01",
			ServiceType:    control.ServiceType,
			AssignmentRole: "primary",
		}},
		Profiles: map[string][]control.RuntimeProfile{
			"caption": {{
				ID:   "caption-01",
				Kind: "caption",
				Config: map[string]any{
					"service_id":          "worker-01",
					"provider":            "deepgram",
					"model":               "nova-3",
					"language":            "ja",
					"api_key_secret_name": "deepgram_api_key",
					"endpointing_ms":      300,
					"interim_results":     true,
					"smart_format":        true,
					"delay_ms":            800,
				},
			}},
		},
	}
}

func validCaptionAudioBody() string {
	return `{"stream_id":"stream-01","source":"discord","packets":[{"ssrc":42,"user_id":"user-42","job_generation":1,"connection_generation":1,"sequence":7,"timestamp":960,"received_at":"2026-07-14T00:00:00Z","opus_base64":"` + base64.StdEncoding.EncodeToString([]byte{1}) + `"}]}`
}

func signedCaptionAudioAuthorization(t *testing.T, signingKey, streamID string) string {
	t.Helper()
	token, err := ingesttoken.Issue(signingKey, ingesttoken.Claims{
		StreamID:    streamID,
		ServiceID:   "discord-01",
		ServiceType: "discord_bot",
		Purpose:     "caption_audio",
		Audience:    "worker",
		ExpiresAt:   time.Now().Add(time.Hour).Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return "Bearer " + token
}

func postJSON(t *testing.T, endpoint, authorization, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, endpoint, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", authorization)
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func writeNodeConfigForVerifierTest(t *testing.T, path, nodeType string) {
	t.Helper()
	body := `panel:
  url: "https://panel.example.jp"
node:
  id: "worker-01"
  name: "Worker 01"
  type: "` + nodeType + `"
api:
  host: "worker.example.jp"
  port: 8443
  ssl_enabled: true
auth:
  token_id: "token-id"
  token: "runtime-secret"
stream_ingest:
  signing_key: "node-config-signing-key"
`
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
}
