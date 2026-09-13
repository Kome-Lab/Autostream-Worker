package httpapi

import (
	"encoding/json"
	"github.com/example/autostream-worker/internal/control"
	"github.com/example/autostream-worker/internal/encoder"
	"github.com/example/autostream-worker/internal/jobs"
	"github.com/example/autostream-worker/internal/observability"
	"github.com/example/autostream-worker/internal/version"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUpdaterVersionEndpointIsUnauthenticatedAndReturnsIdentityBoundProbe(t *testing.T) {
	previousVersion := version.Version
	version.Version = "v1.1.1"
	t.Setenv("SERVICE_VERSION", "v9.9.9")
	t.Setenv("SERVICE_ID", "wrong-fallback")
	t.Cleanup(func() { version.Version = previousVersion })

	configPath := filepath.Join(t.TempDir(), "config.yml")
	writeNodeConfigForVerifierTestWithIdentity(t, configPath, control.ServiceType, "worker-probe-01", 7)
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
	if got := res.Header.Get("Cache-Control"); got != "no-store" {
		t.Fatalf("expected no-store, got %q", got)
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

	methodRes, err := http.Post(server.URL+"/updater/version", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer methodRes.Body.Close()
	if methodRes.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("updater version POST status = %d", methodRes.StatusCode)
	}
	if got := methodRes.Header.Get("Cache-Control"); got != "no-store" {
		t.Fatalf("updater version POST cache control = %q", got)
	}
	if got := methodRes.Header.Get("Allow"); !strings.Contains(got, http.MethodGet) {
		t.Fatalf("updater version POST Allow = %q", got)
	}
}

func TestStatusEndpointUsesAuthoritativeNodeConfigServiceID(t *testing.T) {
	t.Setenv("SERVICE_ID", "")
	configPath := filepath.Join(t.TempDir(), "config.yml")
	writeNodeConfigForVerifierTestWithIdentity(t, configPath, control.ServiceType, "worker-status-01", 7)
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
