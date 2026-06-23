package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/example/autostream-worker/internal/control"
	"github.com/example/autostream-worker/internal/encoder"
	"github.com/example/autostream-worker/internal/jobs"
	"github.com/example/autostream-worker/internal/observability"
)

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
