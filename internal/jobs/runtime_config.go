package jobs

import (
	"strings"

	"github.com/example/autostream-worker/internal/control"
)

type ProfileDefaults struct {
	OverlayProfileID string
	CaptionProfileID string
}

type AssignmentPolicy struct {
	Enforce        bool
	PrimaryStreams map[string]bool
}

func (m *Manager) SetAssignmentPolicy(policy AssignmentPolicy) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.assignments = AssignmentPolicy{Enforce: policy.Enforce, PrimaryStreams: map[string]bool{}}
	for streamID, allowed := range policy.PrimaryStreams {
		streamID = strings.TrimSpace(streamID)
		if streamID != "" && allowed {
			m.assignments.PrimaryStreams[streamID] = true
		}
	}
}

func (m *Manager) SetProfileDefaults(defaults ProfileDefaults) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.defaults = ProfileDefaults{
		OverlayProfileID: strings.TrimSpace(defaults.OverlayProfileID),
		CaptionProfileID: strings.TrimSpace(defaults.CaptionProfileID),
	}
}

func (m *Manager) ApplyRuntimeConfig(cfg control.RuntimeConfig) {
	m.SetProfileDefaults(ProfileDefaultsFromRuntimeConfig(cfg))
	m.SetAssignmentPolicy(AssignmentPolicyFromRuntimeConfig(cfg))
	m.mu.Lock()
	m.captionProfiles = captionProfilesFromRuntimeConfig(cfg)
	m.mu.Unlock()
}

func ProfileDefaultsFromRuntimeConfig(cfg control.RuntimeConfig) ProfileDefaults {
	defaults := ProfileDefaults{}
	if profile, ok := firstRuntimeProfileForService(cfg.Profiles["overlay"], cfg.Service.ServiceID); ok {
		defaults.OverlayProfileID = profile.ID
	}
	if profile, ok := firstRuntimeProfileForService(cfg.Profiles["caption"], cfg.Service.ServiceID); ok {
		defaults.CaptionProfileID = profile.ID
	}
	return defaults
}

func captionProfilesFromRuntimeConfig(cfg control.RuntimeConfig) map[string]control.RuntimeProfile {
	profiles := map[string]control.RuntimeProfile{}
	for _, profile := range cfg.Profiles["caption"] {
		profile.ID = strings.TrimSpace(profile.ID)
		if profile.ID == "" || (profile.Kind != "" && profile.Kind != "caption") || !profileBelongsToService(profile, cfg.Service.ServiceID) {
			continue
		}
		profile.Config = cloneConfig(profile.Config)
		profiles[profile.ID] = profile
	}
	return profiles
}

func AssignmentPolicyFromRuntimeConfig(cfg control.RuntimeConfig) AssignmentPolicy {
	policy := AssignmentPolicy{Enforce: true, PrimaryStreams: map[string]bool{}}
	serviceID := strings.TrimSpace(cfg.Service.ServiceID)
	for _, assignment := range cfg.Assignments {
		if assignment.ServiceType != control.ServiceType {
			continue
		}
		if strings.TrimSpace(assignment.ServiceID) != serviceID {
			continue
		}
		if assignment.AssignmentRole != "primary" {
			continue
		}
		streamID := strings.TrimSpace(assignment.StreamID)
		if streamID != "" {
			policy.PrimaryStreams[streamID] = true
		}
	}
	return policy
}

func firstRuntimeProfileForService(profiles []control.RuntimeProfile, serviceID string) (control.RuntimeProfile, bool) {
	for _, profile := range profiles {
		if profileBelongsToService(profile, serviceID) {
			return profile, true
		}
	}
	return control.RuntimeProfile{}, false
}

func profileBelongsToService(profile control.RuntimeProfile, serviceID string) bool {
	rawServiceID, ok := profile.Config["service_id"]
	if !ok {
		return true
	}
	profileServiceID, ok := rawServiceID.(string)
	if !ok {
		return false
	}
	profileServiceID = strings.TrimSpace(profileServiceID)
	return profileServiceID == "" || profileServiceID == strings.TrimSpace(serviceID)
}

func (m *Manager) ApplyProfileDefaults(stream StreamContext) StreamContext {
	m.mu.Lock()
	defaults := m.defaults
	m.mu.Unlock()
	if strings.TrimSpace(stream.OverlayProfileID) == "" {
		stream.OverlayProfileID = defaults.OverlayProfileID
	}
	if strings.TrimSpace(stream.CaptionProfileID) == "" {
		stream.CaptionProfileID = defaults.CaptionProfileID
	}
	return stream
}

func (m *Manager) applyStartProfileDefaults(stream StreamContext) StreamContext {
	m.mu.Lock()
	defaults := m.defaults
	m.mu.Unlock()
	if strings.TrimSpace(stream.OverlayProfileID) == "" {
		stream.OverlayProfileID = defaults.OverlayProfileID
	}
	return stream
}

func (m *Manager) streamAssignedLocked(streamID string) bool {
	if !m.assignments.Enforce {
		return true
	}
	return m.assignments.PrimaryStreams[strings.TrimSpace(streamID)]
}
