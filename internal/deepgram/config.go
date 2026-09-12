package deepgram

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Model             string
	Language          string
	EndpointingMS     int
	UtteranceEndMS    int
	LocalFinalize     time.Duration
	SpeakerIdleClose  time.Duration
	KeepAliveInterval time.Duration
	InterimResults    bool
	SmartFormat       bool
	Keyterms          []string
	MIPOptOut         bool
	ReplayBufferMax   time.Duration
	Delay             time.Duration
}

type sessionOptions struct {
	endpoint          string
	dialer            socketDialer
	keepAliveInterval time.Duration
	dialTimeout       time.Duration
	writeTimeout      time.Duration
	closeTimeout      time.Duration
	maxConnections    int
}

func (o sessionOptions) withDefaults() sessionOptions {
	if strings.TrimSpace(o.endpoint) == "" {
		o.endpoint = Endpoint
	}
	if o.dialer == nil {
		o.dialer = coderDialer{client: &http.Client{
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}}
	}
	if o.keepAliveInterval <= 0 {
		o.keepAliveInterval = defaultKeepAliveInterval
	}
	if o.dialTimeout <= 0 {
		o.dialTimeout = defaultDialTimeout
	}
	if o.writeTimeout <= 0 {
		o.writeTimeout = defaultWriteTimeout
	}
	if o.closeTimeout <= 0 {
		o.closeTimeout = defaultCloseTimeout
	}
	if o.maxConnections <= 0 {
		o.maxConnections = defaultMaxConnections
	}
	return o
}

func ListenURL(config Config) (string, error) {
	return buildListenURL(Endpoint, config)
}

func buildListenURL(endpoint string, config Config) (string, error) {
	if err := validateConfig(config); err != nil {
		return "", err
	}
	parsed, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil || (parsed.Scheme != "wss" && parsed.Scheme != "ws") || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("deepgram endpoint is invalid")
	}
	query := parsed.Query()
	query.Set("encoding", "opus")
	query.Set("sample_rate", "48000")
	query.Set("channels", "2")
	query.Set("model", config.Model)
	query.Set("language", config.Language)
	query.Set("interim_results", strconv.FormatBool(config.InterimResults))
	query.Set("smart_format", strconv.FormatBool(config.SmartFormat))
	query.Set("endpointing", strconv.Itoa(config.EndpointingMS))
	if config.UtteranceEndMS > 0 && config.InterimResults {
		query.Set("utterance_end_ms", strconv.Itoa(config.UtteranceEndMS))
	}
	if config.MIPOptOut {
		query.Set("mip_opt_out", "true")
	}
	for _, keyterm := range config.Keyterms {
		keyterm = strings.TrimSpace(keyterm)
		if keyterm != "" {
			query.Add("keyterm", keyterm)
		}
	}
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}

func validateConfig(config Config) error {
	if strings.TrimSpace(config.Model) == "" || len(config.Model) > 64 {
		return errors.New("deepgram model is invalid")
	}
	if strings.TrimSpace(config.Language) == "" || len(config.Language) > 32 {
		return errors.New("deepgram language is invalid")
	}
	if config.EndpointingMS < 10 || config.EndpointingMS > 5000 {
		return errors.New("deepgram endpointing_ms is invalid")
	}
	if config.Delay < 0 || config.Delay > 10*time.Second {
		return errors.New("deepgram delay is invalid")
	}
	if config.UtteranceEndMS < 0 || config.UtteranceEndMS > 10000 {
		return errors.New("deepgram utterance_end_ms is invalid")
	}
	if config.LocalFinalize < 0 || config.LocalFinalize > 10*time.Second || config.SpeakerIdleClose < 0 || config.SpeakerIdleClose > 2*time.Minute || config.KeepAliveInterval < 0 || config.KeepAliveInterval > time.Minute {
		return errors.New("deepgram session timing is invalid")
	}
	if len(config.Keyterms) > 20 {
		return errors.New("deepgram keyterms are invalid")
	}
	for _, keyterm := range config.Keyterms {
		if keyterm = strings.TrimSpace(keyterm); keyterm == "" || len(keyterm) > 128 {
			return errors.New("deepgram keyterms are invalid")
		}
	}
	return nil
}
