package control

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestRunConfigureCommandWritesNodeConfig(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/node-agent/configure" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"config_yml":"panel:\n  url: \"https://panel.example.jp\"\nnode:\n  id: \"worker-01\"\n  name: \"Worker 01\"\n  type: \"worker\"\napi:\n  host: \"worker.example.jp\"\n  port: 8443\n  ssl_enabled: true\nauth:\n  token_id: \"runtime-token-id\"\n  token: \"runtime-token\"\n"}`))
	}))
	defer server.Close()

	configPath := filepath.Join(t.TempDir(), "config.yml")
	var out bytes.Buffer
	err := RunConfigureCommand([]string{"--panel-url", server.URL, "--token", "configure-token", "--node", "worker-01", "--config", configPath}, ServiceType, &out)
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `id: "worker-01"`) || strings.Contains(string(body), "configure-token") {
		t.Fatalf("unexpected config body: %s", body)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(configPath)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != nodeConfigFileMode {
			t.Fatalf("config mode = %v, want %v", got, nodeConfigFileMode)
		}
	}
	if !strings.Contains(out.String(), "configure succeeded: wrote") || !strings.Contains(out.String(), "worker-01") {
		t.Fatalf("missing success output: %s", out.String())
	}
}

func TestRunConfigureCommandRejectsNodeTypeMismatchBeforeWrite(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"config_yml":"panel:\n  url: \"https://panel.example.jp\"\nnode:\n  id: \"worker-01\"\n  name: \"Discord 01\"\n  type: \"discord_bot\"\napi:\n  host: \"discord.example.jp\"\n  port: 8443\n  ssl_enabled: true\nauth:\n  token_id: \"runtime-token-id\"\n  token: \"runtime-token\"\n"}`))
	}))
	defer server.Close()

	configPath := filepath.Join(t.TempDir(), "config.yml")
	err := RunConfigureCommand([]string{"--panel-url", server.URL, "--token", "configure-token", "--node", "worker-01", "--config", configPath}, ServiceType, nil)
	if err == nil || !strings.Contains(err.Error(), "node.type") {
		t.Fatalf("expected node type mismatch, got %v", err)
	}
	if _, statErr := os.Stat(configPath); !os.IsNotExist(statErr) {
		t.Fatalf("config should not be written on mismatch, stat err=%v", statErr)
	}
}
