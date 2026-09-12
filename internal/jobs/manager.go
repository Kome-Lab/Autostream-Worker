package jobs

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/example/autostream-worker/internal/control"
	"github.com/example/autostream-worker/internal/encoder"
	"github.com/example/autostream-worker/internal/events"
	"github.com/example/autostream-worker/internal/sceneappearance"
)

var (
	ErrCaptionNotConfigured                     = errors.New("caption transcription is not configured")
	ErrCaptionProfileInvalid                    = errors.New("caption profile is invalid")
	ErrCaptionRuntimeUnavailable                = errors.New("caption transcription runtime is unavailable")
	ErrCaptionAudioUnavailable                  = errors.New("caption audio transcription is unavailable")
	ErrCaptionAudioQueueFull                    = errors.New("caption audio queue is full")
	ErrCaptionAudioGenerationRequired           = errors.New("caption audio job generation is required")
	ErrCaptionAudioConnectionGenerationRequired = errors.New("caption audio connection generation is required")
	ErrCaptionAudioConnectionGenerationStale    = errors.New("caption audio connection generation is stale")
	ErrCaptionAudioPayloadInvalid               = errors.New("caption audio payload is invalid")
	ErrStreamAlreadyStopped                     = errors.New("stream job is already stopped")
	ErrNoActiveStreamJob                        = errors.New("no active stream job")
	ErrStreamIDDoesNotMatchJob                  = errors.New("stream_id does not match current job")
	ErrJobGenerationMismatch                    = errors.New("job generation does not match current job")
	ErrCaptionSessionGenerationMismatch         = errors.New("caption session generation does not match current session")
	ErrStreamStopping                           = errors.New("stream job is stopping")
	ErrStoppedTargetReceiptUnavailable          = errors.New("stopped target receipt is unavailable")
	ErrVideoOutputUnavailable                   = errors.New("worker video output is unavailable")
	ErrLegacyDiscordChatFields                  = errors.New("discord chat payload contains legacy fields")
)

const (
	maxStoppedStreamTargets           = 64
	captionAudioQueueBatches          = 128
	captionAudioQueuePackets          = 1024
	captionAudioQueueBytes            = 8 << 20
	captionAudioBatchTimeout          = 15 * time.Second
	captionAudioRetryMax              = 3
	captionAudioRetryBase             = 250 * time.Millisecond
	captionDiagnosticTimeout          = 2 * time.Second
	captionAudioMaxUserIDBytes        = 128
	captionAudioPacketAccountingBytes = 128
)

type StreamContext struct {
	StreamID              string                    `json:"stream_id"`
	StreamName            string                    `json:"stream_name,omitempty"`
	EncoderRecorderURL    string                    `json:"encoder_recorder_url,omitempty"`
	StreamIngestToken     string                    `json:"stream_ingest_token,omitempty"`
	OverlayProfileID      string                    `json:"overlay_profile_id,omitempty"`
	CaptionProfileID      string                    `json:"caption_profile_id,omitempty"`
	SceneAppearance       *sceneappearance.Snapshot `json:"scene_appearance,omitempty"`
	EncoderProfileID      string                    `json:"encoder_profile_id,omitempty"`
	VideoWidth            int                       `json:"video_width,omitempty"`
	VideoHeight           int                       `json:"video_height,omitempty"`
	VideoFPS              int                       `json:"video_fps,omitempty"`
	VideoIngestURL        string                    `json:"video_ingest_url,omitempty"`
	VideoIngestPassphrase string                    `json:"video_ingest_passphrase,omitempty"`
	VideoIngestPBKeylen   int                       `json:"video_ingest_pbkeylen,omitempty"`
}

type Manager struct {
	publisher encoder.Publisher
	reporter  Reporter

	lifecycleMu sync.Mutex
	mu          sync.Mutex
	current     StreamContext
	defaults    ProfileDefaults
	assignments AssignmentPolicy

	captionProfiles          map[string]control.RuntimeProfile
	secretResolver           RuntimeSecretResolver
	captionFactory           CaptionSessionFactory
	captionSession           CaptionSession
	captionIngress           *captionAudioIngress
	captionSessionGeneration uint64
	sceneRenderer            SceneRenderer
	sceneAppearanceRuntime   SceneAppearanceRuntime
	sceneVideo               VideoSceneConfig
	videoOutput              VideoOutput
	jobGeneration            uint64
	stopping                 bool
	deliveryCtx              context.Context
	deliveryCancel           context.CancelFunc
	deliveryWake             chan struct{}
	deliveryWG               sync.WaitGroup
	pendingEvents            map[string]pendingWorkerEvent
	latestEventByKey         map[string]string
	publisherMu              sync.Mutex

	startedAt                        time.Time
	stoppedOrder                     []stoppedTargetReceipt
	stoppedTargetReceiptPath         string
	events                           []events.OverlayEvent
	eventCounts                      map[string]int
	sendFailures                     int
	captionAudioStartedGeneration    uint64
	captionAudioConnectionGeneration uint64
	captionAudioQueueDrops           int
	captionAudioRetries              int
	captionAudioProviderDrops        int
	captionAudioSupersededDrops      int
	maxEvents                        int
}

func NewManager(publisher encoder.Publisher, reporter Reporter) *Manager {
	return newManager(publisher, reporter, "")
}

// NewManagerWithStoppedTargetReceiptFile retains a bounded set of recently
// stopped stream IDs across process restarts. A delayed stop request can then
// be safely acknowledged without touching a successor stream.
func NewManagerWithStoppedTargetReceiptFile(publisher encoder.Publisher, reporter Reporter, path string) (*Manager, error) {
	manager := newManager(publisher, reporter, path)
	if err := manager.loadStoppedTargetReceipts(); err != nil {
		return nil, err
	}
	return manager, nil
}

func newManager(publisher encoder.Publisher, reporter Reporter, stoppedTargetReceiptPath string) *Manager {
	if publisher == nil {
		publisher = encoder.NoopPublisher{}
	}
	return &Manager{
		publisher:                publisher,
		reporter:                 reporter,
		captionProfiles:          map[string]control.RuntimeProfile{},
		captionFactory:           deepgramSessionFactory{},
		stoppedTargetReceiptPath: strings.TrimSpace(stoppedTargetReceiptPath),
		eventCounts:              map[string]int{},
		maxEvents:                200,
		pendingEvents:            map[string]pendingWorkerEvent{},
		latestEventByKey:         map[string]string{},
	}
}

func (m *Manager) Start(ctx context.Context, stream StreamContext) error {
	m.lifecycleMu.Lock()
	defer m.lifecycleMu.Unlock()

	stream = m.applyStartProfileDefaults(stream)
	if strings.TrimSpace(stream.StreamID) == "" {
		return errors.New("stream_id is required")
	}
	stream.StreamID = strings.TrimSpace(stream.StreamID)
	stream.CaptionProfileID = strings.TrimSpace(stream.CaptionProfileID)
	videoConfig, err := normalizeVideoSceneConfig(stream.VideoWidth, stream.VideoHeight, stream.VideoFPS)
	if err != nil {
		return err
	}
	if err := validateVideoOutputRequest(stream); err != nil {
		return err
	}

	m.mu.Lock()
	if !m.streamAssignedLocked(stream.StreamID) {
		m.mu.Unlock()
		return errors.New("stream is not assigned to this worker service as primary")
	}
	if m.current.StreamID != "" {
		m.mu.Unlock()
		return errors.New("a stream job is already active")
	}
	profile, profileSelected := m.captionProfiles[stream.CaptionProfileID]
	resolver := m.secretResolver
	factory := m.captionFactory
	appearanceRuntime := m.sceneAppearanceRuntime
	_, appearanceRendererAvailable := m.sceneRenderer.(sceneAppearanceConfigurer)
	nextGeneration := m.jobGeneration + 1
	nextCaptionSessionGeneration := m.captionSessionGeneration + 1
	m.mu.Unlock()

	var preparedAppearance sceneappearance.Prepared
	if stream.SceneAppearance != nil {
		if appearanceRuntime == nil || !appearanceRendererAvailable {
			return sceneappearance.NewError(sceneappearance.CodeCapabilityRequired)
		}
		preparedAppearance, err = appearanceRuntime.Prepare(ctx, stream.StreamID, stream.SceneAppearance)
		if err != nil {
			if _, ok := sceneappearance.CodeOf(err); !ok {
				err = sceneappearance.NewError(sceneappearance.CodeMediaAssetVariantFailed)
			}
			return err
		}
	}

	var captionSession CaptionSession
	displayConfig := defaultCaptionDisplayConfig()
	if stream.CaptionProfileID != "" {
		if !profileSelected {
			return m.captionStartFailed(ctx, stream.StreamID, stream.CaptionProfileID, "profile_not_found", ErrCaptionProfileInvalid)
		}
		config, secretName, err := captionConfig(profile)
		if err != nil {
			return m.captionStartFailed(ctx, stream.StreamID, stream.CaptionProfileID, "profile_invalid", ErrCaptionProfileInvalid)
		}
		var displayOK bool
		displayConfig, displayOK = captionDisplayConfigFromProfile(profile.Config)
		if !displayOK {
			return m.captionStartFailed(ctx, stream.StreamID, stream.CaptionProfileID, "profile_invalid", ErrCaptionProfileInvalid)
		}
		if resolver == nil || factory == nil {
			return m.captionStartFailed(ctx, stream.StreamID, stream.CaptionProfileID, "runtime_unavailable", ErrCaptionRuntimeUnavailable)
		}
		secret, err := resolver.ResolveRuntimeSecret(ctx, stream.StreamID, secretName)
		if err != nil {
			return m.captionStartFailed(ctx, stream.StreamID, stream.CaptionProfileID, "secret_resolve_failed", ErrCaptionRuntimeUnavailable)
		}
		if secret.SecretName != secretName || strings.TrimSpace(secret.Value) == "" || secret.ExpiresInSec <= 0 {
			secret.Value = ""
			return m.captionStartFailed(ctx, stream.StreamID, stream.CaptionProfileID, "secret_invalid", ErrCaptionRuntimeUnavailable)
		}
		apiKey := []byte(secret.Value)
		secret.Value = ""
		captionSession, err = factory.New(config, apiKey, m.captionTranscriptHandler(stream.StreamID, nextGeneration, nextCaptionSessionGeneration))
		zeroBytes(apiKey)
		if err != nil || captionSession == nil {
			closeCaptionSession(captionSession)
			return m.captionStartFailed(ctx, stream.StreamID, stream.CaptionProfileID, "session_create_failed", ErrCaptionRuntimeUnavailable)
		}
	}

	m.mu.Lock()
	if !m.streamAssignedLocked(stream.StreamID) {
		m.mu.Unlock()
		closeCaptionSession(captionSession)
		return errors.New("stream is not assigned to this worker service as primary")
	}
	if m.current.StreamID != "" {
		m.mu.Unlock()
		closeCaptionSession(captionSession)
		return errors.New("a stream job is already active")
	}
	if err := m.forgetStoppedTargetLocked(stream.StreamID); err != nil {
		m.mu.Unlock()
		closeCaptionSession(captionSession)
		return fmt.Errorf("%w: %v", ErrStoppedTargetReceiptUnavailable, err)
	}
	m.current = stream
	m.jobGeneration++
	generation := m.jobGeneration
	m.captionSessionGeneration = nextCaptionSessionGeneration
	m.startEventDeliveryLocked()
	m.captionSession = captionSession
	m.sceneVideo = videoConfig
	sceneRenderer := m.sceneRenderer
	appearanceConfigurer, appearanceRendererAvailable := sceneRenderer.(sceneAppearanceConfigurer)
	videoOutput := m.videoOutput
	videoRequested := videoOutputRequested(stream)
	if stream.SceneAppearance != nil && !appearanceRendererAvailable {
		m.current = StreamContext{}
		m.captionSession = nil
		m.sceneVideo = VideoSceneConfig{}
		m.mu.Unlock()
		m.stopEventDelivery()
		closeCaptionSession(captionSession)
		return sceneappearance.NewError(sceneappearance.CodeCapabilityRequired)
	}
	if videoRequested && (sceneRenderer == nil || videoOutput == nil) {
		m.current = StreamContext{}
		m.captionSession = nil
		m.sceneVideo = VideoSceneConfig{}
		m.mu.Unlock()
		m.stopEventDelivery()
		closeCaptionSession(captionSession)
		return ErrVideoOutputUnavailable
	}
	var captionIngress *captionAudioIngress
	if sceneRenderer != nil {
		sceneRenderer.ConfigureDisplay(displayConfig.maxItems, displayConfig.reorderWindow, displayConfig.interimTTL, displayConfig.finalTTL, displayConfig.showVoiceTranscripts)
	}
	if sceneRenderer != nil {
		sceneRenderer.Reset(generation, stream.StreamID, stream.StreamName)
	}
	if stream.SceneAppearance != nil {
		if err := appearanceConfigurer.ConfigureAppearance(preparedAppearance); err != nil {
			if _, ok := sceneappearance.CodeOf(err); !ok {
				err = sceneappearance.NewError(sceneappearance.CodeMediaAssetVariantFailed)
			}
			m.current = StreamContext{}
			m.captionSession = nil
			m.captionIngress = nil
			m.sceneVideo = VideoSceneConfig{}
			m.mu.Unlock()
			m.stopEventDelivery()
			sceneRenderer.Clear(stream.StreamID)
			closeCaptionSession(captionSession)
			return err
		}
	}
	if captionSession != nil {
		captionIngress = newCaptionAudioIngress()
		m.captionIngress = captionIngress
	}
	m.startedAt = time.Now().UTC()
	m.events = nil
	m.eventCounts = map[string]int{}
	m.sendFailures = 0
	m.captionAudioStartedGeneration = 0
	m.captionAudioConnectionGeneration = 0
	m.captionAudioQueueDrops = 0
	m.captionAudioRetries = 0
	m.captionAudioProviderDrops = 0
	m.captionAudioSupersededDrops = 0
	m.mu.Unlock()
	if captionIngress != nil {
		go m.runCaptionAudioIngress(captionIngress)
	}
	if videoRequested {
		videoStartConfig := videoConfig
		videoStartConfig.Generation = generation
		if err := videoOutput.Start(ctx, stream, videoStartConfig); err != nil {
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cleanupCancel()
			_ = videoOutput.Stop(cleanupCtx, stream.StreamID)
			m.mu.Lock()
			if m.current.StreamID == stream.StreamID {
				m.current = StreamContext{}
				m.captionSession = nil
				m.captionIngress = nil
				m.captionAudioConnectionGeneration = 0
				m.sceneVideo = VideoSceneConfig{}
				m.startedAt = time.Time{}
				m.events = nil
				m.eventCounts = map[string]int{}
				m.sendFailures = 0
			}
			m.mu.Unlock()
			m.stopEventDelivery()
			stopCaptionAudioIngress(cleanupCtx, captionIngress)
			sceneRenderer.Clear(stream.StreamID)
			if captionSession != nil {
				_ = captionSession.Close(cleanupCtx)
			}
			m.report(ctx, stream.StreamID, "worker.video.start_failed", "failed", nil)
			return ErrVideoOutputUnavailable
		}
	}
	m.report(ctx, stream.StreamID, "worker.job.started", "running", nil)
	return nil
}

func (m *Manager) Stop(ctx context.Context, streamID string) error {
	m.lifecycleMu.Lock()
	defer m.lifecycleMu.Unlock()

	streamID = strings.TrimSpace(streamID)
	m.mu.Lock()
	if m.current.StreamID == "" {
		if streamID != "" && m.wasStoppedTargetLocked(streamID) {
			m.mu.Unlock()
			return ErrStreamAlreadyStopped
		}
		m.mu.Unlock()
		return ErrNoActiveStreamJob
	}
	if streamID != "" && streamID != m.current.StreamID {
		if m.wasStoppedTargetLocked(streamID) {
			m.mu.Unlock()
			return ErrStreamAlreadyStopped
		}
		m.mu.Unlock()
		return ErrStreamIDDoesNotMatchJob
	}
	stoppedStreamID := m.current.StreamID
	captionSession := m.captionSession
	captionIngress := m.captionIngress
	videoOutput := m.videoOutput
	videoRequested := videoOutputRequested(m.current)
	m.captionSession = nil
	m.captionIngress = nil
	m.captionSessionGeneration++
	m.captionAudioConnectionGeneration = 0
	// Mark the stop boundary before waiting for downstream stop operations. The
	// current stream is retained until its durable stop receipt is written so a
	// persistence failure preserves the existing retryable Stop semantics, but
	// all new events are rejected while this fence is active.
	m.stopping = true
	m.jobGeneration++
	m.mu.Unlock()
	m.stopEventDelivery()
	stopCaptionAudioIngress(ctx, captionIngress)

	if videoRequested && videoOutput != nil {
		if err := videoOutput.Stop(ctx, stoppedStreamID); err != nil {
			m.report(ctx, stoppedStreamID, "worker.video.stop_failed", "failed", nil)
		}
	}

	if captionSession != nil {
		if err := captionSession.Close(ctx); err != nil {
			m.report(ctx, stoppedStreamID, "worker.caption.stop_failed", "failed", nil)
		}
	}

	m.mu.Lock()
	if err := m.rememberStoppedTargetLocked(stoppedStreamID); err != nil {
		m.mu.Unlock()
		return fmt.Errorf("%w: %v", ErrStoppedTargetReceiptUnavailable, err)
	}
	m.current = StreamContext{}
	m.sceneVideo = VideoSceneConfig{}
	m.startedAt = time.Time{}
	m.stopping = false
	sceneRenderer := m.sceneRenderer
	m.mu.Unlock()
	if sceneRenderer != nil {
		sceneRenderer.Clear(stoppedStreamID)
	}
	m.report(ctx, stoppedStreamID, "worker.job.stopped", "stopped", nil)
	return nil
}

func (m *Manager) Close(ctx context.Context) error {
	streamID := m.CurrentStreamID()
	if streamID != "" {
		return m.Stop(ctx, streamID)
	}
	return nil
}

func (m *Manager) CurrentStreamID() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.current.StreamID
}

func (m *Manager) ensureStreamLocked(streamID string) error {
	if m.current.StreamID == "" {
		return errors.New("no active stream job")
	}
	if m.stopping {
		return ErrStreamStopping
	}
	if streamID == "" {
		return errors.New("stream_id is required")
	}
	if streamID != m.current.StreamID {
		return errors.New("stream_id does not match current job")
	}
	return nil
}
