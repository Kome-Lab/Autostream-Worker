package jobs

import (
	"context"
	"errors"
	"image"
	"strings"
	"time"

	"github.com/example/autostream-worker/internal/events"
	"github.com/example/autostream-worker/internal/sceneappearance"
)

type VideoSceneConfig struct {
	Width      int    `json:"width"`
	Height     int    `json:"height"`
	FPS        int    `json:"fps"`
	Generation uint64 `json:"-"`
}

type SceneRenderer interface {
	ConfigureDisplay(maxItems int, reorderWindow, interimTTL, finalTTL time.Duration, showVoiceTranscripts bool)
	Reset(generation uint64, streamID, streamName string)
	Clear(streamID string)
	Apply(generation uint64, event events.OverlayEvent) error
	RenderSize(width, height int, at time.Time) (*image.RGBA, error)
	AvatarRefreshInterval() time.Duration
	RefreshAvatars()
}

type sceneAppearanceConfigurer interface {
	ConfigureAppearance(sceneappearance.Prepared) error
}

type SceneAppearanceRuntime interface {
	Prepare(context.Context, string, *sceneappearance.Snapshot) (sceneappearance.Prepared, error)
}

type SceneAppearanceRuntimeFunc func(context.Context, string, *sceneappearance.Snapshot) (sceneappearance.Prepared, error)

func (f SceneAppearanceRuntimeFunc) Prepare(ctx context.Context, streamID string, snapshot *sceneappearance.Snapshot) (sceneappearance.Prepared, error) {
	if f == nil {
		return sceneappearance.Prepared{}, sceneappearance.NewError(sceneappearance.CodeCapabilityRequired)
	}
	return f(ctx, streamID, snapshot)
}

type VideoOutput interface {
	Start(context.Context, StreamContext, VideoSceneConfig) error
	Stop(context.Context, string) error
}

func (m *Manager) SetSceneRenderer(renderer SceneRenderer) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sceneRenderer = renderer
}

func (m *Manager) SetSceneAppearanceRuntime(runtime SceneAppearanceRuntime) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sceneAppearanceRuntime = runtime
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
	captionIngress := m.captionIngress
	sceneRenderer := m.sceneRenderer
	m.current = StreamContext{}
	m.captionSession = nil
	m.captionIngress = nil
	m.captionSessionGeneration++
	m.captionAudioConnectionGeneration = 0
	m.sceneVideo = VideoSceneConfig{}
	m.startedAt = time.Time{}
	m.jobGeneration++
	m.mu.Unlock()
	m.stopEventDelivery()

	if sceneRenderer != nil {
		sceneRenderer.Clear(streamID)
	}
	failureCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	stopCaptionAudioIngress(failureCtx, captionIngress)
	if captionSession != nil {
		if err := captionSession.Close(failureCtx); err != nil {
			m.report(failureCtx, streamID, "worker.caption.stop_failed", "failed", nil)
		}
	}
	m.lifecycleMu.Unlock()

	errorClass = normalizeVideoOutputErrorClass(errorClass)
	attributes := map[string]any{"reason": "transport_stopped", "error_class": errorClass}
	if receiptErr != nil {
		attributes["stopped_target_receipt"] = "unavailable"
	}
	m.report(failureCtx, streamID, "worker.video.output_failed", "failed", attributes)
}

func normalizeVideoOutputErrorClass(value string) string {
	switch strings.TrimSpace(value) {
	case "srt_write", "scene_render", "frame_shape", "jpeg_encode", "jpeg_size", "transport_stopped":
		return strings.TrimSpace(value)
	default:
		return "unknown"
	}
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
