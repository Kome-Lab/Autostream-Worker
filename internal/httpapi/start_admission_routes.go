package httpapi

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/example/autostream-worker/internal/manualhold"
)

func writeStartAdmissionError(w http.ResponseWriter, err error) {
	status, result := http.StatusServiceUnavailable, "UNKNOWN"
	if !errors.Is(err, manualhold.ErrUnknown) && errors.Is(err, manualhold.ErrRefused) {
		status, result = http.StatusConflict, "REFUSED_UNCHANGED"
	}
	writeJSON(w, status, map[string]string{"code": "start_admission_" + result, "result": result})
}

func registerManualStartAdmissionRoutes(mux *http.ServeMux, guard *manualhold.Guard, authorized func(*http.Request) bool) {
	wrap := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "no-store")
			if !authorized(r) {
				writeJSON(w, http.StatusUnauthorized, map[string]string{"code": "missing_or_invalid_service_token"})
				return
			}
			if guard == nil {
				writeStartAdmissionError(w, manualhold.ErrUnknown)
				return
			}
			next(w, r)
		}
	}
	mux.HandleFunc("GET /start-admission/identity", wrap(func(w http.ResponseWriter, r *http.Request) {
		producer, err := guard.ProducerRef()
		if err != nil {
			writeStartAdmissionError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, producer)
	}))
	mux.HandleFunc("POST /streams/{id}/start-admission/close", wrap(func(w http.ResponseWriter, r *http.Request) {
		data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, manualhold.MaxRecordBytes))
		if err != nil {
			writeStartAdmissionError(w, manualhold.ErrRefused)
			return
		}
		record, err := manualhold.DecodeJSON(data)
		if err != nil {
			writeStartAdmissionError(w, err)
			return
		}
		ack, err := guard.Close(r.Context(), r.PathValue("id"), record)
		if err != nil {
			writeStartAdmissionError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, ack)
	}))
	mux.HandleFunc("POST /streams/{id}/start-admission/inspect", wrap(func(w http.ResponseWriter, r *http.Request) {
		data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, manualhold.MaxRecordBytes))
		if err != nil {
			writeStartAdmissionError(w, manualhold.ErrRefused)
			return
		}
		original, err := manualhold.DecodeJSON(data)
		if err != nil || manualhold.Validate(original, r.PathValue("id")) != nil {
			writeStartAdmissionError(w, manualhold.ErrRefused)
			return
		}
		record, observation, err := guard.Inspect(r.PathValue("id"), original.Header.Correlation.RequestID)
		if err != nil {
			writeStartAdmissionError(w, err)
			return
		}
		// A target ACK differs only in target record ownership/producer identity.
		if !manualhold.SameSource(original, record) {
			writeStartAdmissionError(w, manualhold.ErrRefused)
			return
		}
		if observation != nil {
			body, _ := json.Marshal(observation)
			w.Header().Set("X-AutoStream-Start-Outcome", base64.RawURLEncoding.EncodeToString(body))
		}
		writeJSON(w, http.StatusOK, record)
	}))
}
