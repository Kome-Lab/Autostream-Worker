package control

import (
	"os"
	"path/filepath"
	"testing"
)

func TestConfigFromEnvUsesNodeConfig(t *testing.T) {
	path := writeNodeConfigForTest(t, "worker")
	t.Setenv("AUTOSTREAM_NODE_CONFIG", path)
	cfg := ConfigFromEnv()
	if cfg.ControlPanelURL != "https://panel.example.jp" || cfg.Token != "runtime-secret" || cfg.ServiceID != "worker-01" || cfg.ServiceName != "Worker 01" || cfg.ServicePublicURL != "https://worker.example.jp:8443" {
		t.Fatalf("unexpected config from node file: %#v", cfg)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("node config should validate: %v", err)
	}
	if got := NodeRuntimeTokenFromEnv(); got != "runtime-secret" {
		t.Fatalf("runtime token = %q", got)
	}
	t.Setenv("AUTOSTREAM_STREAM_INGEST_SIGNING_KEY", "legacy-env-signing-key")
	if got := StreamIngestSigningKey(); got != "node-config-signing-key" {
		t.Fatalf("stream ingest signing key = %q", got)
	}
}

func TestConfigFromEnvRejectsWrongNodeType(t *testing.T) {
	path := writeNodeConfigForTest(t, "encoder_recorder")
	t.Setenv("AUTOSTREAM_NODE_CONFIG", path)
	cfg := ConfigFromEnv()
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected wrong node type to fail validation")
	}
}

func TestConfigFromEnvTreatsMissingNodeConfigAsPending(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", "config.yml")
	t.Setenv("AUTOSTREAM_NODE_CONFIG", path)
	t.Setenv("CONTROL_PANEL_URL", "https://legacy-panel.example.jp")
	t.Setenv("CONTROL_PANEL_TOKEN", "legacy-token")
	t.Setenv("SERVICE_ID", "legacy-worker")
	t.Setenv("SERVICE_NAME", "Legacy Worker")
	t.Setenv("SERVICE_PUBLIC_URL", "https://legacy-worker.example.jp")
	cfg := ConfigFromEnv()
	if cfg.ConfigError != "" {
		t.Fatalf("missing node config should not be fatal: %#v", cfg)
	}
	if cfg.ControlPanelURL != "" || cfg.Token != "" || cfg.ServiceID != "" || cfg.ServiceName != "" || cfg.ServicePublicURL != "" {
		t.Fatalf("configured node path must clear legacy panel identity while pending: %#v", cfg)
	}
	if !NodeConfigPendingFromEnv() {
		t.Fatal("missing node config should be reported as pending")
	}
	if got := NodeRuntimeTokenFromEnv(); got != "" {
		t.Fatalf("runtime token = %q, want empty", got)
	}
	t.Setenv("AUTOSTREAM_STREAM_INGEST_SIGNING_KEY", "legacy-env-signing-key")
	if got := StreamIngestSigningKey(); got != "" {
		t.Fatalf("configured node path must not fall back to the legacy signing key, got %q", got)
	}
}

func writeNodeConfigForTest(t *testing.T, nodeType string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yml")
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
stream_ingest:
  signing_key: "node-config-signing-key"
`
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}
