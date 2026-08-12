package scene

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	_ "golang.org/x/image/webp"
)

const (
	defaultAvatarMaxBytes      = 1 << 20
	defaultAvatarMaxDimension  = 1024
	defaultAvatarMaxPixels     = 1024 * 1024
	defaultAvatarMaxEntries    = 64
	defaultAvatarMaxConcurrent = 4
	defaultAvatarSuccessTTL    = 15 * time.Minute
	defaultAvatarFailureTTL    = time.Minute
)

type AvatarConfig struct {
	HTTPClient    *http.Client
	MaxBytes      int64
	MaxDimension  int
	MaxPixels     int
	MaxEntries    int
	MaxConcurrent int
	SuccessTTL    time.Duration
	FailureTTL    time.Duration
}

type avatarEntry struct {
	image     image.Image
	expiresAt time.Time
	lastUsed  time.Time
}

type avatarCache struct {
	mu           sync.Mutex
	client       *http.Client
	maxBytes     int64
	maxDimension int
	maxPixels    int
	maxEntries   int
	successTTL   time.Duration
	failureTTL   time.Duration
	semaphore    chan struct{}
	entries      map[string]avatarEntry
	inflight     map[string]bool
}

func newAvatarCache(config AvatarConfig) *avatarCache {
	if config.MaxBytes <= 0 {
		config.MaxBytes = defaultAvatarMaxBytes
	}
	if config.MaxDimension <= 0 {
		config.MaxDimension = defaultAvatarMaxDimension
	}
	if config.MaxPixels <= 0 {
		config.MaxPixels = defaultAvatarMaxPixels
	}
	if config.MaxEntries <= 0 {
		config.MaxEntries = defaultAvatarMaxEntries
	}
	if config.MaxConcurrent <= 0 {
		config.MaxConcurrent = defaultAvatarMaxConcurrent
	}
	if config.SuccessTTL <= 0 {
		config.SuccessTTL = defaultAvatarSuccessTTL
	}
	if config.FailureTTL <= 0 {
		config.FailureTTL = defaultAvatarFailureTTL
	}
	base := config.HTTPClient
	if base == nil {
		base = &http.Client{Timeout: 5 * time.Second}
	}
	clientCopy := *base
	if clientCopy.Timeout <= 0 || clientCopy.Timeout > 10*time.Second {
		clientCopy.Timeout = 5 * time.Second
	}
	previousRedirect := clientCopy.CheckRedirect
	clientCopy.CheckRedirect = func(request *http.Request, via []*http.Request) error {
		if len(via) >= 3 {
			return errors.New("avatar redirect limit exceeded")
		}
		if err := validateAvatarURL(request.URL.String()); err != nil {
			return err
		}
		if previousRedirect != nil {
			return previousRedirect(request, via)
		}
		return nil
	}
	return &avatarCache{
		client: &clientCopy, maxBytes: config.MaxBytes, maxDimension: config.MaxDimension,
		maxPixels: config.MaxPixels, maxEntries: config.MaxEntries,
		successTTL: config.SuccessTTL, failureTTL: config.FailureTTL,
		semaphore: make(chan struct{}, config.MaxConcurrent), entries: map[string]avatarEntry{}, inflight: map[string]bool{},
	}
}

func validateAvatarURL(raw string) error {
	if strings.TrimSpace(raw) == "" {
		return errors.New("avatar URL is empty")
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.User != nil || parsed.Fragment != "" {
		return errors.New("avatar URL must be an HTTPS URL without credentials or fragment")
	}
	if parsed.Port() != "" && parsed.Port() != "443" {
		return errors.New("avatar URL port is not allowed")
	}
	host := strings.ToLower(parsed.Hostname())
	if host != "cdn.discordapp.com" && host != "media.discordapp.net" {
		return errors.New("avatar URL host is not an allowed Discord CDN")
	}
	if !strings.HasPrefix(parsed.EscapedPath(), "/") || parsed.Path == "" {
		return errors.New("avatar URL path is invalid")
	}
	return nil
}

func (c *avatarCache) Prefetch(rawURL string) {
	if c == nil || validateAvatarURL(rawURL) != nil {
		return
	}
	now := time.Now()
	c.mu.Lock()
	if entry, ok := c.entries[rawURL]; ok && entry.expiresAt.After(now) {
		entry.lastUsed = now
		c.entries[rawURL] = entry
		c.mu.Unlock()
		return
	}
	if c.inflight[rawURL] {
		c.mu.Unlock()
		return
	}
	if len(c.inflight) >= c.maxEntries {
		c.mu.Unlock()
		return
	}
	c.inflight[rawURL] = true
	c.mu.Unlock()
	go c.load(rawURL)
}

func (c *avatarCache) Lookup(rawURL string) image.Image {
	if c == nil || rawURL == "" {
		return nil
	}
	now := time.Now()
	c.mu.Lock()
	entry, ok := c.entries[rawURL]
	if ok && entry.expiresAt.After(now) {
		entry.lastUsed = now
		c.entries[rawURL] = entry
		c.mu.Unlock()
		return entry.image
	}
	if ok {
		delete(c.entries, rawURL)
	}
	c.mu.Unlock()
	c.Prefetch(rawURL)
	return nil
}

func (c *avatarCache) refreshInterval() time.Duration {
	if c == nil || c.successTTL <= 0 {
		return defaultAvatarSuccessTTL
	}
	interval := c.successTTL / 2
	if interval < 30*time.Second {
		return 30 * time.Second
	}
	return interval
}

func (c *avatarCache) load(rawURL string) {
	c.semaphore <- struct{}{}
	img, err := c.fetch(context.Background(), rawURL)
	<-c.semaphore
	now := time.Now()
	ttl := c.successTTL
	if err != nil {
		img = nil
		ttl = c.failureTTL
	}
	c.mu.Lock()
	delete(c.inflight, rawURL)
	c.entries[rawURL] = avatarEntry{image: img, expiresAt: now.Add(ttl), lastUsed: now}
	c.evictLocked()
	c.mu.Unlock()
}

func (c *avatarCache) fetch(ctx context.Context, rawURL string) (image.Image, error) {
	if err := validateAvatarURL(rawURL); err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, errors.New("create avatar request")
	}
	request.Header.Set("Accept", "image/avif,image/webp,image/png,image/jpeg,image/gif")
	request.Header.Set("User-Agent", "AutoStream-Worker/scene-avatar")
	response, err := c.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("fetch avatar: %w", err)
	}
	defer response.Body.Close()
	if response.Request == nil || response.Request.URL == nil || validateAvatarURL(response.Request.URL.String()) != nil {
		return nil, errors.New("avatar response URL is not an allowed Discord CDN")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("fetch avatar returned HTTP %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, c.maxBytes+1))
	if err != nil {
		return nil, errors.New("read avatar response")
	}
	if int64(len(data)) > c.maxBytes {
		return nil, errors.New("avatar response exceeds byte limit")
	}
	config, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || config.Width <= 0 || config.Height <= 0 {
		return nil, errors.New("avatar image format is invalid")
	}
	if config.Width > c.maxDimension || config.Height > c.maxDimension || config.Width > c.maxPixels/config.Height {
		return nil, errors.New("avatar image dimensions exceed limit")
	}
	decoded, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, errors.New("decode avatar image")
	}
	return decoded, nil
}

func (c *avatarCache) evictLocked() {
	if len(c.entries) <= c.maxEntries {
		return
	}
	type candidate struct {
		url      string
		lastUsed time.Time
	}
	candidates := make([]candidate, 0, len(c.entries))
	for rawURL, entry := range c.entries {
		candidates = append(candidates, candidate{url: rawURL, lastUsed: entry.lastUsed})
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].lastUsed.Before(candidates[j].lastUsed) })
	for i := 0; len(c.entries) > c.maxEntries && i < len(candidates); i++ {
		delete(c.entries, candidates[i].url)
	}
}
