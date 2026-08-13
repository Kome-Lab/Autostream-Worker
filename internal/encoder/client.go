package encoder

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

type Event struct {
	ID         string         `json:"id"`
	StreamID   string         `json:"stream_id"`
	ServiceID  string         `json:"service_id,omitempty"`
	Generation uint64         `json:"job_generation,omitempty"`
	Attempt    uint32         `json:"attempt,omitempty"`
	Type       string         `json:"type"`
	Payload    map[string]any `json:"payload"`
	Timestamp  time.Time      `json:"timestamp"`
	URL        string         `json:"-"`
	Token      string         `json:"-"`
}

type Publisher interface {
	Publish(ctx context.Context, event Event) error
}

type Config struct {
	URL            string
	Token          string
	ServiceID      string
	Timeout        time.Duration
	RetryMax       int
	RetryBaseDelay time.Duration
}

type Client struct {
	Config Config
	HTTP   *http.Client
}

func ConfigFromEnv() Config {
	serviceID := strings.TrimSpace(os.Getenv("SERVICE_ID"))
	if serviceID == "" {
		serviceID = "worker-01"
	}
	return Config{
		URL:       os.Getenv("ENCODER_RECORDER_URL"),
		Token:     os.Getenv("ENCODER_RECORDER_TOKEN"),
		ServiceID: serviceID,
		Timeout:   envDuration("ENCODER_RECORDER_TIMEOUT_SEC", 5*time.Second),
		// Manager owns the durable bounded retry queue. Keep one HTTP attempt
		// per logical delivery attempt by default so attempt metadata and
		// duplicate pressure remain predictable; operators may opt into a
		// separate bounded client retry through the environment.
		RetryMax:       envInt("ENCODER_RECORDER_RETRY_MAX", 0),
		RetryBaseDelay: envDuration("ENCODER_RECORDER_RETRY_BASE_DELAY_SEC", time.Second),
	}
}

func (c Config) Validate() error {
	if strings.TrimSpace(c.URL) == "" {
		return errors.New("ENCODER_RECORDER_URL is required")
	}
	parsed, err := url.Parse(c.URL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return errors.New("ENCODER_RECORDER_URL must be an absolute URL")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return errors.New("ENCODER_RECORDER_URL must use http or https")
	}
	if parsed.User != nil {
		return errors.New("ENCODER_RECORDER_URL must not include credentials")
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("ENCODER_RECORDER_URL must not include query or fragment")
	}
	if parsed.Scheme == "http" && !isLocalDevHost(parsed.Hostname()) {
		return errors.New("ENCODER_RECORDER_URL must use https for remote hosts")
	}
	return nil
}

func isLocalDevHost(host string) bool {
	normalized := strings.Trim(strings.ToLower(strings.TrimSpace(host)), "[]")
	return normalized == "localhost" || normalized == "127.0.0.1" || normalized == "host.docker.internal"
}

func (c Client) Publish(ctx context.Context, event Event) error {
	targetURL := strings.TrimSpace(c.Config.URL)
	if strings.TrimSpace(event.URL) != "" {
		targetURL = strings.TrimSpace(event.URL)
	}
	targetConfig := c.Config
	targetConfig.URL = targetURL
	if err := targetConfig.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(c.Config.Token) == "" && strings.TrimSpace(event.Token) == "" {
		return errors.New("ENCODER_RECORDER_TOKEN is required")
	}
	if event.Timestamp.IsZero() {
		event.Timestamp = time.Now().UTC()
	}
	if strings.TrimSpace(event.ServiceID) == "" {
		event.ServiceID = strings.TrimSpace(c.Config.ServiceID)
	}
	body, err := json.Marshal(event)
	if err != nil {
		return err
	}
	attempts := c.Config.RetryMax + 1
	if attempts < 1 {
		attempts = 1
	}
	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		err := c.publishOnce(ctx, targetURL, body, event.Token)
		if err == nil {
			return nil
		}
		lastErr = err
		if !isTransientPublishError(err) || attempt == attempts-1 {
			break
		}
		delay := c.Config.RetryBaseDelay
		if delay <= 0 {
			delay = time.Second
		}
		timer := time.NewTimer(delay * time.Duration(1<<attempt))
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return lastErr
}

func (c Client) publishOnce(ctx context.Context, targetURL string, body []byte, tokenOverride string) error {
	reqCtx := ctx
	if c.Config.Timeout > 0 {
		var cancel context.CancelFunc
		reqCtx, cancel = context.WithTimeout(ctx, c.Config.Timeout)
		defer cancel()
	}
	endpoint, err := endpointURL(targetURL, "/worker-events")
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	token := strings.TrimSpace(c.Config.Token)
	if strings.TrimSpace(tokenOverride) != "" {
		token = strings.TrimSpace(tokenOverride)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	client := c.HTTP
	if client == nil {
		client = noRedirectClient()
	}
	res, err := client.Do(req)
	if err != nil {
		class := "transport"
		if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
			class = "timeout"
		}
		return transientError{err: err, class: class, retryable: true}
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		err := fmt.Errorf("encoder worker-event publish failed with status %d", res.StatusCode)
		if res.StatusCode == http.StatusRequestTimeout || res.StatusCode == http.StatusConflict || res.StatusCode == http.StatusTooManyRequests || res.StatusCode >= 500 {
			return transientError{err: err, status: res.StatusCode, class: "http_status", retryable: true}
		}
		return transientError{err: err, status: res.StatusCode, class: "http_status"}
	}
	return nil
}

func endpointURL(baseURL, endpoint string) (string, error) {
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return "", err
	}
	pathPrefix := strings.TrimRight(parsed.EscapedPath(), "/")
	parsed.RawPath = pathPrefix + "/" + strings.TrimLeft(endpoint, "/")
	parsed.Path = strings.TrimRight(parsed.Path, "/") + "/" + strings.TrimLeft(endpoint, "/")
	return parsed.String(), nil
}

func noRedirectClient() *http.Client {
	return &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

type transientError struct {
	err       error
	status    int
	class     string
	retryable bool
}

// NewRetryablePublishError is used by bounded-delivery adapters that need to
// preserve the same safe retry classification as the HTTP client.
func NewRetryablePublishError(status int, class string) error {
	if strings.TrimSpace(class) == "" {
		class = "http_status"
	}
	return transientError{err: fmt.Errorf("encoder worker-event publish failed with status %d", status), status: status, class: class, retryable: true}
}

func (e transientError) Error() string {
	return e.err.Error()
}

func (e transientError) Unwrap() error {
	return e.err
}

func isTransientPublishError(err error) bool {
	var transient transientError
	return errors.As(err, &transient) && transient.retryable
}

// IsRetryablePublishError exposes only the bounded-retry decision. Callers
// must not log the underlying error because it may contain provider details.
func IsRetryablePublishError(err error) bool {
	return isTransientPublishError(err)
}

// PublishErrorMetadata returns safe, low-cardinality delivery metadata.
func PublishErrorMetadata(err error) (class string, status int) {
	var transient transientError
	if errors.As(err, &transient) {
		class = transient.class
		status = transient.status
		if class == "" {
			class = "transport"
		}
		return class, status
	}
	if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
		return "timeout", 0
	}
	return "non_retryable", 0
}

type NoopPublisher struct{}

func (NoopPublisher) Publish(context.Context, Event) error { return nil }

func envDuration(key string, fallback time.Duration) time.Duration {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	duration, err := time.ParseDuration(value + "s")
	if err != nil || duration <= 0 {
		return fallback
	}
	return duration
}

func envInt(key string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < 0 {
		return fallback
	}
	return parsed
}
