package httpapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"github.com/example/autostream-worker/internal/control"
	"github.com/example/autostream-worker/internal/deepgram"
	"github.com/example/autostream-worker/internal/encoder"
	"github.com/example/autostream-worker/internal/ingesttoken"
	"github.com/example/autostream-worker/internal/jobs"
	"github.com/example/autostream-worker/internal/observability"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

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

func TestCaptionAudioEndpointAcceptsMatchingJobGeneration(t *testing.T) {
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
	for _, test := range []struct {
		name string
		body string
	}{
		{
			name: "missing",
			body: `{"stream_id":"stream-01","source":"discord","packets":[{"ssrc":42,"user_id":"user-42","connection_generation":1,"sequence":7,"timestamp":960,"received_at":"2026-07-14T00:00:00Z","opus_base64":"AQ=="}]}`,
		},
		{
			name: "zero",
			body: `{"stream_id":"stream-01","source":"discord","packets":[{"ssrc":42,"user_id":"user-42","job_generation":0,"connection_generation":1,"sequence":7,"timestamp":960,"received_at":"2026-07-14T00:00:00Z","opus_base64":"AQ=="}]}`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			const signingKey = "caption-generation-signing-key"
			manager, _ := captionReadyManager(t)
			server := httptest.NewServer(NewServer("worker", manager, TokenVerifier{IngestTokenSigningKey: signingKey}))
			defer server.Close()
			res := postJSON(t, server.URL+"/streams/stream-01/audio/opus", signedCaptionAudioAuthorization(t, signingKey, "stream-01"), test.body)
			defer res.Body.Close()
			if res.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", res.StatusCode)
			}
			var response map[string]string
			if err := json.NewDecoder(res.Body).Decode(&response); err != nil {
				t.Fatal(err)
			}
			if response["code"] != "job_generation_required" {
				t.Fatalf("unexpected generation response: %#v", response)
			}
		})
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
