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

	base := readFile(t, "docker-compose.yml")
	for _, required := range []string{
		"AUTOSTREAM_BIND_ADDR: 0.0.0.0:8080",
		`- "8084:8080"`,
	} {
		if !strings.Contains(base, required) {
			t.Errorf("base compose is missing %q", required)
		}
	}

	production := readFile(t, "docker-compose.prod.yml")
	for _, required := range []string{
		"AUTOSTREAM_BIND_ADDR: 0.0.0.0:8080",
		"ports: !override",
		`- "127.0.0.1:8084:8080"`,
	} {
		if !strings.Contains(production, required) {
			t.Errorf("production compose is missing %q", required)
		}
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
