package deepgram

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/coder/websocket"
)

type socketDialer interface {
	Dial(context.Context, string, http.Header) (socket, int, error)
}

type providerFailure interface {
	error
	SafeErrorClass() string
	SafeHTTPStatus() int
	Retryable() bool
}

type providerError struct {
	errorClass string
	httpStatus int
	retryable  bool
}

func (e *providerError) Error() string {
	return ErrUnavailable.Error()
}

func (e *providerError) Unwrap() error {
	return ErrUnavailable
}

func (e *providerError) SafeErrorClass() string {
	return e.errorClass
}

func (e *providerError) SafeHTTPStatus() int {
	return e.httpStatus
}

func (e *providerError) Retryable() bool {
	return e.retryable
}

func newProviderError(errorClass string, httpStatus int, retryable bool) error {
	return &providerError{
		errorClass: errorClass,
		httpStatus: safeHTTPStatus(httpStatus),
		retryable:  retryable,
	}
}

// SafeErrorClass returns only a bounded provider failure class. Raw transport
// errors, provider payloads, endpoints, and credentials are never returned.
func SafeErrorClass(err error) (string, bool) {
	var failure providerFailure
	if !errors.As(err, &failure) || !isSafeProviderErrorClass(failure.SafeErrorClass()) {
		return "", false
	}
	return failure.SafeErrorClass(), true
}

// SafeHTTPStatus returns a valid provider handshake status when one was
// available. Response bodies are never retained or exposed.
func SafeHTTPStatus(err error) (int, bool) {
	var failure providerFailure
	if !errors.As(err, &failure) {
		return 0, false
	}
	status := safeHTTPStatus(failure.SafeHTTPStatus())
	return status, status != 0
}

// IsRetryable reports whether retrying the same immutable session may
// converge. Authentication and request rejections require a new job/session.
func IsRetryable(err error) bool {
	var failure providerFailure
	if errors.As(err, &failure) {
		return failure.Retryable()
	}
	return errors.Is(err, ErrUnavailable) || errors.Is(err, context.DeadlineExceeded)
}

func safeHTTPStatus(status int) int {
	if status < 100 || status > 599 {
		return 0
	}
	return status
}

func isSafeProviderErrorClass(errorClass string) bool {
	switch errorClass {
	case "provider_auth_rejected",
		"provider_request_rejected",
		"provider_rate_limited",
		"provider_dial_timeout",
		"provider_unavailable",
		"provider_dial_failed",
		"provider_write_failed":
		return true
	default:
		return false
	}
}

func classifyDialFailure(status int, err error) error {
	status = safeHTTPStatus(status)
	switch {
	case errors.Is(err, context.Canceled):
		return context.Canceled
	case errors.Is(err, context.DeadlineExceeded), status == http.StatusRequestTimeout:
		return newProviderError("provider_dial_timeout", status, true)
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return newProviderError("provider_auth_rejected", status, false)
	case status == http.StatusTooManyRequests:
		return newProviderError("provider_rate_limited", status, true)
	case status == http.StatusConflict || status == http.StatusTooEarly || status >= http.StatusInternalServerError:
		return newProviderError("provider_unavailable", status, true)
	case status != 0 && status != http.StatusSwitchingProtocols:
		return newProviderError("provider_request_rejected", status, false)
	default:
		return newProviderError("provider_dial_failed", status, true)
	}
}

type socket interface {
	Read(context.Context) (websocket.MessageType, []byte, error)
	Write(context.Context, websocket.MessageType, []byte) error
	Close(websocket.StatusCode, string) error
	CloseNow() error
	SetReadLimit(int64)
}

type coderDialer struct {
	client *http.Client
}

func (d coderDialer) Dial(ctx context.Context, endpoint string, header http.Header) (socket, int, error) {
	conn, response, err := websocket.Dial(ctx, endpoint, &websocket.DialOptions{
		HTTPClient: d.client,
		HTTPHeader: header,
	})
	status := 0
	if response != nil {
		status = response.StatusCode
	}
	if err != nil {
		return nil, status, err
	}
	return conn, status, nil
}

func (s *Session) dial(ctx context.Context) (socket, error) {
	authorization := make([]byte, len("Token ")+len(s.apiKey))
	copy(authorization, "Token ")
	copy(authorization[len("Token "):], s.apiKey)
	header := make(http.Header)
	header.Set("Authorization", string(authorization))
	conn, status, err := s.options.dialer.Dial(ctx, s.endpoint, header)
	header.Del("Authorization")
	zeroBytes(authorization)
	if err != nil || conn == nil {
		return nil, classifyDialFailure(status, err)
	}
	conn.SetReadLimit(maxResultMessageBytes)
	return conn, nil
}

func classifyProviderReadError(err error) string {
	switch websocket.CloseStatus(err) {
	case websocket.StatusPolicyViolation:
		return "provider_audio_rejected"
	case websocket.StatusInternalError:
		return "provider_unavailable"
	default:
		return "provider_read_closed"
	}
}

func isProviderErrorMessage(payload []byte) bool {
	var envelope struct {
		Type string `json:"type"`
	}
	return json.Unmarshal(payload, &envelope) == nil && strings.EqualFold(strings.TrimSpace(envelope.Type), "error")
}
