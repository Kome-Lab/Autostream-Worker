package jobs

import (
	"net/http"
	"path/filepath"
	"sync/atomic"
	"time"

	lifecycle "github.com/Kome-Lab/Autostream-Contracts/v3lifecycle"
	"github.com/example/autostream-worker/internal/control"
	"github.com/example/autostream-worker/internal/manualhold"
)

func (m *Manager) ConfigureStartAdmission() error {
	if m.StartAdmission != nil {
		return nil
	}
	stateFile := m.stoppedTargetReceiptPath
	if stateFile == "" {
		stateFile = DefaultStoppedTargetReceiptPath
	}
	directory, err := filepath.Abs(filepath.Join(filepath.Dir(stateFile), "start-admissions"))
	if err != nil {
		return manualhold.ErrUnknown
	}
	client := manualhold.HTTPInspector{
		Config: func() (string, string, error) {
			cfg := control.ConfigFromEnv()
			if cfg.ConfigError != "" || cfg.ConfigRevision <= 0 {
				return "", "", manualhold.ErrUnknown
			}
			return cfg.ControlPanelURL, cfg.Token, nil
		},
		HTTP: &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
	}
	guard, err := manualhold.New(directory, client, func() (lifecycle.ProducerRef, error) {
		cfg := control.ConfigFromEnv()
		if cfg.ConfigError != "" || cfg.ConfigRevision <= 0 {
			return lifecycle.ProducerRef{}, manualhold.ErrUnknown
		}
		return manualhold.LocalProducer("C06", cfg.ServiceID)
	})
	if err != nil {
		return err
	}
	var required atomic.Bool
	initial := control.ConfigFromEnv()
	required.Store(initial.ControlPanelURL != "" || initial.ConfigError != "")
	guard.Require = func() bool {
		cfg := control.ConfigFromEnv()
		if cfg.ControlPanelURL != "" || cfg.ConfigError != "" {
			required.Store(true)
		}
		return required.Load()
	}
	m.StartAdmission = guard
	return nil
}
