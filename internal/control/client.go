package control

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

const ServiceType = "worker"

type Config struct {
	ControlPanelURL  string
	Token            string
	ServiceID        string
	ServiceName      string
	ServicePublicURL string
	Version          string
	HeartbeatEvery   time.Duration
}

type Client struct {
	Config Config
	HTTP   *http.Client
}

type Registration struct {
	ServiceID    string         `json:"service_id"`
	ServiceType  string         `json:"service_type"`
	ServiceName  string         `json:"service_name"`
	PublicURL    string         `json:"public_url"`
	Version      string         `json:"version"`
	Capabilities map[string]any `json:"capabilities"`
}

type Heartbeat struct {
	ServiceID       string             `json:"service_id"`
	Status          string             `json:"status"`
	CurrentStreamID string             `json:"current_stream_id,omitempty"`
	Metrics         map[string]float64 `json:"metrics,omitempty"`
}

type RuntimeConfig struct {
	Service     RegisteredService           `json:"service"`
	Assignments []StreamServiceAssignment   `json:"assignments"`
	Profiles    map[string][]RuntimeProfile `json:"profiles"`
}

type RegisteredService struct {
	ServiceID       string         `json:"service_id"`
	ServiceType     string         `json:"service_type"`
	ServiceName     string         `json:"service_name"`
	PublicURL       string         `json:"public_url"`
	Version         string         `json:"version"`
	Status          string         `json:"status"`
	AssignmentRole  string         `json:"assignment_role,omitempty"`
	CurrentStreamID string         `json:"current_stream_id,omitempty"`
	Capabilities    map[string]any `json:"capabilities,omitempty"`
}

type StreamServiceAssignment struct {
	StreamID       string    `json:"stream_id"`
	ServiceID      string    `json:"service_id"`
	ServiceType    string    `json:"service_type"`
	AssignmentRole string    `json:"assignment_role"`
	AssignedAt     time.Time `json:"assigned_at"`
}

type RuntimeProfile struct {
	ID        string         `json:"id"`
	Kind      string         `json:"kind"`
	Name      string         `json:"name"`
	Config    map[string]any `json:"config"`
	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
}

func ConfigFromEnv() Config {
	return Config{
		ControlPanelURL:  os.Getenv("CONTROL_PANEL_URL"),
		Token:            os.Getenv("CONTROL_PANEL_TOKEN"),
		ServiceID:        envDefault("SERVICE_ID", "worker-01"),
		ServiceName:      envDefault("SERVICE_NAME", "Worker"),
		ServicePublicURL: os.Getenv("SERVICE_PUBLIC_URL"),
		Version:          envDefault("SERVICE_VERSION", "dev"),
		HeartbeatEvery:   envDuration("CONTROL_PANEL_HEARTBEAT_INTERVAL_SEC", 30*time.Second),
	}
}

func (c Config) Validate() error {
	if strings.TrimSpace(c.ControlPanelURL) == "" {
		return errors.New("CONTROL_PANEL_URL is required")
	}
	if strings.TrimSpace(c.Token) == "" {
		return errors.New("CONTROL_PANEL_TOKEN is required")
	}
	if strings.TrimSpace(c.ServiceID) == "" {
		return errors.New("SERVICE_ID is required")
	}
	if strings.TrimSpace(c.ServiceName) == "" {
		return errors.New("SERVICE_NAME is required")
	}
	if err := validateHTTPURL(c.ControlPanelURL, "CONTROL_PANEL_URL"); err != nil {
		return err
	}
	if err := validateHTTPURL(c.ServicePublicURL, "SERVICE_PUBLIC_URL"); err != nil {
		return err
	}
	return nil
}

func validateHTTPURL(raw, name string) error {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return errors.New(name + " must be an absolute URL")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return errors.New(name + " must use http or https")
	}
	if parsed.User != nil {
		return errors.New(name + " must not include credentials")
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New(name + " must not include query or fragment")
	}
	if parsed.Scheme == "http" && !isLocalDevHost(parsed.Hostname()) {
		return errors.New(name + " must use https for remote hosts")
	}
	return nil
}

func isLocalDevHost(host string) bool {
	normalized := strings.Trim(strings.ToLower(strings.TrimSpace(host)), "[]")
	return normalized == "localhost" || normalized == "127.0.0.1" || normalized == "host.docker.internal"
}

func (c Client) Register(ctx context.Context) error {
	body := Registration{
		ServiceID:   c.Config.ServiceID,
		ServiceType: ServiceType,
		ServiceName: c.Config.ServiceName,
		PublicURL:   c.Config.ServicePublicURL,
		Version:     c.Config.Version,
		Capabilities: map[string]any{
			"overlay_events":      true,
			"caption_events":      true,
			"participant_state":   true,
			"active_speaker":      true,
			"current_time_events": true,
			"health_endpoint":     true,
			"job_endpoint":        true,
		},
	}
	return c.post(ctx, "/services/register", body)
}

func (c Client) Heartbeat(ctx context.Context, status, currentStreamID string) error {
	return c.HeartbeatWithMetrics(ctx, status, currentStreamID, nil)
}

func (c Client) HeartbeatWithMetrics(ctx context.Context, status, currentStreamID string, metrics map[string]float64) error {
	if status == "" {
		status = "online"
	}
	return c.post(ctx, "/services/heartbeat", Heartbeat{ServiceID: c.Config.ServiceID, Status: status, CurrentStreamID: currentStreamID, Metrics: metrics})
}

func (c Client) RuntimeConfig(ctx context.Context) (RuntimeConfig, error) {
	endpoint := "/services/runtime-config?service_id=" + url.QueryEscape(c.Config.ServiceID)
	var cfg RuntimeConfig
	if err := c.get(ctx, endpoint, &cfg); err != nil {
		return RuntimeConfig{}, err
	}
	return cfg, nil
}

func (c Client) RunHeartbeatLoop(ctx context.Context, currentStreamID func() string, onError func(error)) {
	c.RunHeartbeatLoopWithMetrics(ctx, currentStreamID, nil, onError)
}

func (c Client) RunHeartbeatLoopWithMetrics(ctx context.Context, currentStreamID func() string, metrics func() map[string]float64, onError func(error)) {
	if currentStreamID == nil {
		currentStreamID = func() string { return "" }
	}
	if metrics == nil {
		metrics = func() map[string]float64 { return nil }
	}
	interval := c.Config.HeartbeatEvery
	if interval <= 0 {
		interval = 30 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if err := c.HeartbeatWithMetrics(ctx, "online", currentStreamID(), metrics()); err != nil && onError != nil {
			onError(err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (c Client) get(ctx context.Context, endpoint string, out any) error {
	if err := c.Config.Validate(); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, joinURL(c.Config.ControlPanelURL, endpoint), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Config.Token)
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
		return fmt.Errorf("control panel %s failed with status %d", endpoint, res.StatusCode)
	}
	return json.NewDecoder(res.Body).Decode(out)
}

func (c Client) post(ctx context.Context, endpoint string, payload any) error {
	if err := c.Config.Validate(); err != nil {
		return err
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, joinURL(c.Config.ControlPanelURL, endpoint), bytes.NewReader(body))
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
		return fmt.Errorf("control panel %s failed with status %d", endpoint, res.StatusCode)
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

func joinURL(baseURL, endpoint string) string {
	return strings.TrimRight(baseURL, "/") + "/" + strings.TrimLeft(endpoint, "/")
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
