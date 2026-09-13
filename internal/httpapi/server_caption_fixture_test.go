package httpapi

import (
	"context"
	"encoding/base64"
	"github.com/example/autostream-worker/internal/control"
	"github.com/example/autostream-worker/internal/deepgram"
	"github.com/example/autostream-worker/internal/encoder"
	"github.com/example/autostream-worker/internal/ingesttoken"
	"github.com/example/autostream-worker/internal/jobs"
	"github.com/example/autostream-worker/internal/observability"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

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
	writeNodeConfigForVerifierTestWithIdentity(t, path, nodeType, "worker-01", 7)
}

func writeNodeConfigForVerifierTestWithIdentity(t *testing.T, path, nodeType, serviceID string, revision int64) {
	t.Helper()
	writeNodeListenerCredentialForVerifierTest(t, path, nodeType, revision)
	body := `panel:
  url: "https://panel.example.jp"
node:
  id: "` + serviceID + `"
  name: "Worker 01"
  type: "` + nodeType + `"
listener:
  credential: "node-listener.json"
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

func writeNodeListenerCredentialForVerifierTest(t *testing.T, configPath, serviceType string, revision int64) {
	t.Helper()
	credentialDir := filepath.Join(filepath.Dir(configPath), "credentials")
	if err := os.MkdirAll(credentialDir, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CREDENTIALS_DIRECTORY", credentialDir)
	body := `{"schema_version":2,"service_type":"` + serviceType + `","bind_address":"127.0.0.1:18084","config_revision":` + strconv.FormatInt(revision, 10) + `}`
	if err := os.WriteFile(filepath.Join(credentialDir, "node-listener.json"), []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
}
