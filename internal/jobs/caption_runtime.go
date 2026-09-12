package jobs

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/example/autostream-worker/internal/control"
	"github.com/example/autostream-worker/internal/deepgram"
)

type RuntimeSecretResolver interface {
	ResolveRuntimeSecret(context.Context, string, string) (control.RuntimeSecret, error)
}

type RuntimeSecretResolverFunc func(context.Context, string, string) (control.RuntimeSecret, error)

func (f RuntimeSecretResolverFunc) ResolveRuntimeSecret(ctx context.Context, streamID, secretName string) (control.RuntimeSecret, error) {
	if f == nil {
		return control.RuntimeSecret{}, ErrCaptionRuntimeUnavailable
	}
	return f(ctx, streamID, secretName)
}

type CaptionSession interface {
	Ingest(context.Context, deepgram.AudioPacket) error
	Close(context.Context) error
}

type captionSessionStatusProvider interface {
	Status() deepgram.Status
}

type CaptionSessionFactory interface {
	New(deepgram.Config, []byte, deepgram.Handler) (CaptionSession, error)
}

type CaptionSessionFactoryFunc func(deepgram.Config, []byte, deepgram.Handler) (CaptionSession, error)

func (f CaptionSessionFactoryFunc) New(config deepgram.Config, apiKey []byte, handler deepgram.Handler) (CaptionSession, error) {
	if f == nil {
		return nil, ErrCaptionRuntimeUnavailable
	}
	return f(config, apiKey, handler)
}

type deepgramSessionFactory struct{}

func (deepgramSessionFactory) New(config deepgram.Config, apiKey []byte, handler deepgram.Handler) (CaptionSession, error) {
	return deepgram.NewSession(config, apiKey, handler)
}

func (m *Manager) SetCaptionRuntime(resolver RuntimeSecretResolver, factory CaptionSessionFactory) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.secretResolver = resolver
	if factory == nil {
		factory = deepgramSessionFactory{}
	}
	m.captionFactory = factory
}

// UpdateCaptionRuntimeSettings replaces only the active Deepgram session and
// caption display policy. The stream job generation remains stable so the
// existing Discord audio route and video output continue without a restart.
// A replacement session is prepared before the active pointers are swapped;
// any preparation failure therefore leaves the old session untouched.
func (m *Manager) UpdateCaptionRuntimeSettings(ctx context.Context, streamID, profileID string) error {
	m.lifecycleMu.Lock()
	defer m.lifecycleMu.Unlock()

	streamID = strings.TrimSpace(streamID)
	profileID = strings.TrimSpace(profileID)
	if streamID == "" || profileID == "" {
		return ErrCaptionProfileInvalid
	}

	m.mu.Lock()
	switch {
	case m.current.StreamID == "":
		m.mu.Unlock()
		return ErrNoActiveStreamJob
	case m.stopping:
		m.mu.Unlock()
		return ErrStreamStopping
	case m.current.StreamID != streamID:
		m.mu.Unlock()
		return ErrStreamIDDoesNotMatchJob
	}
	profile, profileSelected := m.captionProfiles[profileID]
	resolver := m.secretResolver
	factory := m.captionFactory
	jobGeneration := m.jobGeneration
	nextCaptionSessionGeneration := m.captionSessionGeneration + 1
	m.mu.Unlock()

	if !profileSelected {
		return m.captionRuntimeUpdateFailed(ctx, streamID, profileID, jobGeneration, "profile_not_found", ErrCaptionProfileInvalid)
	}
	config, secretName, err := captionConfig(profile)
	if err != nil {
		return m.captionRuntimeUpdateFailed(ctx, streamID, profileID, jobGeneration, "profile_invalid", ErrCaptionProfileInvalid)
	}
	displayConfig, displayOK := captionDisplayConfigFromProfile(profile.Config)
	if !displayOK {
		return m.captionRuntimeUpdateFailed(ctx, streamID, profileID, jobGeneration, "profile_invalid", ErrCaptionProfileInvalid)
	}
	if resolver == nil || factory == nil {
		return m.captionRuntimeUpdateFailed(ctx, streamID, profileID, jobGeneration, "runtime_unavailable", ErrCaptionRuntimeUnavailable)
	}
	secret, err := resolver.ResolveRuntimeSecret(ctx, streamID, secretName)
	if err != nil {
		return m.captionRuntimeUpdateFailed(ctx, streamID, profileID, jobGeneration, "secret_resolve_failed", ErrCaptionRuntimeUnavailable)
	}
	if secret.SecretName != secretName || strings.TrimSpace(secret.Value) == "" || secret.ExpiresInSec <= 0 {
		secret.Value = ""
		return m.captionRuntimeUpdateFailed(ctx, streamID, profileID, jobGeneration, "secret_invalid", ErrCaptionRuntimeUnavailable)
	}
	apiKey := []byte(secret.Value)
	secret.Value = ""
	newSession, err := factory.New(config, apiKey, m.captionTranscriptHandler(streamID, jobGeneration, nextCaptionSessionGeneration))
	zeroBytes(apiKey)
	if err != nil || newSession == nil {
		closeCaptionSession(newSession)
		return m.captionRuntimeUpdateFailed(ctx, streamID, profileID, jobGeneration, "session_create_failed", ErrCaptionRuntimeUnavailable)
	}
	if err := ctx.Err(); err != nil {
		closeCaptionSession(newSession)
		return err
	}

	newIngress := newCaptionAudioIngress()
	m.mu.Lock()
	switch {
	case m.current.StreamID == "":
		m.mu.Unlock()
		closeCaptionSession(newSession)
		return ErrNoActiveStreamJob
	case m.stopping:
		m.mu.Unlock()
		closeCaptionSession(newSession)
		return ErrStreamStopping
	case m.current.StreamID != streamID:
		m.mu.Unlock()
		closeCaptionSession(newSession)
		return ErrStreamIDDoesNotMatchJob
	case m.jobGeneration != jobGeneration:
		m.mu.Unlock()
		closeCaptionSession(newSession)
		return ErrJobGenerationMismatch
	case !m.streamAssignedLocked(streamID):
		m.mu.Unlock()
		closeCaptionSession(newSession)
		return errors.New("stream is not assigned to this worker service as primary")
	}
	oldSession := m.captionSession
	oldIngress := m.captionIngress
	sceneRenderer := m.sceneRenderer
	m.current.CaptionProfileID = profileID
	m.captionSession = newSession
	m.captionIngress = newIngress
	m.captionSessionGeneration = nextCaptionSessionGeneration
	m.captionAudioStartedGeneration = 0
	m.mu.Unlock()

	go m.runCaptionAudioIngress(newIngress)
	if sceneRenderer != nil {
		sceneRenderer.ConfigureDisplay(displayConfig.maxItems, displayConfig.reorderWindow, displayConfig.interimTTL, displayConfig.finalTTL, displayConfig.showVoiceTranscripts)
	}

	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cleanupCancel()
	stopCaptionAudioIngress(cleanupCtx, oldIngress)
	if oldSession != nil {
		if err := oldSession.Close(cleanupCtx); err != nil {
			m.report(cleanupCtx, streamID, "worker.caption.runtime_update_old_session_close_failed", "failed", map[string]any{
				"caption_profile_id":         profileID,
				"job_generation":             jobGeneration,
				"caption_session_generation": nextCaptionSessionGeneration,
			})
		}
	}
	m.report(ctx, streamID, "worker.caption.runtime_updated", "applied", map[string]any{
		"caption_profile_id":         profileID,
		"job_generation":             jobGeneration,
		"caption_session_generation": nextCaptionSessionGeneration,
	})
	return nil
}

func (m *Manager) captionStartFailed(ctx context.Context, streamID, profileID, reason string, target error) error {
	m.report(ctx, streamID, "worker.caption.start_failed", "failed", map[string]any{
		"caption_profile_id": profileID,
		"reason":             reason,
	})
	return target
}

func (m *Manager) captionRuntimeUpdateFailed(ctx context.Context, streamID, profileID string, jobGeneration uint64, reason string, target error) error {
	m.report(ctx, streamID, "worker.caption.runtime_update_failed", "failed", map[string]any{
		"caption_profile_id": profileID,
		"job_generation":     jobGeneration,
		"reason":             reason,
	})
	return target
}

func closeCaptionSession(session CaptionSession) {
	if session != nil {
		_ = session.Close(context.Background())
	}
}

func zeroBytes(value []byte) {
	for i := range value {
		value[i] = 0
	}
}
