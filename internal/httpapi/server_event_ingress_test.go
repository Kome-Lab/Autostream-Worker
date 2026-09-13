package httpapi

import (
	"bytes"
	"encoding/json"
	"github.com/example/autostream-worker/internal/control"
	"github.com/example/autostream-worker/internal/encoder"
	"github.com/example/autostream-worker/internal/ingesttoken"
	"github.com/example/autostream-worker/internal/jobs"
	"github.com/example/autostream-worker/internal/observability"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestNewServerFailsClosedOnInvalidListenerConfigRevision(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yml")
	writeNodeConfigForVerifierTest(t, configPath, control.ServiceType)
	writeNodeListenerCredentialForVerifierTest(t, configPath, control.ServiceType, 0)
	t.Setenv("AUTOSTREAM_NODE_CONFIG", configPath)
	defer func() {
		if recover() == nil {
			t.Fatal("NewServer must reject an invalid listener config_revision")
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

	res = post("/streams/stream-01/events/overlay", `{"type":"overlay.discord_chat","payload":{"message_id":"msg-01","author_id":"user-01","display_name":"alice","content":"こんにちは","text_channel_id":"text-01","created_at":"2026-07-01T12:00:00Z"}}`)
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
	if forwarded.Type != "overlay.discord_chat" || forwarded.StreamID != "stream-01" || forwarded.Payload["message_id"] != "msg-01" || forwarded.Payload["author_id"] != "user-01" || forwarded.Payload["display_name"] != "alice" || forwarded.Payload["content"] != "こんにちは" || forwarded.Payload["text_channel_id"] != "text-01" || forwarded.Payload["created_at"] != "2026-07-01T12:00:00Z" {
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
	eventReq, err := http.NewRequest(http.MethodPost, server.URL+"/streams/stream-01/events/overlay", strings.NewReader(`{"type":"overlay.discord_chat","payload":{"message_id":"msg-01","author_id":"user-01","content":"hello"}}`))
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
