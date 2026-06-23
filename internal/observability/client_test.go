package observability

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestReportPostsSignal(t *testing.T) {
	var gotAuth string
	var got Signal
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/signals" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		gotAuth = r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()

	client := Client{Config: Config{URL: server.URL, Token: "secret-token", ServiceID: "worker-01", Timeout: time.Second}}
	if err := client.Event(t.Context(), "stream-01", "worker.event.sent", "sent", map[string]any{"type": "overlay.current_time"}); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer secret-token" || got.ServiceType != serviceType || got.StreamID != "stream-01" {
		t.Fatalf("unexpected signal: auth=%q signal=%#v", gotAuth, got)
	}
}

func TestMetricPostsValue(t *testing.T) {
	var got Signal
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()

	client := Client{Config: Config{URL: server.URL, Token: "secret-token", ServiceID: "worker-01", Timeout: time.Second}}
	if err := client.Metric(t.Context(), "stream-01", "worker.overlay_events_total", "ok", 2, nil); err != nil {
		t.Fatal(err)
	}
	if got.Type != "metric" || got.Name != "worker.overlay_events_total" || got.Value == nil || *got.Value != 2 {
		t.Fatalf("unexpected metric signal: %#v", got)
	}
}

func TestReportErrorDoesNotLeakTokenOrResponseBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "secret-token", http.StatusForbidden)
	}))
	defer server.Close()

	client := Client{Config: Config{URL: server.URL, Token: "secret-token", ServiceID: "worker-01", Timeout: time.Second}}
	err := client.Event(t.Context(), "stream-01", "worker.event.sent", "sent", nil)
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), "secret-token") {
		t.Fatalf("token leaked in error: %v", err)
	}
}

func TestReportDoesNotFollowRedirectsWithBearerToken(t *testing.T) {
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

	client := Client{Config: Config{URL: server.URL, Token: "secret-token", ServiceID: "worker-01", Timeout: time.Second}}
	err := client.Event(t.Context(), "stream-01", "worker.event.sent", "sent", nil)
	if err == nil {
		t.Fatal("expected redirect response to fail")
	}
	if redirectedAuth != "" {
		t.Fatalf("authorization header followed redirect: %q", redirectedAuth)
	}
}

func TestValidateRejectsNonHTTPObservabilityURL(t *testing.T) {
	client := Client{Config: Config{URL: "ftp://observability.example.com/signals", Token: "<SERVICE_TOKEN>", ServiceID: "worker-01", Timeout: time.Second}}
	err := client.Validate()
	if err == nil {
		t.Fatal("expected validation error")
	}
	if !strings.Contains(err.Error(), "http or https") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateRejectsRemoteHTTPObservabilityURL(t *testing.T) {
	client := Client{Config: Config{URL: "http://observability.example.com", Token: "<SERVICE_TOKEN>", ServiceID: "worker-01", Timeout: time.Second}}
	err := client.Validate()
	if err == nil {
		t.Fatal("expected validation error")
	}
	if !strings.Contains(err.Error(), "https for remote hosts") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateAllowsLocalHTTPObservabilityURL(t *testing.T) {
	client := Client{Config: Config{URL: "http://host.docker.internal:18085", Token: "<SERVICE_TOKEN>", ServiceID: "worker-01", Timeout: time.Second}}
	if err := client.Validate(); err != nil {
		t.Fatalf("expected local http URL to be allowed: %v", err)
	}
}

func TestValidateRejectsObservabilityURLQueryOrFragment(t *testing.T) {
	client := Client{Config: Config{URL: "https://observability.example.com#frag", Token: "<SERVICE_TOKEN>", ServiceID: "worker-01", Timeout: time.Second}}
	err := client.Validate()
	if err == nil {
		t.Fatal("expected validation error")
	}
	if !strings.Contains(err.Error(), "query or fragment") {
		t.Fatalf("unexpected error: %v", err)
	}
}
