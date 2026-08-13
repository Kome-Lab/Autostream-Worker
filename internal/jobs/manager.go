package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/example/autostream-worker/internal/control"
	"github.com/example/autostream-worker/internal/deepgram"
	"github.com/example/autostream-worker/internal/encoder"
	"github.com/example/autostream-worker/internal/events"
)

var (
	ErrCaptionNotConfigured            = errors.New("caption transcription is not configured")
	ErrCaptionProfileInvalid           = errors.New("caption profile is invalid")
	ErrCaptionRuntimeUnavailable       = errors.New("caption transcription runtime is unavailable")
	ErrCaptionAudioUnavailable         = errors.New("caption audio transcription is unavailable")
	ErrStreamAlreadyStopped            = errors.New("stream job is already stopped")
	ErrNoActiveStreamJob               = errors.New("no active stream job")
	ErrStreamIDDoesNotMatchJob         = errors.New("stream_id does not match current job")
	ErrJobGenerationMismatch           = errors.New("job generation does not match current job")
	ErrStreamStopping                  = errors.New("stream job is stopping")
	ErrStoppedTargetReceiptUnavailable = errors.New("stopped target receipt is unavailable")
	ErrVideoOutputUnavailable          = errors.New("worker video output is unavailable")
)

const maxStoppedStreamTargets = 64

type StreamContext struct {
	StreamID              string `json:"stream_id"`
	StreamName            string `json:"stream_name,omitempty"`
	EncoderRecorderURL    string `json:"encoder_recorder_url,omitempty"`
	StreamIngestToken     string `json:"stream_ingest_token,omitempty"`
	OverlayProfileID      string `json:"overlay_profile_id,omitempty"`
	CaptionProfileID      string `json:"caption_profile_id,omitempty"`
	EncoderProfileID      string `json:"encoder_profile_id,omitempty"`
	VideoWidth            int    `json:"video_width,omitempty"`
	VideoHeight           int    `json:"video_height,omitempty"`
	VideoFPS              int    `json:"video_fps,omitempty"`
	VideoIngestURL        string `json:"video_ingest_url,omitempty"`
	VideoIngestPassphrase string `json:"video_ingest_passphrase,omitempty"`
	VideoIngestPBKeylen   int    `json:"video_ingest_pbkeylen,omitempty"`
}

type VideoSceneConfig struct {
	Width      int    `json:"width"`
	Height     int    `json:"height"`
	FPS        int    `json:"fps"`
	Generation uint64 `json:"-"`
}

type SceneRenderer interface {
	Reset(generation uint64, streamID, streamName string)
	Clear(streamID string)
	Apply(generation uint64, event events.OverlayEvent) error
	RenderSize(width, height int, at time.Time) (*image.RGBA, error)
	AvatarRefreshInterval() time.Duration
	RefreshAvatars()
}

// sceneDisplayConfigurer is optional so existing renderers remain compatible
// while the built-in scene can apply the selected caption profile's display
// policy at the same job boundary as its Deepgram settings.
type sceneDisplayConfigurer interface {
	ConfigureDisplay(maxItems int, reorderWindow, interimTTL, finalTTL time.Duration, showVoiceTranscripts, showLegacyCaptionBar bool)
}

type captionDisplayConfig struct {
	maxItems             int
	reorderWindow        time.Duration
	interimTTL           time.Duration
	finalTTL             time.Duration
	showVoiceTranscripts bool
	showLegacyCaptionBar bool
}

type VideoOutput interface {
	Start(context.Context, StreamContext, VideoSceneConfig) error
	Stop(context.Context, string) error
}

type ProfileDefaults struct {
	OverlayProfileID string
	CaptionProfileID string
}

type AssignmentPolicy struct {
	Enforce        bool
	PrimaryStreams map[string]bool
}

type Status struct {
	CurrentStreamID string         `json:"current_stream_id,omitempty"`
	StreamName      string         `json:"stream_name,omitempty"`
	JobGeneration   uint64         `json:"job_generation,omitempty"`
	StartedAt       time.Time      `json:"started_at,omitempty"`
	EventCount      int            `json:"event_count"`
	EventCounts     map[string]int `json:"event_counts,omitempty"`
	SendFailures    int            `json:"event_send_failures_total"`
	LastEventAt     time.Time      `json:"last_event_at,omitempty"`
}

type Manager struct {
	publisher encoder.Publisher
	reporter  Reporter

	lifecycleMu sync.Mutex
	mu          sync.Mutex
	current     StreamContext
	defaults    ProfileDefaults
	assignments AssignmentPolicy

	captionProfiles  map[string]control.RuntimeProfile
	secretResolver   RuntimeSecretResolver
	captionFactory   CaptionSessionFactory
	captionSession   CaptionSession
	sceneRenderer    SceneRenderer
	sceneVideo       VideoSceneConfig
	videoOutput      VideoOutput
	jobGeneration    uint64
	stopping         bool
	deliveryCtx      context.Context
	deliveryCancel   context.CancelFunc
	deliveryWake     chan struct{}
	deliveryWG       sync.WaitGroup
	pendingEvents    map[string]pendingWorkerEvent
	latestEventByKey map[string]string
	publisherMu      sync.Mutex

	startedAt                time.Time
	stoppedOrder             []stoppedTargetReceipt
	stoppedTargetReceiptPath string
	events                   []events.OverlayEvent
	eventCounts              map[string]int
	sendFailures             int
	maxEvents                int
}

type Reporter interface {
	Event(ctx context.Context, streamID, name, status string, attributes map[string]any) error
	Metric(ctx context.Context, streamID, name, status string, value float64, attributes map[string]any) error
}

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
	nextGeneration := m.jobGeneration + 1
	m.mu.Unlock()

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
		captionSession, err = factory.New(config, apiKey, func(resultCtx context.Context, transcript deepgram.Transcript) error {
			_, publishErr := m.CaptionTranscriptDetailedForGeneration(resultCtx, stream.StreamID, nextGeneration, transcript)
			return publishErr
		})
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
	m.startEventDeliveryLocked()
	m.captionSession = captionSession
	m.sceneVideo = videoConfig
	sceneRenderer := m.sceneRenderer
	videoOutput := m.videoOutput
	videoRequested := videoOutputRequested(stream)
	if videoRequested && (sceneRenderer == nil || videoOutput == nil) {
		m.current = StreamContext{}
		m.captionSession = nil
		m.sceneVideo = VideoSceneConfig{}
		m.mu.Unlock()
		m.stopEventDelivery()
		closeCaptionSession(captionSession)
		return ErrVideoOutputUnavailable
	}
	if configurer, ok := sceneRenderer.(sceneDisplayConfigurer); ok {
		configurer.ConfigureDisplay(displayConfig.maxItems, displayConfig.reorderWindow, displayConfig.interimTTL, displayConfig.finalTTL, displayConfig.showVoiceTranscripts, displayConfig.showLegacyCaptionBar)
	}
	if sceneRenderer != nil {
		sceneRenderer.Reset(generation, stream.StreamID, stream.StreamName)
	}
	m.startedAt = time.Now().UTC()
	m.events = nil
	m.eventCounts = map[string]int{}
	m.sendFailures = 0
	m.mu.Unlock()
	if videoRequested {
		videoStartConfig := videoConfig
		videoStartConfig.Generation = generation
		if err := videoOutput.Start(ctx, stream, videoStartConfig); err != nil {
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 3*time.Second)
			_ = videoOutput.Stop(cleanupCtx, stream.StreamID)
			cleanupCancel()
			m.mu.Lock()
			if m.current.StreamID == stream.StreamID {
				m.current = StreamContext{}
				m.captionSession = nil
				m.sceneVideo = VideoSceneConfig{}
				m.startedAt = time.Time{}
				m.events = nil
				m.eventCounts = map[string]int{}
				m.sendFailures = 0
			}
			m.mu.Unlock()
			m.stopEventDelivery()
			sceneRenderer.Clear(stream.StreamID)
			closeCaptionSession(captionSession)
			m.report(ctx, stream.StreamID, "worker.video.start_failed", "failed", nil)
			return ErrVideoOutputUnavailable
		}
	}
	m.report(ctx, stream.StreamID, "worker.job.started", "running", nil)
	return nil
}

func (m *Manager) SetAssignmentPolicy(policy AssignmentPolicy) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.assignments = AssignmentPolicy{Enforce: policy.Enforce, PrimaryStreams: map[string]bool{}}
	for streamID, allowed := range policy.PrimaryStreams {
		streamID = strings.TrimSpace(streamID)
		if streamID != "" && allowed {
			m.assignments.PrimaryStreams[streamID] = true
		}
	}
}

func (m *Manager) SetProfileDefaults(defaults ProfileDefaults) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.defaults = ProfileDefaults{
		OverlayProfileID: strings.TrimSpace(defaults.OverlayProfileID),
		CaptionProfileID: strings.TrimSpace(defaults.CaptionProfileID),
	}
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

func (m *Manager) SetSceneRenderer(renderer SceneRenderer) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sceneRenderer = renderer
}

func (m *Manager) SetVideoOutput(output VideoOutput) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.videoOutput = output
}

// HandleVideoOutputFailure fails the matching active job closed after its
// already-started video transport exits unexpectedly. The generation fence is
// required because a stream ID may be reused after a stop/rearm cycle.
func (m *Manager) HandleVideoOutputFailure(streamID string, generation uint64, errorClass string) {
	streamID = strings.TrimSpace(streamID)
	if streamID == "" || generation == 0 {
		return
	}

	m.lifecycleMu.Lock()
	m.mu.Lock()
	if m.current.StreamID != streamID || m.jobGeneration != generation || !videoOutputRequested(m.current) {
		m.mu.Unlock()
		m.lifecycleMu.Unlock()
		return
	}
	receiptErr := m.rememberStoppedTargetLocked(streamID)
	if receiptErr != nil {
		// Durability is uncertain, but the running process must still answer the
		// Control Panel's immediate stop convergence idempotently.
		m.rememberStoppedTargetInMemoryLocked(streamID, time.Now().UTC())
	}
	captionSession := m.captionSession
	sceneRenderer := m.sceneRenderer
	m.current = StreamContext{}
	m.captionSession = nil
	m.sceneVideo = VideoSceneConfig{}
	m.startedAt = time.Time{}
	m.jobGeneration++
	m.mu.Unlock()
	m.stopEventDelivery()

	if sceneRenderer != nil {
		sceneRenderer.Clear(streamID)
	}
	m.lifecycleMu.Unlock()

	failureCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	errorClass = normalizeVideoOutputErrorClass(errorClass)
	attributes := map[string]any{"reason": "transport_stopped", "error_class": errorClass}
	if receiptErr != nil {
		attributes["stopped_target_receipt"] = "unavailable"
	}
	m.report(failureCtx, streamID, "worker.video.output_failed", "failed", attributes)
	if captionSession != nil {
		if err := captionSession.Close(failureCtx); err != nil {
			m.report(failureCtx, streamID, "worker.caption.stop_failed", "failed", nil)
		}
	}
}

func normalizeVideoOutputErrorClass(value string) string {
	switch strings.TrimSpace(value) {
	case "srt_write", "scene_render", "frame_shape", "jpeg_encode", "jpeg_size", "transport_stopped":
		return strings.TrimSpace(value)
	default:
		return "unknown"
	}
}

func (m *Manager) ApplyRuntimeConfig(cfg control.RuntimeConfig) {
	m.SetProfileDefaults(ProfileDefaultsFromRuntimeConfig(cfg))
	m.SetAssignmentPolicy(AssignmentPolicyFromRuntimeConfig(cfg))
	m.mu.Lock()
	m.captionProfiles = captionProfilesFromRuntimeConfig(cfg)
	m.mu.Unlock()
}

func ProfileDefaultsFromRuntimeConfig(cfg control.RuntimeConfig) ProfileDefaults {
	defaults := ProfileDefaults{}
	if profile, ok := firstRuntimeProfileForService(cfg.Profiles["overlay"], cfg.Service.ServiceID); ok {
		defaults.OverlayProfileID = profile.ID
	}
	if profile, ok := firstRuntimeProfileForService(cfg.Profiles["caption"], cfg.Service.ServiceID); ok {
		defaults.CaptionProfileID = profile.ID
	}
	return defaults
}

func captionProfilesFromRuntimeConfig(cfg control.RuntimeConfig) map[string]control.RuntimeProfile {
	profiles := map[string]control.RuntimeProfile{}
	for _, profile := range cfg.Profiles["caption"] {
		profile.ID = strings.TrimSpace(profile.ID)
		if profile.ID == "" || (profile.Kind != "" && profile.Kind != "caption") || !profileBelongsToService(profile, cfg.Service.ServiceID) {
			continue
		}
		profile.Config = cloneConfig(profile.Config)
		profiles[profile.ID] = profile
	}
	return profiles
}

func AssignmentPolicyFromRuntimeConfig(cfg control.RuntimeConfig) AssignmentPolicy {
	policy := AssignmentPolicy{Enforce: true, PrimaryStreams: map[string]bool{}}
	serviceID := strings.TrimSpace(cfg.Service.ServiceID)
	for _, assignment := range cfg.Assignments {
		if assignment.ServiceType != control.ServiceType {
			continue
		}
		if strings.TrimSpace(assignment.ServiceID) != serviceID {
			continue
		}
		if assignment.AssignmentRole != "primary" {
			continue
		}
		streamID := strings.TrimSpace(assignment.StreamID)
		if streamID != "" {
			policy.PrimaryStreams[streamID] = true
		}
	}
	return policy
}

func firstRuntimeProfileForService(profiles []control.RuntimeProfile, serviceID string) (control.RuntimeProfile, bool) {
	for _, profile := range profiles {
		if profileBelongsToService(profile, serviceID) {
			return profile, true
		}
	}
	return control.RuntimeProfile{}, false
}

func profileBelongsToService(profile control.RuntimeProfile, serviceID string) bool {
	rawServiceID, ok := profile.Config["service_id"]
	if !ok {
		return true
	}
	profileServiceID, ok := rawServiceID.(string)
	if !ok {
		return false
	}
	profileServiceID = strings.TrimSpace(profileServiceID)
	return profileServiceID == "" || profileServiceID == strings.TrimSpace(serviceID)
}

func captionConfig(profile control.RuntimeProfile) (deepgram.Config, string, error) {
	provider, providerOK := stringConfig(profile.Config, "provider")
	model, modelOK := stringConfig(profile.Config, "model")
	language, languageOK := stringConfig(profile.Config, "language")
	secretName, secretOK := stringConfig(profile.Config, "api_key_secret_name")
	endpointingMS, endpointingOK := integerConfig(profile.Config, "endpointing_ms")
	if !endpointingOK {
		endpointingMS, endpointingOK = 300, true
	}
	delayMS, delayOK := integerConfig(profile.Config, "delay_ms")
	if !delayOK {
		delayMS, delayOK = 800, true
	}
	interimResults, interimOK := booleanConfig(profile.Config, "interim_results")
	if !interimOK {
		interimResults, interimOK = true, true
	}
	smartFormat, smartFormatOK := booleanConfig(profile.Config, "smart_format")
	if !smartFormatOK {
		smartFormat, smartFormatOK = true, true
	}
	utteranceEndMS, utteranceEndOK := integerConfigDefaultBounded(profile.Config, "utterance_end_ms", 1000, 100, 10000)
	localFinalizeMS, localFinalizeOK := integerConfigDefaultBounded(profile.Config, "local_finalize_ms", 1500, 0, 10000)
	speakerIdleCloseSeconds, speakerIdleCloseOK := integerConfigDefaultBounded(profile.Config, "speaker_idle_close_seconds", 8, 1, 120)
	keepAliveSeconds, keepAliveOK := integerConfigDefaultBounded(profile.Config, "keepalive_interval_seconds", 4, 1, 60)
	replayBufferMaxMS, replayBufferMaxOK := integerConfigDefaultBounded(profile.Config, "replay_buffer_max_ms", 2000, 0, 10000)
	if !providerOK || !strings.EqualFold(provider, "deepgram") || !modelOK || model != "nova-3" ||
		!languageOK || language == "" || !secretOK || secretName != "deepgram_api_key" ||
		!endpointingOK || endpointingMS < 10 || endpointingMS > 5000 ||
		!delayOK || delayMS < 0 || delayMS > 10000 || !interimOK || !smartFormatOK ||
		!utteranceEndOK || !localFinalizeOK || !speakerIdleCloseOK || !keepAliveOK || !replayBufferMaxOK {
		return deepgram.Config{}, "", ErrCaptionProfileInvalid
	}
	config := deepgram.Config{
		Model:             model,
		Language:          language,
		EndpointingMS:     endpointingMS,
		UtteranceEndMS:    utteranceEndMS,
		LocalFinalize:     time.Duration(localFinalizeMS) * time.Millisecond,
		SpeakerIdleClose:  time.Duration(speakerIdleCloseSeconds) * time.Second,
		KeepAliveInterval: time.Duration(keepAliveSeconds) * time.Second,
		InterimResults:    interimResults,
		SmartFormat:       smartFormat,
		Keyterms:          stringListConfig(profile.Config, "keyterms"),
		MIPOptOut:         booleanConfigDefault(profile.Config, "mip_opt_out", false),
		ReplayBufferMax:   time.Duration(replayBufferMaxMS) * time.Millisecond,
		Delay:             time.Duration(delayMS) * time.Millisecond,
	}
	if _, err := deepgram.ListenURL(config); err != nil {
		return deepgram.Config{}, "", ErrCaptionProfileInvalid
	}
	return config, secretName, nil
}

func defaultCaptionDisplayConfig() captionDisplayConfig {
	return captionDisplayConfig{
		maxItems:             12,
		reorderWindow:        500 * time.Millisecond,
		interimTTL:           6 * time.Second,
		finalTTL:             15 * time.Second,
		showVoiceTranscripts: true,
	}
}

func captionDisplayConfigFromProfile(config map[string]any) (captionDisplayConfig, bool) {
	display := defaultCaptionDisplayConfig()
	var ok bool
	if display.maxItems, ok = integerConfigDefaultBounded(config, "conversation_max_items", display.maxItems, 1, 64); !ok {
		return captionDisplayConfig{}, false
	}
	var reorderWindowMS int
	if reorderWindowMS, ok = integerConfigDefaultBounded(config, "conversation_reorder_window_ms", 500, 0, 5000); !ok {
		return captionDisplayConfig{}, false
	}
	var interimTTLSeconds int
	if interimTTLSeconds, ok = integerConfigDefaultBounded(config, "voice_interim_ttl_seconds", 6, 1, 60); !ok {
		return captionDisplayConfig{}, false
	}
	var finalTTLSeconds int
	if finalTTLSeconds, ok = integerConfigDefaultBounded(config, "voice_final_ttl_seconds", 15, 1, 300); !ok {
		return captionDisplayConfig{}, false
	}
	display.reorderWindow = time.Duration(reorderWindowMS) * time.Millisecond
	display.interimTTL = time.Duration(interimTTLSeconds) * time.Second
	display.finalTTL = time.Duration(finalTTLSeconds) * time.Second
	display.showVoiceTranscripts = booleanConfigDefault(config, "show_voice_transcripts", true)
	display.showLegacyCaptionBar = booleanConfigDefault(config, "show_legacy_caption_bar", false)
	return display, true
}

func stringConfig(config map[string]any, key string) (string, bool) {
	value, ok := config[key].(string)
	if !ok {
		return "", false
	}
	value = strings.TrimSpace(value)
	return value, value != ""
}

func booleanConfig(config map[string]any, key string) (bool, bool) {
	value, ok := config[key].(bool)
	return value, ok
}

func integerConfig(config map[string]any, key string) (int, bool) {
	value, ok := config[key]
	if !ok {
		return 0, false
	}
	switch typed := value.(type) {
	case int:
		return typed, true
	case int32:
		return int(typed), true
	case int64:
		if typed < -1<<31 || typed > 1<<31-1 {
			return 0, false
		}
		return int(typed), true
	case uint:
		if uint64(typed) > 1<<31-1 {
			return 0, false
		}
		return int(typed), true
	case uint32:
		if typed > 1<<31-1 {
			return 0, false
		}
		return int(typed), true
	case uint64:
		if typed > 1<<31-1 {
			return 0, false
		}
		return int(typed), true
	case float64:
		if math.Trunc(typed) != typed || typed < -1<<31 || typed > 1<<31-1 {
			return 0, false
		}
		return int(typed), true
	case json.Number:
		parsed, err := typed.Int64()
		if err != nil || parsed < -1<<31 || parsed > 1<<31-1 {
			return 0, false
		}
		return int(parsed), true
	default:
		return 0, false
	}
}

func integerConfigDefaultBounded(config map[string]any, key string, fallback, min, max int) (int, bool) {
	if value, ok := integerConfig(config, key); ok {
		if value < min || value > max {
			return 0, false
		}
		return value, true
	}
	return fallback, true
}

func booleanConfigDefault(config map[string]any, key string, fallback bool) bool {
	if value, ok := booleanConfig(config, key); ok {
		return value
	}
	return fallback
}

func stringListConfig(config map[string]any, key string) []string {
	value, ok := config[key]
	if !ok {
		return nil
	}
	var values []string
	switch typed := value.(type) {
	case []string:
		values = append(values, typed...)
	case []any:
		for _, item := range typed {
			if text, ok := item.(string); ok {
				values = append(values, text)
			}
		}
	}
	result := make([]string, 0, len(values))
	seen := map[string]struct{}{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || len(value) > 128 {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
		if len(result) >= 20 {
			break
		}
	}
	return result
}

func cloneConfig(config map[string]any) map[string]any {
	if config == nil {
		return nil
	}
	out := make(map[string]any, len(config))
	for key, value := range config {
		out[key] = cloneConfigValue(value)
	}
	return out
}

func cloneConfigValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return cloneConfig(typed)
	case []any:
		out := make([]any, len(typed))
		for i, item := range typed {
			out[i] = cloneConfigValue(item)
		}
		return out
	default:
		return value
	}
}

func (m *Manager) captionStartFailed(ctx context.Context, streamID, profileID, reason string, target error) error {
	m.report(ctx, streamID, "worker.caption.start_failed", "failed", map[string]any{
		"caption_profile_id": profileID,
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

func (m *Manager) ApplyProfileDefaults(stream StreamContext) StreamContext {
	m.mu.Lock()
	defaults := m.defaults
	m.mu.Unlock()
	if strings.TrimSpace(stream.OverlayProfileID) == "" {
		stream.OverlayProfileID = defaults.OverlayProfileID
	}
	if strings.TrimSpace(stream.CaptionProfileID) == "" {
		stream.CaptionProfileID = defaults.CaptionProfileID
	}
	return stream
}

func (m *Manager) applyStartProfileDefaults(stream StreamContext) StreamContext {
	m.mu.Lock()
	defaults := m.defaults
	m.mu.Unlock()
	if strings.TrimSpace(stream.OverlayProfileID) == "" {
		stream.OverlayProfileID = defaults.OverlayProfileID
	}
	return stream
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
	videoOutput := m.videoOutput
	videoRequested := videoOutputRequested(m.current)
	m.captionSession = nil
	// Mark the stop boundary before waiting for downstream stop operations. The
	// current stream is retained until its durable stop receipt is written so a
	// persistence failure preserves the existing retryable Stop semantics, but
	// all new events are rejected while this fence is active.
	m.stopping = true
	m.jobGeneration++
	m.mu.Unlock()
	m.stopEventDelivery()

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

func (m *Manager) wasStoppedTargetLocked(streamID string) bool {
	cutoff := time.Now().UTC().Add(-stoppedTargetReceiptTTL)
	for _, receipt := range m.stoppedOrder {
		if receipt.StreamID == streamID {
			return !receipt.StoppedAt.Before(cutoff)
		}
	}
	return false
}

func (m *Manager) rememberStoppedTargetLocked(streamID string) error {
	if streamID == "" {
		return nil
	}
	receipts := m.withoutStoppedTargetLocked(streamID)
	receipts = append(receipts, stoppedTargetReceipt{StreamID: streamID, StoppedAt: time.Now().UTC()})
	if len(receipts) > maxStoppedStreamTargets {
		receipts = receipts[len(receipts)-maxStoppedStreamTargets:]
	}
	if err := persistStoppedTargetReceipts(m.stoppedTargetReceiptPath, receipts); err != nil {
		return err
	}
	m.setStoppedTargetReceiptsLocked(receipts)
	return nil
}

func (m *Manager) rememberStoppedTargetInMemoryLocked(streamID string, stoppedAt time.Time) {
	streamID = strings.TrimSpace(streamID)
	if streamID == "" {
		return
	}
	receipts := m.withoutStoppedTargetLocked(streamID)
	receipts = append(receipts, stoppedTargetReceipt{StreamID: streamID, StoppedAt: stoppedAt.UTC()})
	if len(receipts) > maxStoppedStreamTargets {
		receipts = receipts[len(receipts)-maxStoppedStreamTargets:]
	}
	m.setStoppedTargetReceiptsLocked(receipts)
}

func (m *Manager) forgetStoppedTargetLocked(streamID string) error {
	if !m.wasStoppedTargetLocked(streamID) {
		return nil
	}
	receipts := m.withoutStoppedTargetLocked(streamID)
	if err := persistStoppedTargetReceipts(m.stoppedTargetReceiptPath, receipts); err != nil {
		return err
	}
	m.setStoppedTargetReceiptsLocked(receipts)
	return nil
}

func (m *Manager) withoutStoppedTargetLocked(streamID string) []stoppedTargetReceipt {
	receipts := make([]stoppedTargetReceipt, 0, len(m.stoppedOrder))
	cutoff := time.Now().UTC().Add(-stoppedTargetReceiptTTL)
	for _, receipt := range m.stoppedOrder {
		if receipt.StreamID != streamID && !receipt.StoppedAt.Before(cutoff) {
			receipts = append(receipts, receipt)
		}
	}
	return receipts
}

func (m *Manager) setStoppedTargetReceiptsLocked(receipts []stoppedTargetReceipt) {
	m.stoppedOrder = append([]stoppedTargetReceipt(nil), receipts...)
}

func (m *Manager) Close(ctx context.Context) error {
	streamID := m.CurrentStreamID()
	if streamID != "" {
		return m.Stop(ctx, streamID)
	}
	return nil
}

func (m *Manager) IngestCaptionAudio(ctx context.Context, streamID string, packets []deepgram.AudioPacket) error {
	return m.IngestCaptionAudioForGeneration(ctx, streamID, 0, packets)
}

func (m *Manager) IngestCaptionAudioForGeneration(ctx context.Context, streamID string, generation uint64, packets []deepgram.AudioPacket) error {
	if len(packets) == 0 {
		return errors.New("at least one opus packet is required")
	}
	m.mu.Lock()
	if err := m.ensureStreamLocked(streamID); err != nil {
		m.mu.Unlock()
		return err
	}
	if generation != 0 && generation != m.jobGeneration {
		m.mu.Unlock()
		return ErrJobGenerationMismatch
	}
	captionSession := m.captionSession
	m.mu.Unlock()
	if captionSession == nil {
		return ErrCaptionNotConfigured
	}
	failed := false
	for _, packet := range packets {
		if len(packet.Opus) == 0 {
			return errors.New("opus packet is required")
		}
		if err := captionSession.Ingest(ctx, packet); err != nil {
			m.report(ctx, streamID, "worker.caption.audio_failed", "failed", map[string]any{"ssrc": packet.SSRC})
			failed = true
		}
	}
	if failed {
		return ErrCaptionAudioUnavailable
	}
	return nil
}

func (m *Manager) CurrentStreamID() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.current.StreamID
}

func (m *Manager) SceneVideoConfig() (VideoSceneConfig, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sceneVideo, m.current.StreamID != "" && m.sceneRenderer != nil
}

func (m *Manager) RenderScene(at time.Time) (*image.RGBA, error) {
	m.mu.Lock()
	renderer := m.sceneRenderer
	config := m.sceneVideo
	active := m.current.StreamID != ""
	m.mu.Unlock()
	if !active || renderer == nil {
		return nil, ErrNoActiveStreamJob
	}
	return renderer.RenderSize(config.Width, config.Height, at)
}

func (m *Manager) AvatarRefreshInterval() time.Duration {
	m.mu.Lock()
	renderer := m.sceneRenderer
	m.mu.Unlock()
	if renderer == nil {
		return 15 * time.Minute
	}
	return renderer.AvatarRefreshInterval()
}

func (m *Manager) RefreshAvatars() {
	m.mu.Lock()
	renderer := m.sceneRenderer
	active := m.current.StreamID != ""
	m.mu.Unlock()
	if active && renderer != nil {
		renderer.RefreshAvatars()
	}
}

func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	status := Status{EventCount: len(m.events), SendFailures: m.sendFailures}
	if len(m.eventCounts) > 0 {
		status.EventCounts = make(map[string]int, len(m.eventCounts))
		for name, count := range m.eventCounts {
			status.EventCounts[name] = count
		}
	}
	if m.current.StreamID != "" {
		status.CurrentStreamID = m.current.StreamID
		status.StreamName = m.current.StreamName
		status.JobGeneration = m.jobGeneration
		status.StartedAt = m.startedAt
	}
	if len(m.events) > 0 {
		status.LastEventAt = m.events[len(m.events)-1].Timestamp
	}
	return status
}

func (m *Manager) Metrics() map[string]float64 {
	status := m.Status()
	metrics := map[string]float64{
		"worker.event_send_failures_total": float64(status.SendFailures),
		"worker.scene_updates_total":       0,
		"worker.overlay_events_total":      0,
		"worker.caption_events_total":      0,
	}
	for name, count := range status.EventCounts {
		metrics[name] = float64(count)
	}
	return metrics
}

func (m *Manager) RecentEvents(streamID string) ([]events.OverlayEvent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.ensureStreamLocked(streamID); err != nil {
		return nil, err
	}
	out := make([]events.OverlayEvent, len(m.events))
	copy(out, m.events)
	return out, nil
}

func (m *Manager) CurrentTime(ctx context.Context, streamID string, now time.Time) (events.OverlayEvent, error) {
	return m.publish(ctx, events.CurrentTimeEvent(streamID, now))
}

func (m *Manager) CurrentTimeForGeneration(ctx context.Context, streamID string, generation uint64, now time.Time) (events.OverlayEvent, error) {
	return m.publishWithGeneration(ctx, events.CurrentTimeEvent(streamID, now), generation)
}

func (m *Manager) Caption(ctx context.Context, streamID, text, speakerUserID string, now time.Time) (events.OverlayEvent, error) {
	return m.CaptionForGeneration(ctx, streamID, 0, text, speakerUserID, now)
}

func (m *Manager) CaptionForGeneration(ctx context.Context, streamID string, generation uint64, text, speakerUserID string, now time.Time) (events.OverlayEvent, error) {
	if strings.TrimSpace(text) == "" {
		return events.OverlayEvent{}, errors.New("caption text is required")
	}
	return m.publishWithGeneration(ctx, events.CaptionEvent(streamID, text, speakerUserID, now), generation)
}

func (m *Manager) CaptionTranscript(ctx context.Context, streamID, text, speakerUserID string, final bool, now time.Time) (events.OverlayEvent, error) {
	if strings.TrimSpace(text) == "" {
		return events.OverlayEvent{}, errors.New("caption text is required")
	}
	if final {
		return m.publish(ctx, events.FinalCaptionEvent(streamID, text, speakerUserID, now))
	}
	return m.publish(ctx, events.CaptionEvent(streamID, text, speakerUserID, now))
}

func (m *Manager) CaptionTranscriptDetailed(ctx context.Context, streamID string, transcript deepgram.Transcript) (events.OverlayEvent, error) {
	return m.CaptionTranscriptDetailedForGeneration(ctx, streamID, 0, transcript)
}

// CaptionTranscriptDetailedForGeneration keeps a delayed result from a closed
// Deepgram session out of a later rearm that happens to reuse the same stream
// ID.
func (m *Manager) CaptionTranscriptDetailedForGeneration(ctx context.Context, streamID string, generation uint64, transcript deepgram.Transcript) (events.OverlayEvent, error) {
	if strings.TrimSpace(transcript.Text) == "" {
		return events.OverlayEvent{}, errors.New("caption text is required")
	}
	now := transcript.UpdatedAt
	if now.IsZero() {
		now = transcript.ReceivedAt
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	return m.publishWithGeneration(ctx, events.CaptionTranscriptEvent(
		streamID,
		transcript.Text,
		transcript.SpeakerUserID,
		transcript.UtteranceID,
		transcript.Source,
		transcript.Revision,
		transcript.Final,
		transcript.StartedAt,
		now,
		transcript.EndedAt,
		transcript.Confidence,
		transcript.FinalizationReason,
		now,
	), generation)
}

func (m *Manager) Participants(ctx context.Context, streamID string, participants []events.Participant, now time.Time) (events.OverlayEvent, error) {
	return m.publish(ctx, events.ParticipantListEvent(streamID, participants, now))
}

func (m *Manager) ParticipantsForGeneration(ctx context.Context, streamID string, generation uint64, participants []events.Participant, now time.Time) (events.OverlayEvent, error) {
	return m.publishWithGeneration(ctx, events.ParticipantListEvent(streamID, participants, now), generation)
}

func (m *Manager) ActiveSpeaker(ctx context.Context, streamID, userID, displayName string, now time.Time) (events.OverlayEvent, error) {
	return m.ActiveSpeakerState(ctx, streamID, userID, displayName, true, now)
}

func (m *Manager) ActiveSpeakerState(ctx context.Context, streamID, userID, displayName string, speaking bool, now time.Time) (events.OverlayEvent, error) {
	return m.ActiveSpeakerStateForGeneration(ctx, streamID, 0, userID, displayName, speaking, now)
}

func (m *Manager) ActiveSpeakerStateForGeneration(ctx context.Context, streamID string, generation uint64, userID, displayName string, speaking bool, now time.Time) (events.OverlayEvent, error) {
	if speaking && strings.TrimSpace(userID) == "" {
		return events.OverlayEvent{}, errors.New("user_id is required")
	}
	return m.publishWithGeneration(ctx, events.ActiveSpeakerStateEvent(streamID, userID, displayName, speaking, now), generation)
}

func (m *Manager) CustomOverlay(ctx context.Context, streamID, eventType string, payload map[string]any, now time.Time) (events.OverlayEvent, error) {
	return m.CustomOverlayForGeneration(ctx, streamID, 0, eventType, payload, now)
}

func (m *Manager) CustomOverlayForGeneration(ctx context.Context, streamID string, generation uint64, eventType string, payload map[string]any, now time.Time) (events.OverlayEvent, error) {
	if !strings.HasPrefix(eventType, "overlay.") && !strings.HasPrefix(eventType, "caption.") {
		return events.OverlayEvent{}, errors.New("event type must start with overlay. or caption.")
	}
	return m.publishWithGeneration(ctx, events.CustomOverlayEvent(streamID, eventType, payload, now), generation)
}

func (m *Manager) publish(ctx context.Context, event events.OverlayEvent) (events.OverlayEvent, error) {
	return m.publishWithGeneration(ctx, event, 0)
}

func (m *Manager) publishWithGeneration(ctx context.Context, event events.OverlayEvent, expectedGeneration uint64) (events.OverlayEvent, error) {
	m.mu.Lock()
	if err := m.ensureStreamLocked(event.StreamID); err != nil {
		m.mu.Unlock()
		return events.OverlayEvent{}, err
	}
	if expectedGeneration != 0 && expectedGeneration != m.jobGeneration {
		m.mu.Unlock()
		return events.OverlayEvent{}, ErrJobGenerationMismatch
	}
	encoderRecorderURL := m.current.EncoderRecorderURL
	streamIngestToken := m.current.StreamIngestToken
	sceneRenderer := m.sceneRenderer
	generation := m.jobGeneration
	deliveryCtx := m.deliveryCtx
	m.mu.Unlock()
	if deliveryCtx == nil {
		deliveryCtx = ctx
	}
	m.supersedePendingWorkerEvent(event, generation)

	if sceneRenderer != nil {
		if err := sceneRenderer.Apply(generation, event); err != nil {
			m.report(ctx, event.StreamID, "worker.scene.apply_failed", "failed", map[string]any{"event_type": event.Type})
			return events.OverlayEvent{}, errors.New("scene event apply failed")
		}
	}

	encoderEvent := encoder.Event{
		ID:         event.ID,
		StreamID:   event.StreamID,
		Type:       event.Type,
		Payload:    event.Payload,
		Timestamp:  event.Timestamp,
		URL:        encoderRecorderURL,
		Token:      streamIngestToken,
		Generation: generation,
		Attempt:    1,
	}
	m.publisherMu.Lock()
	err := m.publisher.Publish(deliveryCtx, encoderEvent)
	m.publisherMu.Unlock()
	if err != nil {
		class, status := encoder.PublishErrorMetadata(err)
		retryable := encoder.IsRetryablePublishError(err)
		m.recordWorkerEventFailure(ctx, encoderEvent, 1, retryable, class, status)
		m.queueWorkerEventAfterFailure(encoderEvent, err)
		return events.OverlayEvent{}, errors.New("event publish failed")
	}
	if !m.recordDeliveredWorkerEvent(encoderEvent) {
		return events.OverlayEvent{}, errors.New("stream job changed while publishing event")
	}
	return event, nil
}

func normalizeVideoSceneConfig(width, height, fps int) (VideoSceneConfig, error) {
	if width == 0 && height == 0 {
		width, height = 1920, 1080
	}
	if fps <= 0 {
		fps = 30
	}
	if !((width == 1920 && height == 1080) || (width == 1280 && height == 720) || (width == 854 && height == 480)) {
		return VideoSceneConfig{}, errors.New("video scene size must be 1920x1080, 1280x720, or 854x480")
	}
	if fps < 1 || fps > 60 {
		return VideoSceneConfig{}, errors.New("video scene fps must be between 1 and 60")
	}
	return VideoSceneConfig{Width: width, Height: height, FPS: fps}, nil
}

func videoOutputRequested(stream StreamContext) bool {
	return strings.TrimSpace(stream.VideoIngestURL) != "" || strings.TrimSpace(stream.VideoIngestPassphrase) != "" || stream.VideoIngestPBKeylen != 0
}

func validateVideoOutputRequest(stream StreamContext) error {
	if !videoOutputRequested(stream) {
		return nil
	}
	if strings.TrimSpace(stream.EncoderProfileID) == "" || strings.TrimSpace(stream.VideoIngestURL) == "" || strings.TrimSpace(stream.VideoIngestPassphrase) == "" || stream.VideoIngestPBKeylen == 0 {
		return errors.New("encoder_profile_id and complete video ingest configuration are required together")
	}
	return nil
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

func (m *Manager) streamAssignedLocked(streamID string) bool {
	if !m.assignments.Enforce {
		return true
	}
	return m.assignments.PrimaryStreams[strings.TrimSpace(streamID)]
}

func (m *Manager) report(ctx context.Context, streamID, name, status string, attrs map[string]any) {
	if m.reporter == nil {
		return
	}
	_ = m.reporter.Event(ctx, streamID, name, status, attrs)
}

func (m *Manager) metric(ctx context.Context, streamID, name, status string, value float64, attrs map[string]any) {
	if m.reporter == nil {
		return
	}
	_ = m.reporter.Metric(ctx, streamID, name, status, value, attrs)
}

func (m *Manager) recordEventLocked(eventType string) map[string]int {
	if m.eventCounts == nil {
		m.eventCounts = map[string]int{}
	}
	metrics := map[string]int{}
	if strings.HasPrefix(eventType, "caption.") {
		m.eventCounts["worker.caption_events_total"]++
		metrics["worker.caption_events_total"] = m.eventCounts["worker.caption_events_total"]
	}
	if strings.HasPrefix(eventType, "overlay.") {
		m.eventCounts["worker.overlay_events_total"]++
		metrics["worker.overlay_events_total"] = m.eventCounts["worker.overlay_events_total"]
	}
	m.eventCounts["worker.scene_updates_total"]++
	metrics["worker.scene_updates_total"] = m.eventCounts["worker.scene_updates_total"]
	return metrics
}
