package sceneappearance

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"image"
	"image/draw"
	"image/jpeg"
	"image/png"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/image/webp"
)

const (
	Capability = "scene_appearance_v1"

	ReadinessReady    Readiness = "ready"
	ReadinessNotReady Readiness = "not_ready"
	ReadinessUnknown  Readiness = "unknown"

	BackgroundModeDefault  = "default"
	BackgroundModeImage    = "image"
	HeaderTitleModeDefault = "default"
	HeaderTitleModeCustom  = "custom"

	UsageSceneBackground = "scene_background"

	MediaTypePNG  = "image/png"
	MediaTypeJPEG = "image/jpeg"
	MediaTypeWebP = "image/webp"

	CodeMediaAssetFormatUnsupported  ErrorCode = "media_asset_format_unsupported"
	CodeMediaAssetTooLarge           ErrorCode = "media_asset_too_large"
	CodeMediaAssetDecodeFailed       ErrorCode = "media_asset_decode_failed"
	CodeMediaAssetAspectRatioInvalid ErrorCode = "media_asset_aspect_ratio_invalid"
	CodeMediaAssetVariantProcessing  ErrorCode = "media_asset_variant_processing"
	CodeMediaAssetVariantFailed      ErrorCode = "media_asset_variant_failed"
	CodeMediaAssetUnauthorized       ErrorCode = "media_asset_unauthorized"
	CodeMediaAssetNotFound           ErrorCode = "media_asset_not_found"
	CodeMediaAssetHashMismatch       ErrorCode = "media_asset_hash_mismatch"
	CodeMediaAssetDimensionMismatch  ErrorCode = "media_asset_dimension_mismatch"
	CodeMediaAssetTimeout            ErrorCode = "media_asset_timeout"
	CodeStaleJobGeneration           ErrorCode = "stale_job_generation"
	CodeRevisionPayloadConflict      ErrorCode = "revision_payload_conflict"
	CodeCapabilityRequired           ErrorCode = "capability_required"

	maxAssetBytes  int64 = 20 << 20
	maxAssetPixels int64 = 40_000_000
	maxDimension         = 8192
)

var (
	safeIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
	digestPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)
)

type Readiness string
type ErrorCode string

type SafeError struct {
	Code      ErrorCode `json:"code"`
	RequestID string    `json:"request_id,omitempty"`
}

// AssetDescriptor is the safe, immutable processed-variant metadata from the
// Control Panel start snapshot. It deliberately has no URL, path, storage key,
// authorization material, Base64 payload, or raw media field.
type AssetDescriptor struct {
	AssetID             string     `json:"asset_id"`
	VariantID           string     `json:"variant_id"`
	Usage               string     `json:"usage"`
	MediaType           string     `json:"media_type"`
	Width               int        `json:"width"`
	Height              int        `json:"height"`
	ByteSize            int64      `json:"byte_size"`
	PixelCount          int64      `json:"pixel_count"`
	Animated            bool       `json:"animated"`
	AspectRatioErrorPPM *int       `json:"aspect_ratio_error_ppm,omitempty"`
	Opaque              *bool      `json:"opaque,omitempty"`
	SHA256              string     `json:"sha256"`
	Revision            uint64     `json:"revision"`
	Readiness           Readiness  `json:"readiness"`
	Error               *SafeError `json:"error,omitempty"`
}

// Snapshot is the ready-only, Control Panel-owned visual configuration bound
// to one stream start. Generation is the Control Panel visual runtime epoch;
// it is intentionally not the Worker job generation.
type Snapshot struct {
	Generation      uint64           `json:"generation"`
	Revision        uint64           `json:"revision"`
	Capability      string           `json:"capability"`
	Readiness       Readiness        `json:"readiness"`
	BackgroundMode  string           `json:"background_mode"`
	Background      *AssetDescriptor `json:"background,omitempty"`
	HeaderTitleMode string           `json:"header_title_mode"`
	CustomTitle     string           `json:"custom_title,omitempty"`
	Error           *SafeError       `json:"error,omitempty"`
}

// AssetResponse is the authenticated internal response. Body exists only at
// the fetch/runtime seam and is zeroed after validation and decode.
type AssetResponse struct {
	AssetID   string
	VariantID string
	MediaType string
	Width     int
	Height    int
	ByteSize  int64
	SHA256    string
	Body      []byte
}

type Fetcher interface {
	FetchSceneAsset(context.Context, string, AssetDescriptor) (AssetResponse, error)
}

type FetcherFunc func(context.Context, string, AssetDescriptor) (AssetResponse, error)

func (f FetcherFunc) FetchSceneAsset(ctx context.Context, streamID string, descriptor AssetDescriptor) (AssetResponse, error) {
	if f == nil {
		return AssetResponse{}, NewError(CodeCapabilityRequired)
	}
	return f(ctx, streamID, descriptor)
}

type Options struct {
	FetchTimeout  time.Duration
	MaxEntries    int
	MaxCacheBytes int64
}

type Prepared struct {
	Generation      uint64
	Revision        uint64
	BackgroundMode  string
	Background      image.Image
	HeaderTitleMode string
	CustomTitle     string
}

func (p Prepared) IsDefault() bool {
	return p.Background == nil && p.CustomTitle == "" &&
		(p.BackgroundMode == "" || p.BackgroundMode == BackgroundModeDefault) &&
		(p.HeaderTitleMode == "" || p.HeaderTitleMode == HeaderTitleModeDefault)
}

type Error struct{ code ErrorCode }

func NewError(code ErrorCode) error {
	if !isCanonicalCode(code) {
		code = CodeMediaAssetVariantFailed
	}
	return &Error{code: code}
}

func (e *Error) Error() string {
	if e == nil {
		return string(CodeMediaAssetVariantFailed)
	}
	return string(e.code)
}

func CodeOf(err error) (ErrorCode, bool) {
	var target *Error
	if !errors.As(err, &target) || target == nil || !isCanonicalCode(target.code) {
		return "", false
	}
	return target.code, true
}

func isCanonicalCode(code ErrorCode) bool {
	switch code {
	case CodeMediaAssetFormatUnsupported, CodeMediaAssetTooLarge,
		CodeMediaAssetDecodeFailed, CodeMediaAssetAspectRatioInvalid,
		CodeMediaAssetVariantProcessing, CodeMediaAssetVariantFailed,
		CodeMediaAssetUnauthorized, CodeMediaAssetNotFound,
		CodeMediaAssetHashMismatch, CodeMediaAssetDimensionMismatch,
		CodeMediaAssetTimeout, CodeStaleJobGeneration, CodeRevisionPayloadConflict,
		CodeCapabilityRequired:
		return true
	default:
		return false
	}
}

type cacheKey struct {
	streamID   string
	generation uint64
	assetID    string
	variantID  string
	revision   uint64
}

type descriptorIdentity struct {
	assetID    string
	variantID  string
	mediaType  string
	width      int
	height     int
	byteSize   int64
	pixelCount int64
	sha256     string
	revision   uint64
}

type cacheEntry struct {
	identity descriptorIdentity
	image    *image.RGBA
	bytes    int64
	lastUsed uint64
}

type appearanceIdentity struct {
	revision        uint64
	backgroundMode  string
	headerTitleMode string
	customTitle     string
	hasBackground   bool
	background      descriptorIdentity
}

type generationBinding struct {
	generation uint64
	identity   appearanceIdentity
	lastUsed   uint64
}

type Runtime struct {
	fetcher       Fetcher
	fetchTimeout  time.Duration
	maxEntries    int
	maxCacheBytes int64

	mu         sync.Mutex
	closed     bool
	cache      map[cacheKey]*cacheEntry
	cacheBytes int64
	bindings   map[string]generationBinding
	clock      uint64
}

func New(fetcher Fetcher, options Options) *Runtime {
	if options.FetchTimeout <= 0 {
		options.FetchTimeout = 5 * time.Second
	}
	if options.MaxEntries <= 0 {
		options.MaxEntries = 8
	}
	if options.MaxCacheBytes <= 0 {
		options.MaxCacheBytes = 256 << 20
	}
	return &Runtime{
		fetcher: fetcher, fetchTimeout: options.FetchTimeout,
		maxEntries: options.MaxEntries, maxCacheBytes: options.MaxCacheBytes,
		cache: map[cacheKey]*cacheEntry{}, bindings: map[string]generationBinding{},
	}
}

func (r *Runtime) Prepare(ctx context.Context, streamID string, snapshot *Snapshot) (Prepared, error) {
	if r == nil {
		return Prepared{}, NewError(CodeCapabilityRequired)
	}
	r.mu.Lock()
	closed := r.closed
	r.mu.Unlock()
	if closed {
		return Prepared{}, NewError(CodeCapabilityRequired)
	}
	if snapshot == nil {
		return Prepared{}, nil
	}
	streamID = strings.TrimSpace(streamID)
	if streamID == "" {
		return Prepared{}, NewError(CodeRevisionPayloadConflict)
	}
	prepared, descriptor, err := validateSnapshot(*snapshot)
	if err != nil {
		return Prepared{}, err
	}
	if err := r.bindGeneration(streamID, *snapshot, descriptor); err != nil {
		return Prepared{}, err
	}
	if descriptor == nil {
		return prepared, nil
	}
	if r.fetcher == nil {
		return Prepared{}, NewError(CodeCapabilityRequired)
	}

	key := cacheKey{streamID: streamID, generation: snapshot.Generation, assetID: descriptor.AssetID, variantID: descriptor.VariantID, revision: descriptor.Revision}
	identity := identityFor(*descriptor)
	if cached, err := r.cached(key, identity); cached != nil || err != nil {
		if err != nil {
			return Prepared{}, err
		}
		prepared.Background = cached
		return prepared, nil
	}

	fetchCtx, cancel := context.WithTimeout(ctx, r.fetchTimeout)
	defer cancel()
	response, err := r.fetcher.FetchSceneAsset(fetchCtx, streamID, *descriptor)
	if err != nil {
		zeroBytes(response.Body)
		return Prepared{}, normalizeFetchError(fetchCtx, err)
	}
	defer zeroBytes(response.Body)
	if err := fetchCtx.Err(); err != nil {
		return Prepared{}, NewError(CodeMediaAssetTimeout)
	}
	decoded, err := validateAndDecode(*descriptor, response)
	if err != nil {
		return Prepared{}, err
	}
	if err := fetchCtx.Err(); err != nil {
		zeroBytes(decoded.Pix)
		return Prepared{}, NewError(CodeMediaAssetTimeout)
	}
	stored, err := r.store(key, identity, decoded)
	if err != nil {
		zeroBytes(decoded.Pix)
		return Prepared{}, err
	}
	prepared.Background = stored
	return prepared, nil
}

func (r *Runtime) cached(key cacheKey, identity descriptorIdentity) (image.Image, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, NewError(CodeCapabilityRequired)
	}
	entry := r.cache[key]
	if entry == nil {
		return nil, nil
	}
	if entry.identity != identity {
		return nil, NewError(CodeRevisionPayloadConflict)
	}
	r.clock++
	entry.lastUsed = r.clock
	return entry.image, nil
}

func (r *Runtime) store(key cacheKey, identity descriptorIdentity, decoded *image.RGBA) (image.Image, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, NewError(CodeCapabilityRequired)
	}
	if existing := r.cache[key]; existing != nil {
		if existing.identity != identity {
			return nil, NewError(CodeRevisionPayloadConflict)
		}
		zeroBytes(decoded.Pix)
		r.clock++
		existing.lastUsed = r.clock
		return existing.image, nil
	}
	entryBytes := int64(len(decoded.Pix))
	if entryBytes > r.maxCacheBytes {
		return nil, NewError(CodeMediaAssetTooLarge)
	}
	// A new processed revision replaces older revisions only after the new
	// payload has been fetched, verified, and decoded successfully.
	for candidate, entry := range r.cache {
		if candidate.streamID == key.streamID && candidate != key {
			r.removeLocked(candidate, entry)
		}
	}
	r.clock++
	r.cache[key] = &cacheEntry{identity: identity, image: decoded, bytes: entryBytes, lastUsed: r.clock}
	r.cacheBytes += entryBytes
	for len(r.cache) > r.maxEntries || r.cacheBytes > r.maxCacheBytes {
		var oldestKey cacheKey
		var oldest *cacheEntry
		for candidate, entry := range r.cache {
			if candidate == key && len(r.cache) == 1 {
				continue
			}
			if oldest == nil || entry.lastUsed < oldest.lastUsed {
				oldestKey, oldest = candidate, entry
			}
		}
		if oldest == nil {
			break
		}
		r.removeLocked(oldestKey, oldest)
	}
	return decoded, nil
}

func (r *Runtime) removeLocked(key cacheKey, entry *cacheEntry) {
	delete(r.cache, key)
	if entry == nil {
		return
	}
	zeroBytes(entry.image.Pix)
	r.cacheBytes -= entry.bytes
	if r.cacheBytes < 0 {
		r.cacheBytes = 0
	}
}

func (r *Runtime) Close() {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	r.closed = true
	for key, entry := range r.cache {
		r.removeLocked(key, entry)
	}
	r.cache = nil
	r.bindings = nil
}

func (r *Runtime) bindGeneration(streamID string, snapshot Snapshot, descriptor *AssetDescriptor) error {
	identity := appearanceIdentity{
		revision: snapshot.Revision, backgroundMode: snapshot.BackgroundMode,
		headerTitleMode: snapshot.HeaderTitleMode, customTitle: snapshot.CustomTitle,
	}
	if descriptor != nil {
		identity.hasBackground = true
		identity.background = identityFor(*descriptor)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return NewError(CodeCapabilityRequired)
	}
	if existing, ok := r.bindings[streamID]; ok {
		switch {
		case snapshot.Generation < existing.generation:
			return NewError(CodeStaleJobGeneration)
		case snapshot.Generation == existing.generation && identity != existing.identity:
			return NewError(CodeRevisionPayloadConflict)
		case snapshot.Generation == existing.generation:
			r.clock++
			existing.lastUsed = r.clock
			r.bindings[streamID] = existing
			return nil
		}
	}
	r.clock++
	r.bindings[streamID] = generationBinding{generation: snapshot.Generation, identity: identity, lastUsed: r.clock}
	const maxGenerationBindings = 128
	for len(r.bindings) > maxGenerationBindings {
		oldestStream := ""
		var oldest generationBinding
		for candidate, binding := range r.bindings {
			if candidate == streamID {
				continue
			}
			if oldestStream == "" || binding.lastUsed < oldest.lastUsed {
				oldestStream, oldest = candidate, binding
			}
		}
		if oldestStream == "" {
			break
		}
		delete(r.bindings, oldestStream)
	}
	return nil
}

func validateSnapshot(snapshot Snapshot) (Prepared, *AssetDescriptor, error) {
	if snapshot.Generation == 0 || snapshot.Revision == 0 || snapshot.Capability != Capability {
		return Prepared{}, nil, NewError(CodeCapabilityRequired)
	}
	if snapshot.Readiness != ReadinessReady || snapshot.Error != nil {
		if snapshot.Error != nil && isCanonicalCode(snapshot.Error.Code) {
			return Prepared{}, nil, NewError(snapshot.Error.Code)
		}
		if snapshot.Readiness == ReadinessNotReady {
			return Prepared{}, nil, NewError(CodeMediaAssetVariantProcessing)
		}
		return Prepared{}, nil, NewError(CodeMediaAssetVariantFailed)
	}
	prepared := Prepared{
		Generation: snapshot.Generation, Revision: snapshot.Revision,
		BackgroundMode: snapshot.BackgroundMode, HeaderTitleMode: snapshot.HeaderTitleMode,
	}
	switch snapshot.BackgroundMode {
	case BackgroundModeDefault:
		if snapshot.Background != nil {
			return Prepared{}, nil, NewError(CodeRevisionPayloadConflict)
		}
	case BackgroundModeImage:
		if snapshot.Background == nil {
			return Prepared{}, nil, NewError(CodeRevisionPayloadConflict)
		}
		if err := validateDescriptor(*snapshot.Background); err != nil {
			return Prepared{}, nil, err
		}
	default:
		return Prepared{}, nil, NewError(CodeRevisionPayloadConflict)
	}
	switch snapshot.HeaderTitleMode {
	case HeaderTitleModeDefault:
		if snapshot.CustomTitle != "" {
			return Prepared{}, nil, NewError(CodeRevisionPayloadConflict)
		}
	case HeaderTitleModeCustom:
		title, ok := sanitizeCustomTitle(snapshot.CustomTitle)
		if !ok {
			return Prepared{}, nil, NewError(CodeRevisionPayloadConflict)
		}
		prepared.CustomTitle = title
	default:
		return Prepared{}, nil, NewError(CodeRevisionPayloadConflict)
	}
	return prepared, snapshot.Background, nil
}

func validateDescriptor(descriptor AssetDescriptor) error {
	if !safeIDPattern.MatchString(descriptor.AssetID) || !safeIDPattern.MatchString(descriptor.VariantID) || descriptor.Revision == 0 {
		return NewError(CodeRevisionPayloadConflict)
	}
	if descriptor.Usage != UsageSceneBackground || descriptor.AspectRatioErrorPPM != nil || descriptor.Opaque != nil {
		return NewError(CodeRevisionPayloadConflict)
	}
	if descriptor.Readiness != ReadinessReady || descriptor.Error != nil {
		if descriptor.Error != nil && isCanonicalCode(descriptor.Error.Code) {
			return NewError(descriptor.Error.Code)
		}
		if descriptor.Readiness == ReadinessNotReady {
			return NewError(CodeMediaAssetVariantProcessing)
		}
		return NewError(CodeMediaAssetVariantFailed)
	}
	if descriptor.Animated {
		return NewError(CodeMediaAssetFormatUnsupported)
	}
	switch descriptor.MediaType {
	case MediaTypePNG, MediaTypeJPEG, MediaTypeWebP:
	default:
		return NewError(CodeMediaAssetFormatUnsupported)
	}
	if descriptor.Width < 1 || descriptor.Width > maxDimension || descriptor.Height < 1 || descriptor.Height > maxDimension {
		return NewError(CodeMediaAssetDimensionMismatch)
	}
	pixels := int64(descriptor.Width) * int64(descriptor.Height)
	if pixels < 1 || pixels > maxAssetPixels || descriptor.PixelCount != pixels {
		return NewError(CodeMediaAssetDimensionMismatch)
	}
	if descriptor.ByteSize < 1 || descriptor.ByteSize > maxAssetBytes {
		return NewError(CodeMediaAssetTooLarge)
	}
	if !digestPattern.MatchString(descriptor.SHA256) {
		return NewError(CodeMediaAssetHashMismatch)
	}
	return nil
}

func sanitizeCustomTitle(value string) (string, bool) {
	if value == "" || !utf8.ValidString(value) || strings.ContainsAny(value, "\r\n") || utf8.RuneCountInString(value) > 80 {
		return "", false
	}
	value = strings.Map(func(char rune) rune {
		if unicode.IsControl(char) {
			return ' '
		}
		return char
	}, value)
	value = strings.Join(strings.Fields(value), " ")
	return value, value != "" && utf8.RuneCountInString(value) <= 80
}

func identityFor(descriptor AssetDescriptor) descriptorIdentity {
	return descriptorIdentity{
		assetID: descriptor.AssetID, variantID: descriptor.VariantID,
		mediaType: descriptor.MediaType, width: descriptor.Width, height: descriptor.Height,
		byteSize: descriptor.ByteSize, pixelCount: descriptor.PixelCount,
		sha256: descriptor.SHA256, revision: descriptor.Revision,
	}
}

func normalizeFetchError(ctx context.Context, err error) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return NewError(CodeMediaAssetTimeout)
	}
	if code, ok := CodeOf(err); ok {
		return NewError(code)
	}
	return NewError(CodeMediaAssetVariantFailed)
}

func validateAndDecode(descriptor AssetDescriptor, response AssetResponse) (*image.RGBA, error) {
	if response.AssetID != descriptor.AssetID || response.VariantID != descriptor.VariantID {
		return nil, NewError(CodeRevisionPayloadConflict)
	}
	if response.MediaType != descriptor.MediaType {
		return nil, NewError(CodeMediaAssetFormatUnsupported)
	}
	if response.Width != descriptor.Width || response.Height != descriptor.Height {
		return nil, NewError(CodeMediaAssetDimensionMismatch)
	}
	if response.ByteSize > descriptor.ByteSize || int64(len(response.Body)) > descriptor.ByteSize {
		return nil, NewError(CodeMediaAssetTooLarge)
	}
	if response.ByteSize != descriptor.ByteSize || int64(len(response.Body)) != descriptor.ByteSize {
		return nil, NewError(CodeMediaAssetDecodeFailed)
	}
	if response.SHA256 != descriptor.SHA256 {
		return nil, NewError(CodeMediaAssetHashMismatch)
	}
	digest := sha256.Sum256(response.Body)
	if hex.EncodeToString(digest[:]) != descriptor.SHA256 {
		return nil, NewError(CodeMediaAssetHashMismatch)
	}
	if !contentMatchesMediaType(response.Body, descriptor.MediaType) || isAnimated(response.Body, descriptor.MediaType) {
		return nil, NewError(CodeMediaAssetFormatUnsupported)
	}
	config, err := decodeConfig(response.Body, descriptor.MediaType)
	if err != nil {
		return nil, NewError(CodeMediaAssetDecodeFailed)
	}
	if config.Width != descriptor.Width || config.Height != descriptor.Height {
		return nil, NewError(CodeMediaAssetDimensionMismatch)
	}
	decoded, err := decodeImage(response.Body, descriptor.MediaType)
	if err != nil {
		return nil, NewError(CodeMediaAssetDecodeFailed)
	}
	if decoded.Bounds().Dx() != descriptor.Width || decoded.Bounds().Dy() != descriptor.Height {
		return nil, NewError(CodeMediaAssetDimensionMismatch)
	}
	rgba := image.NewRGBA(image.Rect(0, 0, descriptor.Width, descriptor.Height))
	draw.Draw(rgba, rgba.Bounds(), decoded, decoded.Bounds().Min, draw.Src)
	return rgba, nil
}

func contentMatchesMediaType(body []byte, mediaType string) bool {
	switch mediaType {
	case MediaTypePNG:
		return len(body) >= 8 && bytes.Equal(body[:8], []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'})
	case MediaTypeJPEG:
		return len(body) >= 3 && body[0] == 0xff && body[1] == 0xd8 && body[2] == 0xff
	case MediaTypeWebP:
		return len(body) >= 12 && bytes.Equal(body[:4], []byte("RIFF")) && bytes.Equal(body[8:12], []byte("WEBP"))
	default:
		return false
	}
}

func isAnimated(body []byte, mediaType string) bool {
	switch mediaType {
	case MediaTypePNG:
		if len(body) < 8 {
			return false
		}
		for offset := 8; offset+12 <= len(body); {
			length64 := int64(binary.BigEndian.Uint32(body[offset : offset+4]))
			if length64 > int64(len(body)-offset-12) {
				return false
			}
			length := int(length64)
			if bytes.Equal(body[offset+4:offset+8], []byte("acTL")) {
				return true
			}
			offset += 12 + length
		}
	case MediaTypeWebP:
		if len(body) < 12 {
			return false
		}
		for offset := 12; offset+8 <= len(body); {
			length64 := int64(binary.LittleEndian.Uint32(body[offset+4 : offset+8]))
			paddedLength64 := length64 + length64%2
			if paddedLength64 > int64(len(body)-offset-8) {
				return false
			}
			length := int(length64)
			kind := body[offset : offset+4]
			if bytes.Equal(kind, []byte("ANIM")) || bytes.Equal(kind, []byte("ANMF")) {
				return true
			}
			offset += 8 + length + length%2
		}
	}
	return false
}

func decodeConfig(body []byte, mediaType string) (image.Config, error) {
	reader := bytes.NewReader(body)
	switch mediaType {
	case MediaTypePNG:
		return png.DecodeConfig(reader)
	case MediaTypeJPEG:
		return jpeg.DecodeConfig(reader)
	case MediaTypeWebP:
		return webp.DecodeConfig(reader)
	default:
		return image.Config{}, NewError(CodeMediaAssetFormatUnsupported)
	}
}

func decodeImage(body []byte, mediaType string) (image.Image, error) {
	reader := bytes.NewReader(body)
	switch mediaType {
	case MediaTypePNG:
		return png.Decode(reader)
	case MediaTypeJPEG:
		return jpeg.Decode(reader)
	case MediaTypeWebP:
		return webp.Decode(reader)
	default:
		return nil, NewError(CodeMediaAssetFormatUnsupported)
	}
}

func zeroBytes(value []byte) {
	for index := range value {
		value[index] = 0
	}
}
