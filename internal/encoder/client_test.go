package encoder

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestPublishPostsWorkerEvent(t *testing.T) {
	var gotAuth string
	var got Event
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/worker-events" {
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
	if err := client.Publish(t.Context(), Event{ID: "event-01", StreamID: "stream-01", Type: "overlay.current_time", Payload: map[string]any{"ok": true}}); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer secret-token" || got.StreamID != "stream-01" || got.ServiceID != "worker-01" || got.Timestamp.IsZero() {
		t.Fatalf("unexpected request: auth=%q event=%#v", gotAuth, got)
	}
}

func TestPublishUsesEventTokenWhenProvided(t *testing.T) {
	var gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()

	client := Client{Config: Config{URL: server.URL, Token: "env-token", Timeout: time.Second}}
	err := client.Publish(t.Context(), Event{ID: "event-01", StreamID: "stream-01", Type: "overlay.current_time", Token: "job-stream-ingest-token"})
	if err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer job-stream-ingest-token" {
		t.Fatalf("expected job token to override env token, got %q", gotAuth)
	}
}

func TestPublishUsesJobScopedRouteWithoutStaticConfiguration(t *testing.T) {
	var gotAuth string
	var got Event
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()

	client := Client{Config: Config{ServiceID: "worker-01", Timeout: time.Second}}
	err := client.Publish(t.Context(), Event{
		ID:       "event-01",
		StreamID: "stream-01",
		Type:     "overlay.current_time",
		URL:      server.URL,
		Token:    "job-stream-ingest-token",
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer job-stream-ingest-token" || got.ServiceID != "worker-01" {
		t.Fatalf("unexpected job-scoped request: auth=%q event=%#v", gotAuth, got)
	}
}

func TestPublishErrorDoesNotLeakTokenOrBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "secret-token", http.StatusForbidden)
	}))
	defer server.Close()

	client := Client{Config: Config{URL: server.URL, Token: "secret-token", Timeout: time.Second}}
	err := client.Publish(t.Context(), Event{ID: "event-01", StreamID: "stream-01", Type: "overlay.current_time"})
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), "secret-token") {
		t.Fatalf("token leaked in error: %v", err)
	}
}

func TestPublishDoesNotFollowRedirectsWithBearerToken(t *testing.T) {
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

	client := Client{Config: Config{URL: server.URL, Token: "secret-token", Timeout: time.Second}}
	err := client.Publish(t.Context(), Event{ID: "event-01", StreamID: "stream-01", Type: "overlay.current_time"})
	if err == nil {
		t.Fatal("expected redirect response to fail")
	}
	if redirectedAuth != "" {
		t.Fatalf("authorization header followed redirect: %q", redirectedAuth)
	}
}

func TestPublishRetriesTransientFailures(t *testing.T) {
	var attempts int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts == 1 {
			http.Error(w, "temporary upstream failure", http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()

	client := Client{Config: Config{URL: server.URL, Token: "secret-token", Timeout: time.Second, RetryMax: 2, RetryBaseDelay: time.Millisecond}}
	if err := client.Publish(t.Context(), Event{ID: "event-01", StreamID: "stream-01", Type: "overlay.current_time"}); err != nil {
		t.Fatal(err)
	}
	if attempts != 2 {
		t.Fatalf("expected retry after transient failure, got %d attempts", attempts)
	}
}

func TestPublishDoesNotRetryValidationFailures(t *testing.T) {
	var attempts int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		http.Error(w, "bad request", http.StatusBadRequest)
	}))
	defer server.Close()

	client := Client{Config: Config{URL: server.URL, Token: "secret-token", Timeout: time.Second, RetryMax: 2, RetryBaseDelay: time.Millisecond}}
	if err := client.Publish(t.Context(), Event{ID: "event-01", StreamID: "stream-01", Type: "overlay.current_time"}); err == nil {
		t.Fatal("expected non-transient error")
	}
	if attempts != 1 {
		t.Fatalf("non-transient failure should not be retried, got %d attempts", attempts)
	}
}

func TestValidateRejectsNonHTTPEncoderURL(t *testing.T) {
	cfg := Config{URL: "ftp://encoder.example.com/events", Token: "<SERVICE_TOKEN>", Timeout: time.Second}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected validation error")
	}
	if !strings.Contains(err.Error(), "http or https") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateRejectsRemoteHTTPEncoderURL(t *testing.T) {
	cfg := Config{URL: "http://encoder.example.com", Token: "<SERVICE_TOKEN>", Timeout: time.Second}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected validation error")
	}
	if !strings.Contains(err.Error(), "https for remote hosts") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateAllowsLocalHTTPEncoderURL(t *testing.T) {
	cfg := Config{URL: "http://127.0.0.1:18084", Token: "<SERVICE_TOKEN>", Timeout: time.Second}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected local http URL to be allowed: %v", err)
	}
}

func TestValidateRejectsEncoderURLQueryOrFragment(t *testing.T) {
	cfg := Config{URL: "https://encoder.example.com?token=bad", Token: "<SERVICE_TOKEN>", Timeout: time.Second}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected validation error")
	}
	if !strings.Contains(err.Error(), "query or fragment") {
		t.Fatalf("unexpected error: %v", err)
	}
}
