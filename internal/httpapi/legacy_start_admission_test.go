package httpapi

// Existing v2 tests remain on an explicitly unmanaged synthetic peer. New managed
// tests use normal product constructors and retain every prior v2 assertion.
import (
	"context"
	lifecycle "github.com/Kome-Lab/Autostream-Contracts/v3lifecycle"
	"github.com/example/autostream-worker/internal/jobs"
	"github.com/example/autostream-worker/internal/manualhold"
	"net/http"
	"os"
	"path/filepath"
)

func legacyStartManager(manager *jobs.Manager) *jobs.Manager {
	if manager == nil {
		manager = jobs.NewManager(nil, nil)
	}
	guard, err := manualhold.New(filepath.Join(os.TempDir(), "autostream-explicit-legacy-worker-v2"), manualhold.InspectorFunc(func(context.Context, lifecycle.Record) (lifecycle.Record, error) {
		return lifecycle.Record{}, manualhold.ErrUnknown
	}), func() (lifecycle.ProducerRef, error) { return lifecycle.ProducerRef{}, manualhold.ErrUnknown })
	if err != nil {
		panic(err)
	}
	guard.Require = func() bool { return false }
	manager.StartAdmission = guard
	return manager
}
func legacyNewServer(service string, manager *jobs.Manager, verifier TokenVerifier) http.Handler {
	return NewServer(service, legacyStartManager(manager), verifier)
}
func legacyNewServerWithRuntimeConfig(service string, manager *jobs.Manager, verifier TokenVerifier, runtime RuntimeConfigProvider) http.Handler {
	return NewServerWithRuntimeConfig(service, legacyStartManager(manager), verifier, runtime)
}
func legacyNewServerWithRuntimeConfigAndUpdaterIdentity(service string, manager *jobs.Manager, verifier TokenVerifier, runtime RuntimeConfigProvider, updater *UpdaterIdentityLatch) http.Handler {
	return NewServerWithRuntimeConfigAndUpdaterIdentity(service, legacyStartManager(manager), verifier, runtime, updater)
}
