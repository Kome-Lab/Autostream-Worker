package jobs

import (
	"encoding/json"
	"math"
	"strings"
	"time"

	"github.com/example/autostream-worker/internal/control"
	"github.com/example/autostream-worker/internal/deepgram"
)

type captionDisplayConfig struct {
	maxItems             int
	reorderWindow        time.Duration
	interimTTL           time.Duration
	finalTTL             time.Duration
	showVoiceTranscripts bool
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
