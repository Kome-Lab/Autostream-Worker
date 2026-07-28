package httpapi

import (
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/example/autostream-worker/internal/control"
)

var (
	ErrUpdaterIdentityPending = errors.New("updater identity is pending")
	ErrUpdaterIdentityDrift   = errors.New("updater identity drift detected")
)

type UpdaterIdentity struct {
	ServiceID      string
	ServiceType    string
	ConfigRevision int64
}

type UpdaterIdentityLatch struct {
	mu          sync.Mutex
	serviceType string
	identity    UpdaterIdentity
	bound       bool
}

func NewUpdaterIdentityLatch(serviceType string) *UpdaterIdentityLatch {
	return &UpdaterIdentityLatch{serviceType: strings.TrimSpace(serviceType)}
}

func (l *UpdaterIdentityLatch) ResolveFromEnv() (UpdaterIdentity, error) {
	if l == nil {
		return UpdaterIdentity{}, errors.New("updater identity latch is required")
	}
	if l.serviceType != control.ServiceType {
		return UpdaterIdentity{}, fmt.Errorf("updater service type must be %q", control.ServiceType)
	}
	revision, err := ConfigRevisionFromEnv()
	if err != nil {
		return UpdaterIdentity{}, err
	}
	if control.NodeConfigPendingFromEnv() {
		l.mu.Lock()
		defer l.mu.Unlock()
		if l.bound {
			return UpdaterIdentity{}, fmt.Errorf("%w: authoritative node config became unavailable", ErrUpdaterIdentityDrift)
		}
		return UpdaterIdentity{}, ErrUpdaterIdentityPending
	}
	cfg := control.ConfigFromEnv()
	if strings.TrimSpace(cfg.ConfigError) != "" {
		return UpdaterIdentity{}, errors.New(cfg.ConfigError)
	}
	return l.bind(UpdaterIdentity{
		ServiceID:      strings.TrimSpace(cfg.ServiceID),
		ServiceType:    control.ServiceType,
		ConfigRevision: revision,
	})
}

func (l *UpdaterIdentityLatch) bind(candidate UpdaterIdentity) (UpdaterIdentity, error) {
	if candidate.ServiceID == "" {
		return UpdaterIdentity{}, errors.New("updater service id is required")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.bound {
		l.identity = candidate
		l.bound = true
		return candidate, nil
	}
	if l.identity != candidate {
		return UpdaterIdentity{}, fmt.Errorf(
			"%w: expected %s/%s revision %d",
			ErrUpdaterIdentityDrift,
			l.identity.ServiceType,
			l.identity.ServiceID,
			l.identity.ConfigRevision,
		)
	}
	return l.identity, nil
}
