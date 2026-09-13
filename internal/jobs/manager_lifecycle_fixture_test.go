package jobs

import (
	"github.com/example/autostream-worker/internal/control"
	"testing"
	"time"
)

func waitForManager(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out waiting for manager condition")
}

func testTime() time.Time {
	return time.Date(2026, 5, 28, 0, 0, 0, 0, time.UTC)
}

func captionRuntimeConfig(profiles ...control.RuntimeProfile) control.RuntimeConfig {
	return control.RuntimeConfig{
		Service: control.RegisteredService{ServiceID: "worker-01", ServiceType: control.ServiceType},
		Assignments: []control.StreamServiceAssignment{{
			StreamID:       "stream-01",
			ServiceID:      "worker-01",
			ServiceType:    control.ServiceType,
			AssignmentRole: "primary",
		}},
		Profiles: map[string][]control.RuntimeProfile{"caption": profiles},
	}
}

func captionProfileConfig(language string) map[string]any {
	return map[string]any{
		"service_id":          "worker-01",
		"provider":            "deepgram",
		"model":               "nova-3",
		"language":            language,
		"api_key_secret_name": "deepgram_api_key",
		"endpointing_ms":      300,
		"interim_results":     true,
		"smart_format":        true,
		"delay_ms":            800,
	}
}
