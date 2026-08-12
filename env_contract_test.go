package worker_test

import (
	"os"
	"strings"
	"testing"
)

func TestHostAndContainerBindContract(t *testing.T) {
	env := readFile(t, ".env.example")
	if !strings.Contains(env, "AUTOSTREAM_BIND_ADDR=127.0.0.1:8084") {
		t.Error(".env.example must bind the host service to 127.0.0.1:8084")
	}
	for _, required := range []string{
		"AUTOSTREAM_WORKER_PORT=8084",
		"AUTOSTREAM_WORKER_CONTAINER_PORT=8080",
	} {
		if !strings.Contains(env, required) {
			t.Errorf(".env.example is missing Docker port default %q", required)
		}
	}
	if !strings.Contains(env, "1024") || !strings.Contains(env, "65535") {
		t.Error(".env.example must document the supported unprivileged port range")
	}
	if !strings.Contains(env, "legacy 127.0.0.1:8080 fallback") {
		t.Error(".env.example must document the env-unset legacy port fallback")
	}
	if !strings.Contains(env, "AUTOSTREAM_CONFIG_REVISION=1") ||
		!strings.Contains(strings.ToLower(env), "root-owned") {
		t.Error(".env.example must document the root-owned updater probe config revision")
	}
	if !strings.Contains(env, "AUTOSTREAM_SCENE_FONT_FILE=/usr/share/fonts/opentype/noto/NotoSansCJK-Regular.ttc") ||
		!strings.Contains(env, "no basic-font fallback") {
		t.Error(".env.example must document the Japanese Noto scene font override and fail-closed behavior")
	}

	dockerfile := readFile(t, "Dockerfile")
	// Worker emits low-rate JPEG scene frames. The Encoder/Recorder owns the
	// FFmpeg H.264/MPEG-TS encode and audio mux, so those packages must not be
	// part of this Worker runtime contract.
	for _, required := range []string{"fontconfig", "fonts-noto-cjk", "NotoSansCJK-Regular.ttc", "AUTOSTREAM_SCENE_FONT_FILE"} {
		if !strings.Contains(dockerfile, required) {
			t.Errorf("Dockerfile is missing scene runtime contract %q", required)
		}
	}

	installer := readFile(t, "release/install-autostream-worker")
	for _, required := range []string{"DEFAULT_SCENE_FONT_FILE", "required Japanese Noto scene font is unavailable"} {
		if !strings.Contains(installer, required) {
			t.Errorf("installer is missing scene runtime preflight %q", required)
		}
	}

	base := readFile(t, "docker-compose.yml")
	for _, required := range []string{
		"AUTOSTREAM_CONFIG_REVISION: ${AUTOSTREAM_CONFIG_REVISION:-1}",
		"AUTOSTREAM_BIND_ADDR: 0.0.0.0:${AUTOSTREAM_WORKER_CONTAINER_PORT:-8080}",
		`127.0.0.1:${AUTOSTREAM_WORKER_PORT:-8084}:${AUTOSTREAM_WORKER_CONTAINER_PORT:-8080}`,
	} {
		if !strings.Contains(base, required) {
			t.Errorf("base compose is missing %q", required)
		}
	}

	local := readFile(t, "docker-compose.local.yml")
	for _, required := range []string{
		"AUTOSTREAM_CONFIG_REVISION: ${AUTOSTREAM_CONFIG_REVISION:-1}",
		"AUTOSTREAM_BIND_ADDR: 0.0.0.0:${AUTOSTREAM_WORKER_CONTAINER_PORT:-8080}",
		`127.0.0.1:${AUTOSTREAM_WORKER_PORT:-8084}:${AUTOSTREAM_WORKER_CONTAINER_PORT:-8080}`,
	} {
		if !strings.Contains(local, required) {
			t.Errorf("local compose is missing %q", required)
		}
	}

	production := readFile(t, "docker-compose.prod.yml")
	for _, required := range []string{
		"AUTOSTREAM_CONFIG_REVISION: ${AUTOSTREAM_CONFIG_REVISION:-1}",
		"AUTOSTREAM_BIND_ADDR: 0.0.0.0:${AUTOSTREAM_WORKER_CONTAINER_PORT:-8080}",
		"ports: !override",
		`127.0.0.1:${AUTOSTREAM_WORKER_PORT:-8084}:${AUTOSTREAM_WORKER_CONTAINER_PORT:-8080}`,
	} {
		if !strings.Contains(production, required) {
			t.Errorf("production compose is missing %q", required)
		}
	}

	unit := readFile(t, "systemd/autostream-worker.service.example")
	primaryEnv := "EnvironmentFile=/etc/autostream/worker.env"
	managedEnv := "EnvironmentFile=-/opt/autostream/local-executor/ports/worker.env"
	if !strings.Contains(unit, primaryEnv) {
		t.Error("systemd unit must load the configurable bind address from worker.env")
	}
	if !strings.Contains(unit, managedEnv) {
		t.Error("systemd unit must optionally load the Control Panel managed port sidecar")
	}
	if strings.Index(unit, managedEnv) <= strings.Index(unit, primaryEnv) {
		t.Error("managed port sidecar must load after worker.env so its bind address and revision win")
	}
	if strings.Contains(unit, "8084") {
		t.Error("systemd unit must not hard-code the worker port")
	}
	if !strings.Contains(unit, "AUTOSTREAM_CONFIG_REVISION") ||
		!strings.Contains(unit, "root-owned") {
		t.Error("systemd unit must document the root-owned config revision environment")
	}

	install := readFile(t, "release/README.install.md")
	for _, required := range []string{
		"AUTOSTREAM_CONFIG_REVISION=1",
		"version, service_id, service_type, and config_revision",
		`PROBE_HOST="${PROBE_HOST:-127.0.0.1}"`,
		"PROBE_HOST='[::1]'",
	} {
		if !strings.Contains(install, required) {
			t.Errorf("release install guide is missing %q", required)
		}
	}

	readme := readFile(t, "README.md")
	for _, required := range []string{
		"host/reverse-proxy responsibility",
		"`1024` through `65535`",
		"The production health authority is the host Local Executor.",
		"intentionally omit an in-container `healthcheck`",
		"does not add or repurpose `curl`, `wget`, or another unrelated executable",
		"probes the loopback published port for both `/health` and `/updater/version`",
		"the published port is the health port",
	} {
		if !strings.Contains(readme, required) {
			t.Errorf("README is missing Docker published-port boundary %q", required)
		}
	}
}

func TestProductionComposePersistsStoppedTargetReceiptDirectory(t *testing.T) {
	const (
		receiptVolume = "worker-stopped-target-receipts"
		receiptPath   = "/var/lib/autostream/worker"
	)

	production := readFile(t, "docker-compose.prod.yml")
	if !strings.Contains(production, "\nvolumes:\n  "+receiptVolume+":\n") {
		t.Errorf("production compose must define named receipt volume %q", receiptVolume)
	}
	if !strings.Contains(production, "\n      - "+receiptVolume+":"+receiptPath+"\n") {
		t.Errorf("production compose must mount %q at %q", receiptVolume, receiptPath)
	}
	if strings.Contains(production, receiptVolume+":"+receiptPath+":ro") {
		t.Errorf("production compose must mount %q read-write so the worker can persist receipts", receiptVolume)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}
