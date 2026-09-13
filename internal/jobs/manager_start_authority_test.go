package jobs

import (
	"context"
	"errors"
	"github.com/example/autostream-worker/internal/control"
	"github.com/example/autostream-worker/internal/deepgram"
	"github.com/example/autostream-worker/internal/observability"
	"strings"
	"testing"
)

func TestManagerStartFailsAtomicallyWhenRuntimeSecretResolutionFails(t *testing.T) {
	manager := NewManager(&fakePublisher{}, observability.Client{})
	resolveCalls := 0
	factoryCalls := 0
	manager.SetCaptionRuntime(RuntimeSecretResolverFunc(func(context.Context, string, string) (control.RuntimeSecret, error) {
		resolveCalls++
		return control.RuntimeSecret{}, errors.New("control panel failed with dg-secret-must-not-leak")
	}), CaptionSessionFactoryFunc(func(deepgram.Config, []byte, deepgram.Handler) (CaptionSession, error) {
		factoryCalls++
		return &fakeCaptionSession{}, nil
	}))
	manager.ApplyRuntimeConfig(captionRuntimeConfig(control.RuntimeProfile{ID: "caption-01", Kind: "caption", Config: captionProfileConfig("ja")}))

	err := manager.Start(t.Context(), StreamContext{StreamID: "stream-01", CaptionProfileID: "caption-01"})
	if !errors.Is(err, ErrCaptionRuntimeUnavailable) {
		t.Fatalf("unexpected start error: %v", err)
	}
	if strings.Contains(err.Error(), "dg-secret-must-not-leak") {
		t.Fatalf("runtime secret leaked in start error: %v", err)
	}
	if manager.CurrentStreamID() != "" {
		t.Fatalf("job started despite caption initialization failure: %#v", manager.Status())
	}
	if resolveCalls != 1 || factoryCalls != 0 {
		t.Fatalf("unexpected initialization calls: resolve=%d factory=%d", resolveCalls, factoryCalls)
	}
}

func TestManagerClosesPartialCaptionSessionWhenFactoryFails(t *testing.T) {
	manager := NewManager(&fakePublisher{}, observability.Client{})
	session := &fakeCaptionSession{}
	manager.SetCaptionRuntime(RuntimeSecretResolverFunc(func(_ context.Context, _, secretName string) (control.RuntimeSecret, error) {
		return control.RuntimeSecret{SecretName: secretName, Value: "dg-runtime-key", ExpiresInSec: 300}, nil
	}), CaptionSessionFactoryFunc(func(deepgram.Config, []byte, deepgram.Handler) (CaptionSession, error) {
		return session, errors.New("factory failed with dg-runtime-key")
	}))
	manager.ApplyRuntimeConfig(captionRuntimeConfig(control.RuntimeProfile{ID: "caption-01", Kind: "caption", Config: captionProfileConfig("ja")}))

	err := manager.Start(t.Context(), StreamContext{StreamID: "stream-01", CaptionProfileID: "caption-01"})
	if !errors.Is(err, ErrCaptionRuntimeUnavailable) || strings.Contains(err.Error(), "dg-runtime-key") {
		t.Fatalf("unexpected factory error: %v", err)
	}
	if session.closed != 1 || manager.CurrentStreamID() != "" {
		t.Fatalf("partial session was not closed: closed=%d status=%#v", session.closed, manager.Status())
	}
}

func TestManagerRejectsUnsupportedCaptionProviderBeforeSecretResolution(t *testing.T) {
	manager := NewManager(&fakePublisher{}, observability.Client{})
	resolveCalls := 0
	manager.SetCaptionRuntime(RuntimeSecretResolverFunc(func(context.Context, string, string) (control.RuntimeSecret, error) {
		resolveCalls++
		return control.RuntimeSecret{}, nil
	}), nil)
	config := captionProfileConfig("ja")
	config["provider"] = "other"
	manager.ApplyRuntimeConfig(captionRuntimeConfig(control.RuntimeProfile{ID: "caption-01", Kind: "caption", Config: config}))

	err := manager.Start(t.Context(), StreamContext{StreamID: "stream-01", CaptionProfileID: "caption-01"})
	if !errors.Is(err, ErrCaptionProfileInvalid) {
		t.Fatalf("unexpected profile error: %v", err)
	}
	if resolveCalls != 0 || manager.CurrentStreamID() != "" {
		t.Fatalf("unsupported provider reached runtime initialization: calls=%d status=%#v", resolveCalls, manager.Status())
	}
}

func TestManagerStartRequiresPrimaryAssignmentWhenPolicyEnforced(t *testing.T) {
	manager := NewManager(&fakePublisher{}, observability.Client{})
	manager.SetAssignmentPolicy(AssignmentPolicy{
		Enforce:        true,
		PrimaryStreams: map[string]bool{"stream-01": true},
	})
	if err := manager.Start(t.Context(), StreamContext{StreamID: "stream-02"}); err == nil {
		t.Fatal("expected unassigned stream to be rejected")
	}
	if err := manager.Start(t.Context(), StreamContext{StreamID: "stream-01"}); err != nil {
		t.Fatalf("expected assigned primary stream to start: %v", err)
	}
}

func TestManagerRejectsWrongStream(t *testing.T) {
	manager := NewManager(&fakePublisher{}, observability.Client{})
	if err := manager.Start(t.Context(), StreamContext{StreamID: "stream-01"}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.CurrentTime(t.Context(), "stream-02", testTime()); err == nil {
		t.Fatal("expected wrong stream to be rejected")
	}
}

func TestCustomOverlayRequiresKnownPrefix(t *testing.T) {
	manager := NewManager(&fakePublisher{}, observability.Client{})
	if err := manager.Start(t.Context(), StreamContext{StreamID: "stream-01"}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.CustomOverlay(t.Context(), "stream-01", "bad.event", nil, testTime()); err == nil {
		t.Fatal("expected bad event type to be rejected")
	}
}
