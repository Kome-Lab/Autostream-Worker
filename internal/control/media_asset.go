package control

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/example/autostream-worker/internal/sceneappearance"
)

// FetchSceneAsset retrieves one Control Panel-authorized, stream-bound media
// variant. The request is derived only from the stream ID and safe descriptor;
// storage locations and user-supplied URLs never enter this client.
func (c Client) FetchSceneAsset(ctx context.Context, streamID string, descriptor sceneappearance.AssetDescriptor) (sceneappearance.AssetResponse, error) {
	if err := c.validateSceneAssetConfig(); err != nil {
		return sceneappearance.AssetResponse{}, err
	}
	streamID = strings.TrimSpace(streamID)
	if streamID == "" || strings.TrimSpace(descriptor.VariantID) == "" {
		return sceneappearance.AssetResponse{}, sceneappearance.NewError(sceneappearance.CodeRevisionPayloadConflict)
	}
	if descriptor.ByteSize < 1 || descriptor.ByteSize > 20<<20 {
		return sceneappearance.AssetResponse{}, sceneappearance.NewError(sceneappearance.CodeMediaAssetTooLarge)
	}
	switch descriptor.MediaType {
	case sceneappearance.MediaTypePNG, sceneappearance.MediaTypeJPEG, sceneappearance.MediaTypeWebP:
	default:
		return sceneappearance.AssetResponse{}, sceneappearance.NewError(sceneappearance.CodeMediaAssetFormatUnsupported)
	}
	endpoint := "/internal/streams/" + url.PathEscape(streamID) + "/media-assets/" + url.PathEscape(descriptor.VariantID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, joinURL(c.Config.ControlPanelURL, endpoint), nil)
	if err != nil {
		return sceneappearance.AssetResponse{}, sceneappearance.NewError(sceneappearance.CodeMediaAssetVariantFailed)
	}
	req.Header.Set("Authorization", "Bearer "+c.Config.Token)
	req.Header.Set("Accept", descriptor.MediaType)

	client := noRedirectClone(c.HTTP)
	res, err := client.Do(req)
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			return sceneappearance.AssetResponse{}, sceneappearance.NewError(sceneappearance.CodeMediaAssetTimeout)
		}
		return sceneappearance.AssetResponse{}, sceneappearance.NewError(sceneappearance.CodeMediaAssetVariantFailed)
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return sceneappearance.AssetResponse{}, sceneappearance.NewError(sceneAssetStatusCode(res.StatusCode))
	}

	mediaType, _, err := mime.ParseMediaType(res.Header.Get("Content-Type"))
	if err != nil || mediaType == "" {
		return sceneappearance.AssetResponse{}, sceneappearance.NewError(sceneappearance.CodeMediaAssetFormatUnsupported)
	}
	assetID := strings.TrimSpace(res.Header.Get("X-AutoStream-Asset-ID"))
	variantID := strings.TrimSpace(res.Header.Get("X-AutoStream-Variant-ID"))
	if assetID != descriptor.AssetID || variantID != descriptor.VariantID {
		return sceneappearance.AssetResponse{}, sceneappearance.NewError(sceneappearance.CodeRevisionPayloadConflict)
	}
	width, widthErr := strconv.Atoi(strings.TrimSpace(res.Header.Get("X-AutoStream-Width")))
	height, heightErr := strconv.Atoi(strings.TrimSpace(res.Header.Get("X-AutoStream-Height")))
	if widthErr != nil || heightErr != nil {
		return sceneappearance.AssetResponse{}, sceneappearance.NewError(sceneappearance.CodeMediaAssetDimensionMismatch)
	}
	digest, err := parseSceneAssetDigest(res.Header.Get("Digest"))
	if err != nil {
		return sceneappearance.AssetResponse{}, sceneappearance.NewError(sceneappearance.CodeMediaAssetHashMismatch)
	}
	if res.ContentLength > descriptor.ByteSize || res.ContentLength > 20<<20 {
		return sceneappearance.AssetResponse{}, sceneappearance.NewError(sceneappearance.CodeMediaAssetTooLarge)
	}
	if res.ContentLength < 0 || res.ContentLength != descriptor.ByteSize {
		return sceneappearance.AssetResponse{}, sceneappearance.NewError(sceneappearance.CodeMediaAssetDecodeFailed)
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, descriptor.ByteSize+1))
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			return sceneappearance.AssetResponse{}, sceneappearance.NewError(sceneappearance.CodeMediaAssetTimeout)
		}
		return sceneappearance.AssetResponse{}, sceneappearance.NewError(sceneappearance.CodeMediaAssetVariantFailed)
	}
	if int64(len(body)) > descriptor.ByteSize {
		zeroSceneAssetBytes(body)
		return sceneappearance.AssetResponse{}, sceneappearance.NewError(sceneappearance.CodeMediaAssetTooLarge)
	}
	if int64(len(body)) != descriptor.ByteSize {
		zeroSceneAssetBytes(body)
		return sceneappearance.AssetResponse{}, sceneappearance.NewError(sceneappearance.CodeMediaAssetDecodeFailed)
	}
	return sceneappearance.AssetResponse{
		AssetID: assetID, VariantID: variantID, MediaType: mediaType,
		Width: width, Height: height, ByteSize: int64(len(body)), SHA256: digest, Body: body,
	}, nil
}

func (c Client) validateSceneAssetConfig() error {
	if strings.TrimSpace(c.Config.ConfigError) != "" || strings.TrimSpace(c.Config.ControlPanelURL) == "" || strings.TrimSpace(c.Config.ServiceID) == "" {
		return sceneappearance.NewError(sceneappearance.CodeCapabilityRequired)
	}
	if strings.TrimSpace(c.Config.Token) == "" {
		return sceneappearance.NewError(sceneappearance.CodeMediaAssetUnauthorized)
	}
	if err := validateHTTPURL(c.Config.ControlPanelURL, "CONTROL_PANEL_URL"); err != nil {
		return sceneappearance.NewError(sceneappearance.CodeCapabilityRequired)
	}
	return nil
}

func sceneAssetStatusCode(status int) sceneappearance.ErrorCode {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return sceneappearance.CodeMediaAssetUnauthorized
	case http.StatusNotFound:
		return sceneappearance.CodeMediaAssetNotFound
	case http.StatusRequestTimeout, http.StatusGatewayTimeout:
		return sceneappearance.CodeMediaAssetTimeout
	case http.StatusRequestEntityTooLarge:
		return sceneappearance.CodeMediaAssetTooLarge
	case http.StatusUnsupportedMediaType:
		return sceneappearance.CodeMediaAssetFormatUnsupported
	case http.StatusConflict:
		return sceneappearance.CodeMediaAssetHashMismatch
	default:
		return sceneappearance.CodeMediaAssetVariantFailed
	}
}

func parseSceneAssetDigest(value string) (string, error) {
	const prefix = "sha-256="
	value = strings.TrimSpace(value)
	if !strings.HasPrefix(value, prefix) || strings.Contains(value, ",") {
		return "", errors.New("invalid digest")
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(value, prefix))
	if err != nil || len(decoded) != 32 {
		return "", errors.New("invalid digest")
	}
	return hex.EncodeToString(decoded), nil
}

func noRedirectClone(configured *http.Client) *http.Client {
	client := &http.Client{}
	if configured != nil {
		*client = *configured
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return client
}

func zeroSceneAssetBytes(value []byte) {
	for index := range value {
		value[index] = 0
	}
}
