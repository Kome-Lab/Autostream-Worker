package control

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestRegisterPostsServiceRegistration(t *testing.T) {
	var gotAuth string
	var got Registration
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/services/register" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		gotAuth = r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()

	client := Client{Config: Config{ControlPanelURL: server.URL, Token: "secret-token", ServiceID: "worker-01", ServiceName: "Worker 01", ServicePublicURL: "https://worker.example.com", Version: "0.1.0"}}
	if err := client.Register(t.Context()); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer secret-token" || got.ServiceType != ServiceType || got.Capabilities["overlay_events"] != true {
		t.Fatalf("unexpected registration: auth=%q body=%#v", gotAuth, got)
	}
	if got.OS != runtime.GOOS || got.Arch != runtime.GOARCH {
		t.Fatalf("registration did not include runtime platform: %#v", got)
	}
}

func TestHeartbeatPostsStatus(t *testing.T) {
	var got Heartbeat
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/services/heartbeat" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()

	client := Client{Config: Config{ControlPanelURL: server.URL, Token: "secret-token", ServiceID: "worker-01", ServiceName: "Worker 01", ServicePublicURL: "https://worker.example.com"}}
	if err := client.HeartbeatWithMetrics(t.Context(), "", "stream-01", map[string]float64{"worker.scene_updates_total": 2}); err != nil {
		t.Fatal(err)
	}
	if got.Status != "online" || got.CurrentStreamID != "stream-01" {
		t.Fatalf("unexpected heartbeat: %#v", got)
	}
	if got.OS != runtime.GOOS || got.Arch != runtime.GOARCH || got.Capabilities["job_endpoint"] != true {
		t.Fatalf("heartbeat did not include platform/capabilities: %#v", got)
	}
	if got.Metrics["worker.scene_updates_total"] != 2 {
		t.Fatalf("unexpected heartbeat metrics: %#v", got.Metrics)
	}
	if got.Metrics["node.cpu_count"] <= 0 || got.Metrics["process.heap_alloc_bytes"] <= 0 || got.Metrics["process.uptime_seconds"] < 0 {
		t.Fatalf("heartbeat did not include host/process metrics: %#v", got.Metrics)
	}
}

func TestReportSignalPostsViaControlPanel(t *testing.T) {
	var gotAuth string
	var got Signal
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/services/observability/signals" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		gotAuth = r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()

	client := Client{Config: Config{ControlPanelURL: server.URL, Token: "node-token", ServiceID: "worker-01", ServiceName: "Worker 01", ServicePublicURL: server.URL}}
	if err := client.Metric(t.Context(), "stream-01", "worker.event_send_failures_total", "warning", 1, nil); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer node-token" {
		t.Fatalf("unexpected auth header: %q", gotAuth)
	}
	if got.Type != "metric" || got.Name != "worker.event_send_failures_total" || got.Value == nil || *got.Value != 1 {
		t.Fatalf("unexpected signal: %#v", got)
	}
}

func TestRuntimeConfigFetchesScopedServiceConfig(t *testing.T) {
	var gotAuth string
	var gotQuery string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/services/runtime-config" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		gotAuth = r.Header.Get("Authorization")
		gotQuery = r.URL.Query().Get("service_id")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"service":{"service_id":"worker-01","service_type":"worker","service_name":"Worker 01","status":"assigned"},
			"assignments":[{"stream_id":"stream-01","service_id":"worker-01","service_type":"worker","assignment_role":"primary","assigned_at":"2026-06-10T00:00:00Z"}],
			"profiles":{"overlay":[{"id":"profile-01","kind":"overlay","name":"Default Overlay","config":{"service_id":"worker-01","show_clock":true},"created_at":"2026-06-10T00:00:00Z","updated_at":"2026-06-10T00:00:00Z"}]}
		}`))
	}))
	defer server.Close()

	client := Client{Config: Config{ControlPanelURL: server.URL, Token: "secret-token", ServiceID: "worker-01", ServiceName: "Worker 01", ServicePublicURL: "https://worker.example.com"}}
	cfg, err := client.RuntimeConfig(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer secret-token" || gotQuery != "worker-01" {
		t.Fatalf("unexpected request auth=%q query=%q", gotAuth, gotQuery)
	}
	if cfg.Service.ServiceID != "worker-01" || len(cfg.Assignments) != 1 {
		t.Fatalf("unexpected runtime config: %#v", cfg)
	}
	profiles := cfg.Profiles["overlay"]
	if len(profiles) != 1 || profiles[0].Config["show_clock"] != true {
		t.Fatalf("unexpected runtime profiles: %#v", profiles)
	}
}

func TestControlPanelErrorsDoNotLeakTokenOrBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "secret-token", http.StatusForbidden)
	}))
	defer server.Close()

	client := Client{Config: Config{ControlPanelURL: server.URL, Token: "secret-token", ServiceID: "worker-01", ServiceName: "Worker 01", ServicePublicURL: "https://worker.example.com"}}
	err := client.Register(t.Context())
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), "secret-token") {
		t.Fatalf("token leaked in error: %v", err)
	}
}

func TestControlPanelClientDoesNotFollowRedirectsWithBearerToken(t *testing.T) {
	var redirectedAuth string
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirectedAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/capture", http.StatusFound)
	}))
	defer server.Close()

	client := Client{Config: Config{ControlPanelURL: server.URL, Token: "secret-token", ServiceID: "worker-01", ServiceName: "Worker 01", ServicePublicURL: "https://worker.example.com"}}
	err := client.Register(t.Context())
	if err == nil {
		t.Fatal("expected redirect response to fail")
	}
	if redirectedAuth != "" {
		t.Fatalf("authorization header followed redirect: %q", redirectedAuth)
	}
}

func TestValidateRejectsNonHTTPControlPanelURL(t *testing.T) {
	cfg := Config{ControlPanelURL: "ftp://control.example.com/api", Token: "<SERVICE_TOKEN>", ServiceID: "worker-01", ServiceName: "Worker 01", ServicePublicURL: "https://worker.example.com"}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected validation error")
	}
	if !strings.Contains(err.Error(), "http or https") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateRejectsNonHTTPServicePublicURL(t *testing.T) {
	cfg := Config{ControlPanelURL: "https://control.example.com", Token: "<SERVICE_TOKEN>", ServiceID: "worker-01", ServiceName: "Worker 01", ServicePublicURL: "ftp://worker.example.com"}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected validation error")
	}
	if !strings.Contains(err.Error(), "SERVICE_PUBLIC_URL") || !strings.Contains(err.Error(), "http or https") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateRejectsRemoteHTTPControlPanelURL(t *testing.T) {
	cfg := Config{ControlPanelURL: "http://control.example.com", Token: "<SERVICE_TOKEN>", ServiceID: "worker-01", ServiceName: "Worker 01", ServicePublicURL: "https://worker.example.com"}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected validation error")
	}
	if !strings.Contains(err.Error(), "https for remote hosts") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateAllowsLocalHTTPControlPanelURL(t *testing.T) {
	cfg := Config{ControlPanelURL: "http://localhost:8080", Token: "<SERVICE_TOKEN>", ServiceID: "worker-01", ServiceName: "Worker 01", ServicePublicURL: "http://host.docker.internal:18083"}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected local http URLs to be allowed: %v", err)
	}
}

func TestValidateRejectsRemoteHTTPServicePublicURL(t *testing.T) {
	cfg := Config{ControlPanelURL: "https://control.example.com", Token: "<SERVICE_TOKEN>", ServiceID: "worker-01", ServiceName: "Worker 01", ServicePublicURL: "http://worker.example.com"}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected validation error")
	}
	if !strings.Contains(err.Error(), "SERVICE_PUBLIC_URL") || !strings.Contains(err.Error(), "https for remote hosts") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateRejectsControlPanelURLQueryOrFragment(t *testing.T) {
	cfg := Config{ControlPanelURL: "https://control.example.com?token=bad", Token: "<SERVICE_TOKEN>", ServiceID: "worker-01", ServiceName: "Worker 01", ServicePublicURL: "https://worker.example.com"}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected validation error")
	}
	if !strings.Contains(err.Error(), "query or fragment") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestConfigFromEnv(t *testing.T) {
	t.Setenv("CONTROL_PANEL_URL", "https://control.example.com")
	t.Setenv("CONTROL_PANEL_TOKEN", "<SERVICE_TOKEN>")
	t.Setenv("SERVICE_ID", "worker-01")
	t.Setenv("SERVICE_NAME", "Worker 01")
	t.Setenv("SERVICE_PUBLIC_URL", "https://worker.example.com")
	t.Setenv("SERVICE_VERSION", "0.1.0")
	t.Setenv("CONTROL_PANEL_HEARTBEAT_INTERVAL_SEC", "5")

	cfg := ConfigFromEnv()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if cfg.HeartbeatEvery != 5*time.Second || cfg.ServicePublicURL == "" {
		t.Fatalf("unexpected config: %#v", cfg)
	}
}
