package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/example/autostream-worker/internal/jobs"
	"github.com/example/autostream-worker/internal/observability"
	"github.com/example/autostream-worker/internal/sceneappearance"
)

func TestStartJobDecodesOptionalReadySceneAppearanceAndRejectsURLFields(t *testing.T) {
	manager := jobs.NewManager(nil, observability.Client{})
	handler := NewServer("worker", manager, TokenVerifier{PlainToken: "service-token"})
	valid := `{
		"stream_id":"stream-01",
		"scene_appearance":{
			"generation":9,
			"revision":4,
			"capability":"scene_appearance_v1",
			"readiness":"ready",
			"background_mode":"default",
			"header_title_mode":"default"
		}
	}`
	response := serveSceneAppearanceStart(t, handler, valid)
	if response.Code != http.StatusConflict || responseCode(t, response) != string(sceneappearance.CodeCapabilityRequired) {
		t.Fatalf("scene_appearance was not decoded into the start DTO: status=%d body=%s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "message") {
		t.Fatalf("visual error exposed a message: %s", response.Body.String())
	}

	withURL := strings.Replace(valid, `"background_mode":"default",`, `"background_mode":"default","asset_url":"https://forbidden.example/secret.png",`, 1)
	response = serveSceneAppearanceStart(t, handler, withURL)
	if response.Code != http.StatusBadRequest || responseCode(t, response) != "invalid_json" {
		t.Fatalf("URL-bearing scene payload was accepted: status=%d body=%s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "forbidden.example") || strings.Contains(response.Body.String(), "secret.png") {
		t.Fatalf("invalid scene payload reflected its URL: %s", response.Body.String())
	}
}

func TestWriteRequestErrorReturnsOnlyCanonicalVisualCode(t *testing.T) {
	for _, test := range []struct {
		code   sceneappearance.ErrorCode
		status int
	}{
		{sceneappearance.CodeMediaAssetUnauthorized, http.StatusForbidden},
		{sceneappearance.CodeMediaAssetNotFound, http.StatusNotFound},
		{sceneappearance.CodeMediaAssetTimeout, http.StatusGatewayTimeout},
		{sceneappearance.CodeMediaAssetTooLarge, http.StatusRequestEntityTooLarge},
		{sceneappearance.CodeMediaAssetVariantFailed, http.StatusBadGateway},
		{sceneappearance.CodeMediaAssetHashMismatch, http.StatusUnprocessableEntity},
		{sceneappearance.CodeStaleJobGeneration, http.StatusConflict},
		{sceneappearance.CodeRevisionPayloadConflict, http.StatusConflict},
		{sceneappearance.CodeCapabilityRequired, http.StatusConflict},
	} {
		recorder := httptest.NewRecorder()
		writeRequestError(recorder, sceneappearance.NewError(test.code))
		if recorder.Code != test.status || responseCode(t, recorder) != string(test.code) {
			t.Fatalf("code %q status=%d body=%s", test.code, recorder.Code, recorder.Body.String())
		}
		if strings.Contains(recorder.Body.String(), "message") {
			t.Fatalf("code %q exposed a message: %s", test.code, recorder.Body.String())
		}
	}
}

func serveSceneAppearanceStart(t *testing.T, handler http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/jobs/start", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer service-token")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}
