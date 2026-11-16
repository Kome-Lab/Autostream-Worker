// Package manualhold adapts the pinned G0 observation types to product Start
// admission. Its records are not Permits, authority, or production qualification.
package manualhold

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	lifecycle "github.com/Kome-Lab/Autostream-Contracts/v3lifecycle"
)

const Header = "X-AutoStream-Start-Admission"
const ModuleCommit = "d32ea3514b1c1010391caae0348b0ff9e751f447"
const MaxRecordBytes = 32768

var ErrRefused = errors.New("start admission refused unchanged")
var ErrUnknown = errors.New("start admission outcome unknown")

// Limits is the explicit, versioned product observation budget. Missing input
// profile/fields are rejected; these limits do not fill absent observations.
func Limits() lifecycle.Limits {
	return lifecycle.Limits{MaxIDBytes: 96, MaxMembers: 16, MaxAxes: 16, MaxRefs: 128, MaxObligations: 64, MaxPartitions: 32, MaxRanges: 64, MaxDepth: 32}
}

var requiredAxes = []lifecycle.Axis{lifecycle.Membership, lifecycle.Admission, lifecycle.Ownership, lifecycle.Fence, lifecycle.Boot, lifecycle.Process, lifecycle.RunAxis, lifecycle.ConfigAxis}

// Resource is the target-owned Stream named by the CP source vector, even on a target-owned
// acknowledgement. Header ownership and source-vector ownership are distinct.
func Resource(r lifecycle.Record) (lifecycle.ResourceRef, error) {
	if r.Vector == nil || len(r.Vector.Vector) != len(requiredAxes) {
		return lifecycle.ResourceRef{}, ErrRefused
	}
	resource := r.Vector.Vector[0].Key.Resource
	if resource.Kind != lifecycle.Stream || resource.OwnerID == "" || resource.ResourceID == "" {
		return lifecycle.ResourceRef{}, ErrRefused
	}
	return resource, nil
}

func Profile(r lifecycle.Record) lifecycle.ComparisonProfile {
	resource, _ := Resource(r)
	profile := lifecycle.ComparisonProfile{
		Ref:                lifecycle.ProfileRef{ProfileID: lifecycle.ExplicitResume, Revision: 1},
		ResourceKinds:      []lifecycle.ResourceKind{lifecycle.Stream},
		Purposes:           []lifecycle.Purpose{lifecycle.PurposeResume},
		RequiredBoundaries: []lifecycle.BoundaryAddress{{Kind: lifecycle.BackendMutation, OwnerID: resource.OwnerID, Resource: resource, SubBoundary: lifecycle.ServiceApply}},
		TerminalPolicyRef:  "IU-WS2-10-TARGET-START-v1", LimitsRef: "IU-WS2-10-LIMITS-v1", Limits: Limits(),
	}
	for _, axis := range requiredAxes {
		kind := lifecycle.PresentLiteral
		if axis == lifecycle.Membership {
			kind = lifecycle.PresentUint64
		}
		profile.AxisRules = append(profile.AxisRules, lifecycle.AxisRule{Key: lifecycle.AxisKey{Axis: axis, OwnerID: resource.OwnerID, Resource: resource}, OwnerRole: "C01", ValueKind: kind, Required: true, Comparison: "EXACT"})
	}
	return profile
}

func Validate(r lifecycle.Record, streamID string) error {
	resource, err := Resource(r)
	if err != nil || resource.ResourceID != lifecycle.ID(streamID) || r.Header.ContractRef.ContractID != lifecycle.CC02 || r.Header.ContractRef.ModuleCommitRef != ModuleCommit || r.Header.ProfileRef != (lifecycle.ProfileRef{ProfileID: lifecycle.ExplicitResume, Revision: 1}) {
		return ErrRefused
	}
	for _, entry := range r.Vector.Vector {
		if entry.Key.Resource != resource || entry.Key.OwnerID != resource.OwnerID {
			return ErrRefused
		}
	}
	if lifecycle.ValidateRecord(r, Profile(r)).Kind != lifecycle.StructurallyCoherent {
		return ErrRefused
	}
	return nil
}

func Encode(r lifecycle.Record) (string, error) {
	resource, err := Resource(r)
	if err != nil || Validate(r, string(resource.ResourceID)) != nil {
		return "", ErrRefused
	}
	body, err := json.Marshal(r)
	if err != nil || len(body) > MaxRecordBytes {
		return "", ErrRefused
	}
	return base64.RawURLEncoding.EncodeToString(body), nil
}

// Decode uses the pinned bounded/duplicate-key-rejecting decoder. The initial
// parse only identifies the profile resource; no write or authority follows it.
func Decode(encoded string) (lifecycle.Record, error) {
	if len(encoded) > base64.RawURLEncoding.EncodedLen(MaxRecordBytes) {
		return lifecycle.Record{}, ErrRefused
	}
	body, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return lifecycle.Record{}, ErrRefused
	}
	return DecodeJSON(body)
}

func DecodeJSON(body []byte) (lifecycle.Record, error) {
	if len(body) > MaxRecordBytes {
		return lifecycle.Record{}, ErrRefused
	}
	var candidate lifecycle.Record
	if json.Unmarshal(body, &candidate) != nil {
		return lifecycle.Record{}, ErrRefused
	}
	resource, err := Resource(candidate)
	if err != nil {
		return lifecycle.Record{}, ErrRefused
	}
	r, result := lifecycle.DecodeRecord(body, Profile(candidate))
	if result.Kind != lifecycle.StructurallyCoherent || Validate(r, string(resource.ResourceID)) != nil {
		return lifecycle.Record{}, ErrRefused
	}
	return r, nil
}

func Equal(a, b lifecycle.Record) bool {
	return a.Header.RecordRevision == b.Header.RecordRevision && lifecycle.CompareImmutable(a, b, Profile(a)).Kind == lifecycle.StructurallyCoherent
}

// SameSource compares a target acknowledgement with the original CP frame. Only
// the acknowledgement's actual producer and local record key may differ.
func SameSource(original, acknowledgement lifecycle.Record) bool {
	acknowledgement.Header.ProducerRef = original.Header.ProducerRef
	acknowledgement.Header.RecordKey = original.Header.RecordKey
	return Equal(original, acknowledgement)
}

type contextKey struct{}

func WithRecord(ctx context.Context, r lifecycle.Record) context.Context {
	// Detach caller-owned slices/pointers before handing work to another layer.
	body, _ := json.Marshal(r)
	var frozen lifecycle.Record
	_ = json.Unmarshal(body, &frozen)
	return context.WithValue(ctx, contextKey{}, frozen)
}

func RecordFromContext(ctx context.Context) *lifecycle.Record {
	if ctx == nil {
		return nil
	}
	r, ok := ctx.Value(contextKey{}).(lifecycle.Record)
	if !ok {
		return nil
	}
	return &r
}

func RequestContext(r *http.Request) (context.Context, error) {
	values := r.Header.Values(Header)
	if len(values) == 0 {
		return r.Context(), nil
	}
	if len(values) != 1 || strings.TrimSpace(values[0]) == "" {
		return nil, ErrRefused
	}
	record, err := Decode(values[0])
	if err != nil {
		return nil, err
	}
	return WithRecord(r.Context(), record), nil
}

type Inspector interface {
	Check(context.Context, lifecycle.Record) (lifecycle.Record, error)
}

type InspectorFunc func(context.Context, lifecycle.Record) (lifecycle.Record, error)

func (f InspectorFunc) Check(ctx context.Context, r lifecycle.Record) (lifecycle.Record, error) {
	return f(ctx, r)
}

// HTTPInspector obtains its endpoint and token only from the trusted runtime
// composition. It never follows a URL in the Start record and never retries.
type HTTPInspector struct {
	Config func() (baseURL, token string, err error)
	HTTP   *http.Client
}

func (c HTTPInspector) Check(ctx context.Context, original lifecycle.Record) (lifecycle.Record, error) {
	resource, err := Resource(original)
	if err != nil || c.Config == nil {
		return lifecycle.Record{}, ErrUnknown
	}
	base, token, err := c.Config()
	u, parseErr := url.Parse(base)
	if err != nil || parseErr != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.TrimSpace(token) == "" || c.HTTP == nil {
		return lifecycle.Record{}, ErrUnknown
	}
	body, err := json.Marshal(original)
	if err != nil {
		return lifecycle.Record{}, ErrRefused
	}
	endpoint := strings.TrimRight(base, "/") + "/services/streams/" + url.PathEscape(string(resource.ResourceID)) + "/start-admission/inspect"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return lifecycle.Record{}, ErrUnknown
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return lifecycle.Record{}, ErrUnknown
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusConflict || resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusNotFound {
		return lifecycle.Record{}, ErrRefused
	}
	if resp.StatusCode != http.StatusOK {
		return lifecycle.Record{}, ErrUnknown
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, MaxRecordBytes+1))
	if err != nil || len(data) > MaxRecordBytes {
		return lifecycle.Record{}, ErrUnknown
	}
	current, err := DecodeJSON(data)
	if err != nil {
		return lifecycle.Record{}, ErrUnknown
	}
	if !Equal(original, current) {
		return lifecycle.Record{}, fmt.Errorf("%w: current source changed", ErrRefused)
	}
	return current, nil
}

// ValidateStart binds the nonsecret existing Run identity in the actual product
// request to the CP-owned Run axis. A Hold revision is never a Run identifier.
func ValidateStart(ctx context.Context, streamID, archiveRunID string) error {
	record := RecordFromContext(ctx)
	if record == nil {
		return nil
	}
	if archiveRunID == "" || Validate(*record, streamID) != nil {
		return ErrRefused
	}
	for _, entry := range record.Vector.Vector {
		if entry.Key.Axis == lifecycle.RunAxis {
			if entry.Value.Kind != lifecycle.PresentLiteral || entry.Value.Literal == nil || *entry.Value.Literal != lifecycle.ID(archiveRunID) {
				return ErrRefused
			}
			return nil
		}
	}
	return ErrRefused
}
