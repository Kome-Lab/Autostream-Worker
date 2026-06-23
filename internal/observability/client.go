package observability

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const serviceType = "worker"

type Config struct {
	URL       string
	Token     string
	ServiceID string
	Timeout   time.Duration
}

type Client struct {
	Config Config
	HTTP   *http.Client
}

type Signal struct {
	Type        string         `json:"type"`
	Name        string         `json:"name"`
	ServiceID   string         `json:"service_id"`
	ServiceType string         `json:"service_type"`
	StreamID    string         `json:"stream_id,omitempty"`
	Status      string         `json:"status,omitempty"`
	Value       *float64       `json:"value,omitempty"`
	Attributes  map[string]any `json:"attributes,omitempty"`
	Timestamp   time.Time      `json:"timestamp"`
}

func ConfigFromEnv() Config {
	return Config{
		URL:       os.Getenv("OBSERVABILITY_URL"),
		Token:     os.Getenv("OBSERVABILITY_TOKEN"),
		ServiceID: envDefault("SERVICE_ID", "worker-01"),
		Timeout:   envDuration("OBSERVABILITY_TIMEOUT_SEC", 5*time.Second),
	}
}

func NewClientFromEnv() Client {
	return Client{Config: ConfigFromEnv()}
}

func (c Client) Enabled() bool {
	return strings.TrimSpace(c.Config.URL) != "" && strings.TrimSpace(c.Config.Token) != ""
}

func (c Client) Validate() error {
	if strings.TrimSpace(c.Config.URL) == "" {
		return errors.New("OBSERVABILITY_URL is required")
	}
	if strings.TrimSpace(c.Config.Token) == "" {
		return errors.New("OBSERVABILITY_TOKEN is required")
	}
	parsed, err := url.Parse(c.Config.URL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return errors.New("OBSERVABILITY_URL must be an absolute URL")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return errors.New("OBSERVABILITY_URL must use http or https")
	}
	if parsed.User != nil {
		return errors.New("OBSERVABILITY_URL must not include credentials")
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("OBSERVABILITY_URL must not include query or fragment")
	}
	if parsed.Scheme == "http" && !isLocalDevHost(parsed.Hostname()) {
		return errors.New("OBSERVABILITY_URL must use https for remote hosts")
	}
	return nil
}

func isLocalDevHost(host string) bool {
	normalized := strings.Trim(strings.ToLower(strings.TrimSpace(host)), "[]")
	return normalized == "localhost" || normalized == "127.0.0.1" || normalized == "host.docker.internal"
}

func (c Client) Event(ctx context.Context, streamID, name, status string, attributes map[string]any) error {
	return c.Report(ctx, Signal{Type: "event", Name: name, StreamID: streamID, Status: status, Attributes: attributes})
}

func (c Client) Metric(ctx context.Context, streamID, name, status string, value float64, attributes map[string]any) error {
	return c.Report(ctx, Signal{Type: "metric", Name: name, StreamID: streamID, Status: status, Value: &value, Attributes: attributes})
}

func (c Client) Report(ctx context.Context, signal Signal) error {
	if err := c.Validate(); err != nil {
		return err
	}
	if signal.Timestamp.IsZero() {
		signal.Timestamp = time.Now().UTC()
	}
	if signal.ServiceID == "" {
		signal.ServiceID = c.Config.ServiceID
	}
	if signal.ServiceType == "" {
		signal.ServiceType = serviceType
	}
	body, err := json.Marshal(signal)
	if err != nil {
		return err
	}
	reqCtx := ctx
	if c.Config.Timeout > 0 {
		var cancel context.CancelFunc
		reqCtx, cancel = context.WithTimeout(ctx, c.Config.Timeout)
		defer cancel()
	}
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, strings.TrimRight(c.Config.URL, "/")+"/signals", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Config.Token)
	req.Header.Set("Content-Type", "application/json")
	client := c.HTTP
	if client == nil {
		client = noRedirectClient()
	}
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return fmt.Errorf("observability signal failed with status %d", res.StatusCode)
	}
	return nil
}

func noRedirectClient() *http.Client {
	return &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func envDefault(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

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
