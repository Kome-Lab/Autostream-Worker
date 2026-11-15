package jobs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"strconv"
	"time"

	lifecycle "github.com/Kome-Lab/Autostream-Contracts/v3lifecycle"
	"github.com/example/autostream-worker/internal/encoder"
)

const workerLifecycleCommit lifecycle.ID = "d32ea3514b1c1010391caae0348b0ff9e751f447"

// LifecycleWitness stores only local registration/cut/release metadata. Its
// implementation must verify persistence in its own namespace. No network
// provider, credential, raw event, or media payload is passed to this port.
// There is deliberately no default implementation or environment switch.
type LifecycleWitness interface {
	Append(context.Context, lifecycle.AdmissionEvent) (lifecycle.Fact[lifecycle.ID], error)
	Release(context.Context, lifecycle.WorkRef) (lifecycle.Fact[lifecycle.ID], error)
}

// LifecycleEventPublisher is deliberately separate from the legacy client,
// whose configurable retry loop is unsuitable after an uncertain dispatch.
// One call is one attempt; a missing/bad ACK remains an inspect obligation.
type LifecycleEventPublisher interface {
	PublishOnce(context.Context, encoder.Event) error
}

type LifecycleParticipantConfig struct {
	Profile           lifecycle.ComparisonProfile
	Membership        lifecycle.Record
	CurrentVector     lifecycle.Record
	Admission         lifecycle.Record
	CompletionHeader  lifecycle.Header
	Namespace         lifecycle.Namespace
	StreamID          string
	ClosureID         lifecycle.ID
	EmptyFactRef      lifecycle.ID
	RegistrationEpoch lifecycle.ID
	Witness           LifecycleWitness
	Publisher         LifecycleEventPublisher
	ReleaseTimeout    time.Duration
	EffectTarget      lifecycle.ResourceRef
}

type workerLifecycleParticipant struct {
	config           LifecycleParticipantConfig
	model            lifecycle.AdmissionModel
	terminal         map[uint64]bool
	active           map[uint64]lifecycle.WorkRef
	effects          []lifecycle.ID
	registration     *lifecycle.RegistrationClosure
	uncertain        bool
	effectUnknown    bool
	jobGeneration    uint64
	snapshotRevision uint64
}

type lifecycleWrite struct {
	participant *workerLifecycleParticipant
	work        lifecycle.WorkRef
}

// LifecycleProcessIncarnation is owner-local freshness metadata, not a permit.
// A new manager cannot silently reuse an earlier participant's process frame.
func (m *Manager) LifecycleProcessIncarnation() (lifecycle.ID, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.lifecycleIncarnation == "" {
		var value [16]byte
		if _, err := rand.Read(value[:]); err != nil {
			return "", err
		}
		m.lifecycleIncarnation = "worker-process-" + hex.EncodeToString(value[:])
	}
	return lifecycle.ID(m.lifecycleIncarnation), nil
}

func workerResult(kind lifecycle.ResultKind, code string) lifecycle.Result {
	return lifecycle.Result{Kind: kind, Issues: []lifecycle.Issue{{Code: code, Path: "WorkerLifecycle"}}}
}

func workerUnconnected(code string) error {
	return &WorkerLifecycleError{workerResult(lifecycle.NeedsEvidence, code)}
}

type WorkerLifecycleError struct{ Result lifecycle.Result }

func (e *WorkerLifecycleError) Error() string { return "worker lifecycle: " + string(e.Result.Kind) }

// InstallLifecycleParticipant connects the existing event/caption writer to
// the frozen Contracts reducer. This slice has an explicit single namespace:
// active STT sessions, scene/video writers, and pre-existing retries are
// refused instead of being omitted from a completion proof.
func (m *Manager) InstallLifecycleParticipant(config LifecycleParticipantConfig) error {
	if config.Witness == nil || config.Publisher == nil || config.ReleaseTimeout <= 0 || config.ReleaseTimeout > 30*time.Second || config.EffectTarget.Kind != lifecycle.Sink || config.StreamID == "" || config.ClosureID == "" || config.EmptyFactRef == "" || config.RegistrationEpoch == "" || config.Profile.Ref.ProfileID != lifecycle.ClosureAssess {
		return &WorkerLifecycleError{workerResult(lifecycle.NeedsEvidence, "EXPLICIT_PARTICIPANT_CONFIGURATION_REQUIRED")}
	}
	for _, id := range []lifecycle.ID{config.ClosureID, config.EmptyFactRef, config.RegistrationEpoch, config.CompletionHeader.RecordKey.RecordID, config.EffectTarget.OwnerID, config.EffectTarget.ResourceID} {
		if result := lifecycle.ValidateID(id, config.Profile.Limits); result.Kind != lifecycle.StructurallyCoherent {
			return &WorkerLifecycleError{result}
		}
	}
	for _, record := range []lifecycle.Record{config.Membership, config.CurrentVector, config.Admission} {
		if record.Header.ContractRef.ModuleCommitRef != workerLifecycleCommit {
			return &WorkerLifecycleError{workerResult(lifecycle.RefusedUnchanged, "PINNED_CONTRACT_REQUIRED")}
		}
		if result := lifecycle.ValidateRecord(record, config.Profile); result.Kind != lifecycle.StructurallyCoherent {
			return &WorkerLifecycleError{result}
		}
	}
	if config.Membership.Membership == nil || config.CurrentVector.Vector == nil || config.Admission.Admission == nil || config.CompletionHeader.ContractRef.ContractID != lifecycle.CC04 || config.CompletionHeader.ContractRef.ModuleCommitRef != workerLifecycleCommit || config.CompletionHeader.ProducerRef.ComponentID != "C06" || config.Namespace.OwnerID != config.CompletionHeader.ProducerRef.ServiceID || config.Namespace.ProducerIncarnation != config.CompletionHeader.ProducerRef.ProcessIncarnation || config.Namespace.WorkKind != "MEDIA" {
		return &WorkerLifecycleError{workerResult(lifecycle.RefusedUnchanged, "EXACT_PARTICIPANT_RECORDS_REQUIRED")}
	}
	for _, header := range []lifecycle.Header{config.Membership.Header, config.CurrentVector.Header, config.Admission.Header} {
		if !reflect.DeepEqual(header.DomainRef, config.CompletionHeader.DomainRef) || header.Correlation != config.CompletionHeader.Correlation {
			return &WorkerLifecycleError{workerResult(lifecycle.Conflict, "MEMBERSHIP_ATTEMPT_MISMATCH")}
		}
	}
	if config.CurrentVector.Header.ProducerRef != config.CompletionHeader.ProducerRef || config.Admission.Admission.MembershipSnapshot != config.Membership.Header.DomainRef.SnapshotRef || len(config.Admission.Admission.Cut) != 1 || config.Admission.Admission.Cut[0].Namespace != config.Namespace || config.Admission.Admission.Cut[0].Kind != "EMPTY" {
		return &WorkerLifecycleError{workerResult(lifecycle.Conflict, "EXACT_WRITER_NAMESPACE_REQUIRED")}
	}
	if config.Membership.Header.RecordKey.RecordID != config.Membership.Header.DomainRef.SnapshotRef || config.ClosureID != config.Admission.Admission.ReservationRefs.DomainClosureRef || config.CompletionHeader.RecordKey.OwnerID != config.Namespace.OwnerID || config.CompletionHeader.RecordKey.ContractRef != config.CompletionHeader.ContractRef || config.CompletionHeader.ProfileRef != config.Profile.Ref || config.CompletionHeader.ContractRef.LogicalRevision != 1 || config.CompletionHeader.ContractRef.SpecRef != lifecycle.SpecRef || config.CompletionHeader.RecordRevision != 1 || len(config.CompletionHeader.RecordKey.RecordID)+22 > config.Profile.Limits.MaxIDBytes {
		return &WorkerLifecycleError{workerResult(lifecycle.Conflict, "OWNER_CLOSURE_HEADER_MISMATCH")}
	}
	covered := false
	for _, member := range config.Membership.Membership.Members {
		if member.Producer == config.CompletionHeader.ProducerRef && member.Resource == config.Namespace.Resource {
			covered = true
		}
	}
	if !covered || len(config.Membership.Membership.Members) != 1 {
		return &WorkerLifecycleError{workerResult(lifecycle.NeedsEvidence, "EXACT_SINGLE_WRITER_COVERAGE_REQUIRED")}
	}
	// The fixed vector must explicitly bind the actual product job generation.
	var jobGeneration *uint64
	for _, axis := range config.CurrentVector.Vector.Vector {
		if axis.Key.Axis == lifecycle.JobAxis && axis.Key.OwnerID == config.Namespace.OwnerID && axis.Key.Resource == config.Namespace.Resource {
			jobGeneration = axis.Value.Uint64
		}
		if axis.Key.OwnerID == config.Namespace.OwnerID && axis.Key.Resource == config.Namespace.Resource {
			switch axis.Key.Axis {
			case lifecycle.Process:
				if axis.Value.Literal == nil || *axis.Value.Literal != config.CompletionHeader.ProducerRef.ProcessIncarnation {
					return &WorkerLifecycleError{workerResult(lifecycle.Stale, "PROCESS_VECTOR_MISMATCH")}
				}
			case lifecycle.Boot:
				if axis.Value.Literal == nil || *axis.Value.Literal != config.CompletionHeader.ProducerRef.BootID {
					return &WorkerLifecycleError{workerResult(lifecycle.Stale, "BOOT_VECTOR_MISMATCH")}
				}
			case lifecycle.Admission:
				if axis.Value.Literal == nil || *axis.Value.Literal != config.Admission.Admission.AdmissionEpoch {
					return &WorkerLifecycleError{workerResult(lifecycle.Stale, "ADMISSION_VECTOR_MISMATCH")}
				}
			case lifecycle.Membership:
				if axis.Value.Uint64 == nil || *axis.Value.Uint64 != config.Membership.Header.DomainRef.MembershipRevision {
					return &WorkerLifecycleError{workerResult(lifecycle.Stale, "MEMBERSHIP_VECTOR_MISMATCH")}
				}
			}
		}
	}
	if jobGeneration == nil {
		return &WorkerLifecycleError{workerResult(lifecycle.NeedsEvidence, "JOB_GENERATION_AXIS_REQUIRED")}
	}
	m.lifecycleMu.Lock()
	defer m.lifecycleMu.Unlock()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.lifecycleIncarnation == "" || config.CompletionHeader.ProducerRef.ProcessIncarnation != lifecycle.ID(m.lifecycleIncarnation) {
		return &WorkerLifecycleError{workerResult(lifecycle.Stale, "ACTUAL_PROCESS_INCARNATION_MISMATCH")}
	}
	if m.lifecycleParticipant != nil {
		return &WorkerLifecycleError{workerResult(lifecycle.Conflict, "PARTICIPANT_ALREADY_INSTALLED")}
	}
	if m.current.StreamID != config.StreamID || m.jobGeneration != *jobGeneration || m.stopping {
		return &WorkerLifecycleError{workerResult(lifecycle.Stale, "ACTUAL_JOB_GENERATION_MISMATCH")}
	}
	if m.captionSession != nil || m.captionIngress != nil || m.sceneRenderer != nil || videoOutputRequested(m.current) || len(m.pendingEvents) != 0 || m.eventWritesInFlight != 0 {
		return &WorkerLifecycleError{workerResult(lifecycle.NeedsEvidence, "UNCONNECTED_WRITER_OR_BORROW_PRESENT")}
	}
	// Freeze shared record/profile slices without serializing the private port.
	frozen, err := json.Marshal(struct {
		Profile                       lifecycle.ComparisonProfile
		Membership, Vector, Admission lifecycle.Record
		Header                        lifecycle.Header
	}{config.Profile, config.Membership, config.CurrentVector, config.Admission, config.CompletionHeader})
	if err != nil {
		return err
	}
	var copy struct {
		Profile                       lifecycle.ComparisonProfile
		Membership, Vector, Admission lifecycle.Record
		Header                        lifecycle.Header
	}
	if err = json.Unmarshal(frozen, &copy); err != nil {
		return err
	}
	config.Profile = copy.Profile
	config.Membership = copy.Membership
	config.CurrentVector = copy.Vector
	config.Admission = copy.Admission
	config.CompletionHeader = copy.Header
	model := lifecycle.AdmissionModel{Key: config.Admission.Header.RecordKey, MembershipSnapshot: config.Membership.Header.DomainRef.SnapshotRef, AdmissionEpoch: config.Admission.Admission.AdmissionEpoch, Lane: lifecycle.Open, Drain: lifecycle.Waiting, Gate: lifecycle.Held, Revision: 1, RequiredGuardRefs: append([]lifecycle.ID(nil), config.Admission.Admission.ActorGuardRefs...), AllowedNamespaces: []lifecycle.Namespace{config.Namespace}}
	m.lifecycleParticipant = &workerLifecycleParticipant{config: config, model: model, terminal: map[uint64]bool{}, active: map[uint64]lifecycle.WorkRef{}, jobGeneration: *jobGeneration, snapshotRevision: 1}
	return nil
}

func (m *Manager) beginLifecycleWriteLocked(ctx context.Context, eventID string) (*lifecycleWrite, error) {
	p := m.lifecycleParticipant
	if p == nil {
		return nil, nil
	}
	if ctx.Err() != nil {
		return nil, &WorkerLifecycleError{workerResult(lifecycle.RefusedUnchanged, "REQUEST_CANCELED_BEFORE_REGISTRATION")}
	}
	if m.captionSession != nil || m.captionIngress != nil || m.sceneRenderer != nil || videoOutputRequested(m.current) || len(m.pendingEvents) != 0 {
		return nil, &WorkerLifecycleError{workerResult(lifecycle.NeedsEvidence, "UNCONNECTED_WRITER_PRESENT")}
	}
	if p.uncertain {
		return nil, &WorkerLifecycleError{workerResult(lifecycle.Unknown, "WITNESS_RESULT_UNKNOWN_HELD")}
	}
	if m.current.StreamID != p.config.StreamID || m.jobGeneration != p.jobGeneration {
		return nil, &WorkerLifecycleError{workerResult(lifecycle.Stale, "ACTUAL_JOB_GENERATION_MISMATCH")}
	}
	if p.model.Lane != lifecycle.Open {
		return nil, &WorkerLifecycleError{workerResult(lifecycle.RefusedUnchanged, "INDEPENDENT_ADMISSION_CLOSED")}
	}
	if len(p.model.Works) >= p.config.Profile.Limits.MaxRefs {
		return nil, &WorkerLifecycleError{workerResult(lifecycle.Conflict, "REGISTERED_WORK_LIMIT_REACHED")}
	}
	if len(p.active) >= p.config.Profile.Limits.MaxObligations || len(p.active) >= p.config.Profile.Limits.MaxRanges {
		return nil, &WorkerLifecycleError{workerResult(lifecycle.Conflict, "ACTIVE_BORROW_LIMIT_REACHED")}
	}
	sequence := uint64(len(p.model.Works)) + 1
	work := lifecycle.WorkRef{WorkID: lifecycle.ID(eventID), Namespace: p.config.Namespace, Sequence: sequence, AdmissionRecordRef: p.model.Key.RecordID}
	if result := lifecycle.ValidateID(work.WorkID, p.config.Profile.Limits); result.Kind != lifecycle.StructurallyCoherent {
		return nil, &WorkerLifecycleError{result}
	}
	for _, registered := range p.model.Works {
		if registered.Work.WorkID == work.WorkID {
			return nil, &WorkerLifecycleError{workerResult(lifecycle.Conflict, "WORK_ID_REUSED_NO_REDISPATCH")}
		}
	}
	event := lifecycle.AdmissionEvent{Kind: lifecycle.RegisterIndependentWork, ExpectedRevision: p.model.Revision, Work: work}
	if err := p.apply(ctx, event); err != nil {
		return nil, err
	}
	event = lifecycle.AdmissionEvent{Kind: lifecycle.RecordWorkDispatch, ExpectedRevision: p.model.Revision, WorkID: work.WorkID}
	if err := p.apply(ctx, event); err != nil {
		return nil, err
	}
	p.active[sequence] = work
	return &lifecycleWrite{participant: p, work: work}, nil
}

func (p *workerLifecycleParticipant) apply(ctx context.Context, event lifecycle.AdmissionEvent) error {
	fact, err := p.config.Witness.Append(ctx, event)
	if err != nil {
		p.uncertain = true
		return &WorkerLifecycleError{workerResult(lifecycle.Unknown, "WITNESS_APPEND_UNKNOWN")}
	}
	if !p.validWitness(fact) {
		p.uncertain = true
		return &WorkerLifecycleError{workerResult(lifecycle.Unknown, "WITNESS_OWNER_FRAME_UNVERIFIED")}
	}
	event.DurableFact = fact
	next, result := lifecycle.ReduceAdmission(p.model, event, p.config.Profile.Limits)
	if result.Kind != lifecycle.StructurallyCoherent {
		p.uncertain = true
		return &WorkerLifecycleError{workerResult(lifecycle.Unknown, "REDUCER_PROPOSAL_UNVERIFIED_AFTER_WITNESS")}
	}
	p.model = next
	p.snapshotRevision++
	return nil
}

func (p *workerLifecycleParticipant) validWitness(fact lifecycle.Fact[lifecycle.ID]) bool {
	if lifecycle.ValidateFact(fact, p.config.Profile.Limits).Kind != lifecycle.StructurallyCoherent || fact.Observation == nil || fact.Observation.OwnerID != p.config.Namespace.OwnerID || fact.Observation.Scope != p.config.CompletionHeader.DomainRef.DomainID {
		return false
	}
	for _, frame := range p.config.Membership.Membership.SourceFrames {
		if frame.FrameRef == fact.Observation.FrameRef && frame.OwnerID == fact.Observation.OwnerID && frame.Scope == fact.Observation.Scope && frame.SourceRevision == fact.Observation.SourceRevision {
			return true
		}
	}
	return false
}

func (m *Manager) finishLifecycleWrite(write *lifecycleWrite, dispatched, acknowledged bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.eventWritesInFlight--
	if write == nil {
		return
	}
	p := write.participant
	delete(p.active, write.work.Sequence)
	// Local handle release and the provider's terminal result are independent.
	// A returned Publish call cannot prove the external terminal boundary.
	if dispatched {
		p.effects = append(p.effects, write.work.WorkID)
	}
	if dispatched && !acknowledged {
		p.effectUnknown = true
	}
	ctx, cancel := context.WithTimeout(context.Background(), p.config.ReleaseTimeout)
	defer cancel()
	fact, err := p.config.Witness.Release(ctx, write.work)
	if err != nil || !p.validWitness(fact) {
		p.uncertain = true
		return
	}
	p.terminal[write.work.Sequence] = true
	p.snapshotRevision++
}

// CloseLifecycleAdmission closes only this registered writer namespace. It
// never stops media, releases HELD, or grants recovery/execution authority.
func (m *Manager) CloseLifecycleAdmission(ctx context.Context, expected lifecycle.Record) lifecycle.Result {
	m.mu.Lock()
	defer m.mu.Unlock()
	p := m.lifecycleParticipant
	if p == nil {
		return workerResult(lifecycle.NeedsEvidence, "PARTICIPANT_REQUIRED")
	}
	if ctx.Err() != nil {
		return workerResult(lifecycle.RefusedUnchanged, "CUT_CANCELED_BEFORE_WITNESS")
	}
	if result := p.compare(expected, m.current.StreamID, m.jobGeneration); result.Kind != lifecycle.StructurallyCoherent {
		return result
	}
	if p.uncertain {
		return workerResult(lifecycle.Unknown, "WITNESS_RESULT_UNKNOWN_HELD")
	}
	if p.model.Lane != lifecycle.Open {
		return workerResult(lifecycle.Conflict, "CUT_ALREADY_STARTED")
	}
	cut := p.frontier(uint64(len(p.model.Works)))
	if err := p.apply(ctx, lifecycle.AdmissionEvent{Kind: lifecycle.BeginAdmissionCut, ExpectedRevision: p.model.Revision, Cut: []lifecycle.Frontier{cut}}); err != nil {
		return lifecycleErrorResult(err)
	}
	for _, guard := range p.model.RequiredGuardRefs {
		if err := p.apply(ctx, lifecycle.AdmissionEvent{Kind: lifecycle.InstallAdmissionGuard, ExpectedRevision: p.model.Revision, GuardRef: guard}); err != nil {
			return lifecycleErrorResult(err)
		}
	}
	if err := p.apply(ctx, lifecycle.AdmissionEvent{Kind: lifecycle.FinishAdmissionCut, ExpectedRevision: p.model.Revision}); err != nil {
		return lifecycleErrorResult(err)
	}
	closure := lifecycle.RegistrationClosure{MembershipSnapshot: p.model.MembershipSnapshot, AdmissionEpoch: p.model.AdmissionEpoch, Namespace: p.config.Namespace, RegistrationEpoch: p.config.RegistrationEpoch, OwnerID: p.config.Namespace.OwnerID}
	event := lifecycle.AdmissionEvent{Kind: lifecycle.CloseLineageRegistration, ExpectedRevision: p.model.Revision, RegistrationClosure: closure}
	fact, err := p.config.Witness.Append(ctx, event)
	if err != nil || !p.validWitness(fact) {
		p.uncertain = true
		return workerResult(lifecycle.Unknown, "REGISTRATION_CLOSURE_UNKNOWN")
	}
	closure.DurableFactRef = *fact.Value
	event.RegistrationClosure = closure
	event.DurableFact = fact
	next, result := lifecycle.ReduceAdmission(p.model, event, p.config.Profile.Limits)
	if result.Kind != lifecycle.StructurallyCoherent {
		p.uncertain = true
		return workerResult(lifecycle.Unknown, "CLOSURE_PROPOSAL_UNVERIFIED_AFTER_WITNESS")
	}
	p.model = next
	p.registration = &closure
	p.snapshotRevision++
	return lifecycle.Result{Kind: lifecycle.StructurallyCoherent}
}

func lifecycleErrorResult(err error) lifecycle.Result {
	var e *WorkerLifecycleError
	if errors.As(err, &e) {
		return e.Result
	}
	return workerResult(lifecycle.Unknown, "LOCAL_WITNESS_UNKNOWN")
}

func (p *workerLifecycleParticipant) compare(expected lifecycle.Record, streamID string, generation uint64) lifecycle.Result {
	if result := lifecycle.ValidateRecord(expected, p.config.Profile); result.Kind != lifecycle.StructurallyCoherent {
		return result
	}
	if expected.Vector == nil || expected.Header.ContractRef.ContractID != lifecycle.CC02 || expected.Header.ContractRef.ModuleCommitRef != workerLifecycleCommit || expected.Header.Correlation != p.config.CurrentVector.Header.Correlation || !reflect.DeepEqual(expected.Header.DomainRef, p.config.CurrentVector.Header.DomainRef) || expected.Header.ProducerRef != p.config.CurrentVector.Header.ProducerRef || expected.Header.RecordKey != p.config.CurrentVector.Header.RecordKey {
		return workerResult(lifecycle.Conflict, "ORIGINAL_FRAME_MISMATCH")
	}
	if !reflect.DeepEqual(expected.Header, p.config.CurrentVector.Header) {
		return workerResult(lifecycle.Conflict, "IMMUTABLE_HEADER_MISMATCH")
	}
	if streamID != p.config.StreamID || generation != p.jobGeneration {
		return workerResult(lifecycle.Stale, "ACTUAL_JOB_GENERATION_MISMATCH")
	}
	return lifecycle.CompareVectors(expected.Vector.Vector, p.config.CurrentVector.Vector.Vector, p.config.Profile)
}

func (p *workerLifecycleParticipant) frontier(last uint64) lifecycle.Frontier {
	if last == 0 {
		return lifecycle.Frontier{Kind: "EMPTY", Namespace: p.config.Namespace, EmptyFactRef: p.config.EmptyFactRef}
	}
	return lifecycle.Frontier{Kind: "THROUGH", Namespace: p.config.Namespace, LastSequence: &last}
}

// LifecycleCompletionSnapshot reads the actual writer registry in the same
// manager frame. Queue emptiness cannot erase an in-flight publisher borrow.
// Nonempty external effects remain pending under the frozen shared validator.
func (m *Manager) LifecycleCompletionSnapshot(expected lifecycle.Record) (lifecycle.Record, lifecycle.Result) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p := m.lifecycleParticipant
	if p == nil {
		return lifecycle.Record{}, workerResult(lifecycle.NeedsEvidence, "PARTICIPANT_REQUIRED")
	}
	if m.captionSession != nil || m.captionIngress != nil || m.sceneRenderer != nil || videoOutputRequested(m.current) || len(m.pendingEvents) != 0 {
		return lifecycle.Record{}, workerResult(lifecycle.NeedsEvidence, "UNCONNECTED_WRITER_PRESENT")
	}
	if result := p.compare(expected, m.current.StreamID, m.jobGeneration); result.Kind != lifecycle.StructurallyCoherent {
		return lifecycle.Record{}, result
	}
	if p.uncertain {
		return lifecycle.Record{}, workerResult(lifecycle.Unknown, "WITNESS_RESULT_UNKNOWN_HELD")
	}
	if p.registration == nil {
		return lifecycle.Record{}, workerResult(lifecycle.PendingObligation, "REGISTRATION_NOT_CLOSED")
	}
	terminal := uint64(0)
	for sequence := uint64(1); sequence <= uint64(len(p.model.Works)); sequence++ {
		if !p.terminal[sequence] {
			break
		}
		terminal = sequence
	}
	part := lifecycle.PartitionProof{Namespace: p.config.Namespace, CutFrontier: p.model.Cut[0], FinalAcceptedFrontier: p.frontier(uint64(len(p.model.Works))), TerminalFrontier: p.frontier(terminal), RegistrationClosed: *p.registration}
	for sequence := terminal + 1; sequence <= uint64(len(p.model.Works)); sequence++ {
		if !p.terminal[sequence] {
			part.Holes = append(part.Holes, lifecycle.SequenceRange{First: sequence, Last: sequence})
		}
	}
	record := lifecycle.Record{Header: p.config.CompletionHeader, Completion: &lifecycle.CompletionRecord{ClosureID: p.config.ClosureID, ReservationRefs: p.config.Admission.Admission.ReservationRefs, SourceVector: p.config.CurrentVector.Vector.Vector, Partitions: []lifecycle.PartitionProof{part}, Effects: append([]lifecycle.ID(nil), p.effects...), LineageClosureFacts: []lifecycle.RegistrationClosure{*p.registration}, TerminalPolicyRef: p.config.Profile.TerminalPolicyRef, ProofRefs: []lifecycle.ID{p.registration.DurableFactRef}}}
	record.Header.RecordRevision = p.snapshotRevision
	record.Header.RecordKey.RecordID = lifecycle.ID(string(p.config.CompletionHeader.RecordKey.RecordID) + "-r" + strconv.FormatUint(p.snapshotRevision, 10))
	for _, effect := range p.effects {
		record.Completion.RequiredEffects = append(record.Completion.RequiredEffects, lifecycle.EffectRequirement{EffectID: effect, Target: p.config.EffectTarget, Correlation: p.config.CompletionHeader.Correlation})
	}
	for sequence, work := range p.active {
		record.Completion.PhysicalObligations = append(record.Completion.PhysicalObligations, lifecycle.PhysicalObligation{ObligationID: lifecycle.ID("writer-borrow-" + strconv.FormatUint(sequence, 10)), Kind: lifecycle.WriterBorrow, OwnerID: p.config.Namespace.OwnerID, Resource: p.config.Namespace.Resource, WorkRef: work.WorkID, Knowledge: lifecycle.KnowledgeUnknown, Terminal: lifecycle.Fact[lifecycle.ID]{State: lifecycle.Unavailable, Unavailable: &lifecycle.UnavailableReason{Code: "ACTIVE_WRITER_BORROW"}}})
	}
	result := lifecycle.ValidateRecord(record, p.config.Profile)
	if p.effectUnknown && result.Kind == lifecycle.PendingObligation {
		result = workerResult(lifecycle.Unknown, "PROVIDER_RESULT_UNKNOWN_NOT_CP_COMMIT_UNKNOWN")
	}
	frozen, err := json.Marshal(record)
	if err != nil {
		return lifecycle.Record{}, workerResult(lifecycle.Unknown, "SNAPSHOT_COPY_FAILED")
	}
	var detached lifecycle.Record
	if json.Unmarshal(frozen, &detached) != nil {
		return lifecycle.Record{}, workerResult(lifecycle.Unknown, "SNAPSHOT_COPY_FAILED")
	}
	return detached, result
}
