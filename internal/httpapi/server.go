package httpapi

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/example/autostream-worker/internal/control"
	"github.com/example/autostream-worker/internal/deepgram"
	"github.com/example/autostream-worker/internal/encoder"
	"github.com/example/autostream-worker/internal/events"
	"github.com/example/autostream-worker/internal/ingesttoken"
	"github.com/example/autostream-worker/internal/jobs"
	"github.com/example/autostream-worker/internal/observability"
)

type Status struct {
	ServiceType string      `json:"service_type"`
	ServiceID   string      `json:"service_id"`
	Status      string      `json:"status"`
	CheckedAt   time.Time   `json:"checked_at"`
	Worker      jobs.Status `json:"worker"`
}

const maxCaptionAudioBodyBytes int64 = 1 << 20

type discordOpusIngestRequest struct {
	StreamID string                    `json:"stream_id"`
	Source   string                    `json:"source"`
	Packets  []discordOpusIngestPacket `json:"packets"`
}

type discordOpusIngestPacket struct {
	SSRC       *uint32    `json:"ssrc"`
	UserID     string     `json:"user_id,omitempty"`
	Sequence   *uint16    `json:"sequence"`
	Timestamp  *uint64    `json:"timestamp"`
	ReceivedAt *time.Time `json:"received_at"`
	OpusBase64 string     `json:"opus_base64"`
}

type TokenVerifier struct {
	PlainToken            string
	SHA256Hex             string
	IngestTokenSigningKey string
}

func TokenVerifierFromEnv() TokenVerifier {
	verifier := TokenVerifier{PlainToken: os.Getenv("SERVICE_CONTROL_TOKEN"), SHA256Hex: os.Getenv("SERVICE_CONTROL_TOKEN_SHA256"), IngestTokenSigningKey: control.StreamIngestSigningKey()}
	if verifier.PlainToken == "" && verifier.SHA256Hex == "" {
		if token := control.NodeRuntimeTokenFromEnv(); token != "" {
			sum := sha256.Sum256([]byte(token))
			verifier.SHA256Hex = hex.EncodeToString(sum[:])
		}
	}
	return verifier
}

func (v TokenVerifier) Verify(header string) bool {
	if !strings.HasPrefix(header, "Bearer ") {
		return false
	}
	token := strings.TrimSpace(strings.TrimPrefix(header, "Bearer "))
	if token == "" {
		return false
	}
	if v.SHA256Hex != "" {
		sum := sha256.Sum256([]byte(token))
		got := hex.EncodeToString(sum[:])
		return subtle.ConstantTimeCompare([]byte(got), []byte(strings.ToLower(v.SHA256Hex))) == 1
	}
	if v.PlainToken != "" {
		return subtle.ConstantTimeCompare([]byte(token), []byte(v.PlainToken)) == 1
	}
	if runtimeToken := control.NodeRuntimeTokenFromEnv(); runtimeToken != "" {
		return subtle.ConstantTimeCompare([]byte(token), []byte(runtimeToken)) == 1
	}
	if !allowControlPanelTokenFallback() {
		return false
	}
	fallback := os.Getenv("CONTROL_PANEL_TOKEN")
	return fallback != "" && subtle.ConstantTimeCompare([]byte(token), []byte(fallback)) == 1
}

func (v TokenVerifier) VerifyWorkerEvents(header, streamID string) bool {
	if v.Verify(header) {
		return true
	}
	token := bearerToken(header)
	signingKey := strings.TrimSpace(v.IngestTokenSigningKey)
	if signingKey == "" {
		signingKey = control.StreamIngestSigningKey()
	}
	if token == "" || !ingesttoken.IsSigned(token) || signingKey == "" {
		return false
	}
	_, err := ingesttoken.Verify(signingKey, token, ingesttoken.Expected{
		StreamID:    streamID,
		ServiceType: "discord_bot",
		Purpose:     "worker_events",
		Audience:    "worker",
	})
	return err == nil
}

func (v TokenVerifier) VerifyCaptionAudio(header, streamID string) bool {
	token := bearerToken(header)
	signingKey := strings.TrimSpace(v.IngestTokenSigningKey)
	if signingKey == "" {
		signingKey = control.StreamIngestSigningKey()
	}
	if token == "" || !ingesttoken.IsSigned(token) || signingKey == "" {
		return false
	}
	_, err := ingesttoken.Verify(signingKey, token, ingesttoken.Expected{
		StreamID:    streamID,
		ServiceType: "discord_bot",
		Purpose:     "caption_audio",
		Audience:    "worker",
	})
	return err == nil
}

func bearerToken(header string) string {
	if !strings.HasPrefix(header, "Bearer ") {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(header, "Bearer "))
}

func allowControlPanelTokenFallback() bool {
	if envBool("AUTOSTREAM_REQUIRE_CONTROL_PANEL_RUNTIME_CONFIG", false) {
		return false
	}
	return !strings.EqualFold(strings.TrimSpace(os.Getenv("AUTOSTREAM_ENV")), "production")
}

func envBool(key string, fallback bool) bool {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	switch strings.ToLower(raw) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return fallback
	}
}

type Server struct {
	serviceType   string
	manager       *jobs.Manager
	verifier      TokenVerifier
	runtimeConfig RuntimeConfigProvider
}

type RuntimeConfigProvider func(context.Context) (control.RuntimeConfig, error)

func NewServer(serviceType string, manager *jobs.Manager, verifier TokenVerifier) http.Handler {
	return NewServerWithRuntimeConfig(serviceType, manager, verifier, nil)
}

func NewServerWithRuntimeConfig(serviceType string, manager *jobs.Manager, verifier TokenVerifier, runtimeConfig RuntimeConfigProvider) http.Handler {
	if manager == nil {
		manager = jobs.NewManager(encoder.NoopPublisher{}, observability.Client{})
	}
	server := Server{serviceType: serviceType, manager: manager, verifier: verifier, runtimeConfig: runtimeConfig}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", server.health)
	mux.HandleFunc("GET /status", server.status)
	mux.HandleFunc("POST /heartbeat", server.heartbeat)
	mux.HandleFunc("POST /jobs/start", server.startJob)
	mux.HandleFunc("POST /jobs/{id}/stop", server.stopJob)
	mux.HandleFunc("POST /streams/{id}/audio/opus", server.captionAudio)
	mux.HandleFunc("GET /streams/{id}/events", server.recentEvents)
	mux.HandleFunc("POST /streams/{id}/events/current-time", server.currentTimeEvent)
	mux.HandleFunc("POST /streams/{id}/events/caption", server.captionEvent)
	mux.HandleFunc("POST /streams/{id}/events/participants", server.participantsEvent)
	mux.HandleFunc("POST /streams/{id}/events/active-speaker", server.activeSpeakerEvent)
	mux.HandleFunc("POST /streams/{id}/events/overlay", server.customOverlayEvent)
	return securityHeaders(mux)
}

func (s Server) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s Server) status(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, Status{ServiceType: s.serviceType, ServiceID: os.Getenv("SERVICE_ID"), Status: "ready", CheckedAt: time.Now().UTC(), Worker: s.manager.Status()})
}

func (s Server) heartbeat(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"code": "missing_or_invalid_service_token"})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "accepted"})
}

func (s Server) startJob(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"code": "missing_or_invalid_service_token"})
		return
	}
	var req jobs.StreamContext
	if err := decodeJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"code": "invalid_json"})
		return
	}
	if s.runtimeConfig != nil {
		cfg, err := s.runtimeConfig(r.Context())
		if err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"code": "runtime_config_unavailable", "message": "control panel runtime config fetch failed"})
			return
		}
		s.manager.ApplyRuntimeConfig(cfg)
	}
	if err := s.manager.Start(r.Context(), req); err != nil {
		writeRequestError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, s.manager.Status())
}

func (s Server) stopJob(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"code": "missing_or_invalid_service_token"})
		return
	}
	if err := s.manager.Stop(r.Context(), r.PathValue("id")); err != nil {
		writeRequestError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "stopped"})
}

func (s Server) captionAudio(w http.ResponseWriter, r *http.Request) {
	streamID := r.PathValue("id")
	if !s.authorizedCaptionAudio(r, streamID) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"code": "missing_or_invalid_service_token"})
		return
	}
	var req discordOpusIngestRequest
	if err := decodeJSONLimited(w, r, &req, maxCaptionAudioBodyBytes); err != nil {
		var maxBytesError *http.MaxBytesError
		if errors.As(err, &maxBytesError) {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"code": "payload_too_large"})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"code": "invalid_json"})
		return
	}
	if strings.TrimSpace(req.StreamID) == "" || strings.TrimSpace(req.Source) == "" || len(req.Packets) == 0 || len(req.Packets) > 100 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"code": "invalid_audio_payload"})
		return
	}
	if req.StreamID != streamID {
		writeJSON(w, http.StatusBadRequest, map[string]string{"code": "stream_id_mismatch"})
		return
	}
	packets := make([]deepgram.AudioPacket, 0, len(req.Packets))
	for _, packet := range req.Packets {
		if packet.SSRC == nil || packet.Sequence == nil || packet.Timestamp == nil || packet.ReceivedAt == nil || packet.ReceivedAt.IsZero() || packet.OpusBase64 == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"code": "invalid_audio_payload"})
			return
		}
		opus, err := base64.StdEncoding.Strict().DecodeString(packet.OpusBase64)
		if err != nil || len(opus) == 0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"code": "invalid_audio_payload"})
			return
		}
		packets = append(packets, deepgram.AudioPacket{
			SSRC:       *packet.SSRC,
			UserID:     strings.TrimSpace(packet.UserID),
			Sequence:   *packet.Sequence,
			Timestamp:  *packet.Timestamp,
			ReceivedAt: packet.ReceivedAt.UTC(),
			Opus:       opus,
		})
	}
	if err := s.manager.IngestCaptionAudio(r.Context(), streamID, packets); err != nil {
		switch {
		case errors.Is(err, jobs.ErrCaptionNotConfigured):
			writeJSON(w, http.StatusConflict, map[string]string{"code": "caption_audio_disabled"})
		case errors.Is(err, jobs.ErrCaptionAudioUnavailable):
			writeJSON(w, http.StatusBadGateway, map[string]string{"code": "caption_provider_unavailable"})
		default:
			writeRequestError(w, err)
		}
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"status": "accepted", "packet_count": len(packets)})
}

func (s Server) recentEvents(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"code": "missing_or_invalid_service_token"})
		return
	}
	result, err := s.manager.RecentEvents(r.PathValue("id"))
	if err != nil {
		writeRequestError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": result})
}

func (s Server) currentTimeEvent(w http.ResponseWriter, r *http.Request) {
	if !s.authorizedWorkerEvents(r, r.PathValue("id")) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"code": "missing_or_invalid_service_token"})
		return
	}
	event, err := s.manager.CurrentTime(r.Context(), r.PathValue("id"), time.Now().UTC())
	writeEventResult(w, event, err)
}

func (s Server) captionEvent(w http.ResponseWriter, r *http.Request) {
	if !s.authorizedWorkerEvents(r, r.PathValue("id")) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"code": "missing_or_invalid_service_token"})
		return
	}
	var req struct {
		Text          string `json:"text"`
		SpeakerUserID string `json:"speaker_user_id,omitempty"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"code": "invalid_json"})
		return
	}
	event, err := s.manager.Caption(r.Context(), r.PathValue("id"), req.Text, req.SpeakerUserID, time.Now().UTC())
	writeEventResult(w, event, err)
}

func (s Server) participantsEvent(w http.ResponseWriter, r *http.Request) {
	if !s.authorizedWorkerEvents(r, r.PathValue("id")) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"code": "missing_or_invalid_service_token"})
		return
	}
	var req struct {
		Participants []events.Participant `json:"participants"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"code": "invalid_json"})
		return
	}
	event, err := s.manager.Participants(r.Context(), r.PathValue("id"), req.Participants, time.Now().UTC())
	writeEventResult(w, event, err)
}

func (s Server) activeSpeakerEvent(w http.ResponseWriter, r *http.Request) {
	if !s.authorizedWorkerEvents(r, r.PathValue("id")) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"code": "missing_or_invalid_service_token"})
		return
	}
	var req struct {
		UserID      string `json:"user_id"`
		DisplayName string `json:"display_name,omitempty"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"code": "invalid_json"})
		return
	}
	event, err := s.manager.ActiveSpeaker(r.Context(), r.PathValue("id"), req.UserID, req.DisplayName, time.Now().UTC())
	writeEventResult(w, event, err)
}

func (s Server) customOverlayEvent(w http.ResponseWriter, r *http.Request) {
	if !s.authorizedWorkerEvents(r, r.PathValue("id")) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"code": "missing_or_invalid_service_token"})
		return
	}
	var req struct {
		Type    string         `json:"type"`
		Payload map[string]any `json:"payload"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"code": "invalid_json"})
		return
	}
	event, err := s.manager.CustomOverlay(r.Context(), r.PathValue("id"), req.Type, req.Payload, time.Now().UTC())
	writeEventResult(w, event, err)
}

func (s Server) authorized(r *http.Request) bool {
	return s.verifier.Verify(r.Header.Get("Authorization"))
}

func (s Server) authorizedWorkerEvents(r *http.Request, streamID string) bool {
	return s.verifier.VerifyWorkerEvents(r.Header.Get("Authorization"), streamID)
}

func (s Server) authorizedCaptionAudio(r *http.Request, streamID string) bool {
	return s.verifier.VerifyCaptionAudio(r.Header.Get("Authorization"), streamID)
}

func decodeJSON(r *http.Request, value any) error {
	defer r.Body.Close()
	decoder := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	return decoder.Decode(value)
}

func decodeJSONLimited(w http.ResponseWriter, r *http.Request, value any, limit int64) error {
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	defer r.Body.Close()
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return errors.New("multiple JSON values are not allowed")
		}
		return err
	}
	return nil
}

func writeEventResult(w http.ResponseWriter, event events.OverlayEvent, err error) {
	if err != nil {
		writeRequestError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, event)
}

func writeRequestError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, jobs.ErrCaptionProfileInvalid):
		writeJSON(w, http.StatusConflict, map[string]string{"code": "caption_profile_invalid", "message": "selected caption profile is invalid"})
		return
	case errors.Is(err, jobs.ErrCaptionRuntimeUnavailable):
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"code": "caption_runtime_unavailable", "message": "caption runtime initialization failed"})
		return
	case errors.Is(err, jobs.ErrCaptionNotConfigured):
		writeJSON(w, http.StatusConflict, map[string]string{"code": "caption_audio_disabled"})
		return
	case errors.Is(err, jobs.ErrCaptionAudioUnavailable):
		writeJSON(w, http.StatusBadGateway, map[string]string{"code": "caption_provider_unavailable"})
		return
	}
	status := http.StatusConflict
	code := "invalid_stream_state"
	if strings.Contains(err.Error(), "required") || strings.Contains(err.Error(), "must start") {
		status = http.StatusBadRequest
		code = "validation_failed"
	}
	writeJSON(w, status, map[string]string{"code": code, "message": err.Error()})
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
