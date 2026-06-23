package main

import (
	"testing"
	"time"

	"github.com/example/autostream-worker/internal/control"
)

func TestWorkerProfileDefaultsFromRuntimeConfigUsesOnlyOwnServiceProfiles(t *testing.T) {
	cfg := control.RuntimeConfig{
		Service: control.RegisteredService{ServiceID: "worker-01"},
		Profiles: map[string][]control.RuntimeProfile{
			"overlay": {
				{ID: "overlay-other", Kind: "overlay", Config: map[string]any{"service_id": "worker-02"}, CreatedAt: testProfileTime(), UpdatedAt: testProfileTime()},
				{ID: "overlay-own", Kind: "overlay", Config: map[string]any{"service_id": "worker-01"}, CreatedAt: testProfileTime(), UpdatedAt: testProfileTime()},
			},
			"caption": {
				{ID: "caption-other", Kind: "caption", Config: map[string]any{"service_id": "worker-02"}, CreatedAt: testProfileTime(), UpdatedAt: testProfileTime()},
				{ID: "caption-global", Kind: "caption", Config: map[string]any{}, CreatedAt: testProfileTime(), UpdatedAt: testProfileTime()},
			},
		},
	}

	defaults := workerProfileDefaultsFromRuntimeConfig(cfg)
	if defaults.OverlayProfileID != "overlay-own" {
		t.Fatalf("expected own overlay profile, got %#v", defaults)
	}
	if defaults.CaptionProfileID != "caption-global" {
		t.Fatalf("expected unscoped caption profile fallback, got %#v", defaults)
	}
}

func TestWorkerProfileDefaultsFromRuntimeConfigRejectsMalformedServiceID(t *testing.T) {
	cfg := control.RuntimeConfig{
		Service: control.RegisteredService{ServiceID: "worker-01"},
		Profiles: map[string][]control.RuntimeProfile{
			"overlay": {
				{ID: "overlay-malformed", Kind: "overlay", Config: map[string]any{"service_id": []string{"worker-01"}}, CreatedAt: testProfileTime(), UpdatedAt: testProfileTime()},
			},
		},
	}

	defaults := workerProfileDefaultsFromRuntimeConfig(cfg)
	if defaults.OverlayProfileID != "" || defaults.CaptionProfileID != "" {
		t.Fatalf("malformed service-scoped profile should not be applied: %#v", defaults)
	}
}

func TestWorkerAssignmentPolicyFromRuntimeConfigUsesOnlyOwnPrimaryWorkerAssignments(t *testing.T) {
	cfg := control.RuntimeConfig{
		Service: control.RegisteredService{ServiceID: "worker-01"},
		Assignments: []control.StreamServiceAssignment{
			{StreamID: "stream-primary", ServiceID: "worker-01", ServiceType: "worker", AssignmentRole: "primary"},
			{StreamID: "stream-standby", ServiceID: "worker-01", ServiceType: "worker", AssignmentRole: "standby"},
			{StreamID: "stream-other-worker", ServiceID: "worker-02", ServiceType: "worker", AssignmentRole: "primary"},
			{StreamID: "stream-encoder", ServiceID: "encoder-01", ServiceType: "encoder_recorder", AssignmentRole: "primary"},
		},
	}

	policy := workerAssignmentPolicyFromRuntimeConfig(cfg)
	if !policy.Enforce {
		t.Fatal("expected runtime assignment policy to be enforced")
	}
	if !policy.PrimaryStreams["stream-primary"] {
		t.Fatalf("expected own primary worker assignment to be allowed: %#v", policy.PrimaryStreams)
	}
	for _, streamID := range []string{"stream-standby", "stream-other-worker", "stream-encoder"} {
		if policy.PrimaryStreams[streamID] {
			t.Fatalf("unexpected assignment allowed for %s: %#v", streamID, policy.PrimaryStreams)
		}
	}
}

func TestRequireControlPanelRuntimeConfigInProduction(t *testing.T) {
	t.Setenv("AUTOSTREAM_ENV", "production")
	if !requireControlPanelRuntimeConfig() {
		t.Fatal("expected production worker to require Control Panel runtime config")
	}
}

func TestRequireControlPanelRuntimeConfigExplicitEnv(t *testing.T) {
	t.Setenv("AUTOSTREAM_REQUIRE_CONTROL_PANEL_RUNTIME_CONFIG", "true")
	if !requireControlPanelRuntimeConfig() {
		t.Fatal("expected explicit runtime config requirement")
	}
}

func testProfileTime() time.Time {
	return time.Date(2026, 6, 10, 0, 0, 0, 0, time.UTC)
}
