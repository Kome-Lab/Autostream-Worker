package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/example/autostream-worker/internal/control"
	"github.com/example/autostream-worker/internal/encoder"
	"github.com/example/autostream-worker/internal/ingesttoken"
	"github.com/example/autostream-worker/internal/jobs"
	"github.com/example/autostream-worker/internal/observability"
)

type capturePublisher struct {
	events []encoder.Event
}

func (p *capturePublisher) Publish(ctx context.Context, event encoder.Event) error {
	p.events = append(p.events, event)
	return nil
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
`
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
}
