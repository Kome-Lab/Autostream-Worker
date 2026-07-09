package ingesttoken

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

const Prefix = "ast_ingest_v1"

type Claims struct {
	StreamID    string `json:"stream_id"`
	ServiceID   string `json:"service_id"`
	ServiceType string `json:"service_type"`
	Purpose     string `json:"purpose"`
	Audience    string `json:"audience"`
	ExpiresAt   int64  `json:"exp"`
}

type Expected struct {
	StreamID    string
	ServiceID   string
	ServiceType string
	Purpose     string
	Audience    string
	Now         time.Time
}

func Issue(secret string, claims Claims) (string, error) {
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return "", errors.New("ingest token signing key is required")
	}
	if strings.TrimSpace(claims.StreamID) == "" || strings.TrimSpace(claims.ServiceID) == "" || strings.TrimSpace(claims.ServiceType) == "" || strings.TrimSpace(claims.Purpose) == "" || strings.TrimSpace(claims.Audience) == "" || claims.ExpiresAt <= 0 {
		return "", errors.New("ingest token claims are incomplete")
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	encodedPayload := base64.RawURLEncoding.EncodeToString(payload)
	sig := sign(secret, encodedPayload)
	return Prefix + "." + encodedPayload + "." + sig, nil
}

func Verify(secret, token string, expected Expected) (Claims, error) {
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return Claims{}, errors.New("ingest token signing key is required")
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] != Prefix {
		return Claims{}, errors.New("invalid ingest token format")
	}
	wantSig := sign(secret, parts[1])
	if !hmac.Equal([]byte(parts[2]), []byte(wantSig)) {
		return Claims{}, errors.New("invalid ingest token signature")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return Claims{}, errors.New("invalid ingest token payload")
	}
	var claims Claims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return Claims{}, errors.New("invalid ingest token claims")
	}
	now := expected.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if claims.ExpiresAt <= 0 || now.UTC().Unix() > claims.ExpiresAt {
		return Claims{}, errors.New("ingest token expired")
	}
	if expected.StreamID != "" && claims.StreamID != expected.StreamID {
		return Claims{}, errors.New("ingest token stream mismatch")
	}
	if expected.ServiceID != "" && claims.ServiceID != expected.ServiceID {
		return Claims{}, errors.New("ingest token service id mismatch")
	}
	if expected.ServiceType != "" && claims.ServiceType != expected.ServiceType {
		return Claims{}, errors.New("ingest token service type mismatch")
	}
	if expected.Purpose != "" && claims.Purpose != expected.Purpose {
		return Claims{}, errors.New("ingest token purpose mismatch")
	}
	if expected.Audience != "" && claims.Audience != expected.Audience {
		return Claims{}, errors.New("ingest token audience mismatch")
	}
	return claims, nil
}

func IsSigned(token string) bool {
	return strings.HasPrefix(token, Prefix+".")
}

func sign(secret, payload string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(Prefix))
	mac.Write([]byte("."))
	mac.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
