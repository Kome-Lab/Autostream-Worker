package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/example/autostream-worker/internal/control"
	"github.com/example/autostream-worker/internal/deepgram"
	"github.com/example/autostream-worker/internal/encoder"
	"github.com/example/autostream-worker/internal/jobs"
	"github.com/example/autostream-worker/internal/observability"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

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
