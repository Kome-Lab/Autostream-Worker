package jobs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	lifecycle "github.com/Kome-Lab/Autostream-Contracts/v3lifecycle"
	"github.com/example/autostream-worker/internal/control"
	"github.com/example/autostream-worker/internal/deepgram"
	"github.com/example/autostream-worker/internal/encoder"
)

type workerWitnessFixture struct {
	mu             sync.Mutex
	count          int
	fail           lifecycle.AdmissionEventKind
	dispatches     int
	secondDispatch chan struct{}
}

func (w *workerWitnessFixture) Append(_ context.Context, event lifecycle.AdmissionEvent) (lifecycle.Fact[lifecycle.ID], error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.count++
	if event.Kind == w.fail {
		return lifecycle.Fact[lifecycle.ID]{}, errors.New("fixture ACK loss")
	}
	if event.Kind == lifecycle.RecordWorkDispatch {
		w.dispatches++
		if w.dispatches == 2 && w.secondDispatch != nil {
			close(w.secondDispatch)
		}
	}
	return workerWitnessFact(lifecycle.ID("fixture-journal-" + strconv.Itoa(w.count))), nil
}
func (w *workerWitnessFixture) Release(context.Context, lifecycle.WorkRef) (lifecycle.Fact[lifecycle.ID], error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.count++
	return workerWitnessFact(lifecycle.ID("fixture-release-" + strconv.Itoa(w.count))), nil
}
func workerWitnessFact(id lifecycle.ID) lifecycle.Fact[lifecycle.ID] {
	return lifecycle.Fact[lifecycle.ID]{State: lifecycle.Observed, Value: &id, Observation: &lifecycle.ObservationContext{OwnerID: "worker-01", FrameRef: "worker-frame", Scope: "fixture-domain", SourceRevision: 1}}
}

type workerPublisherFixture struct {
	calls   atomic.Int64
	entered chan struct{}
	release chan struct{}
	once    sync.Once
	err     error
}

func (p *workerPublisherFixture) PublishOnce(ctx context.Context, event encoder.Event) error {
	return p.Publish(ctx, event)
}

func (p *workerPublisherFixture) Publish(ctx context.Context, event encoder.Event) error {
	p.calls.Add(1)
	if p.entered != nil {
		p.once.Do(func() { close(p.entered) })
	}
	if p.release != nil {
		select {
		case <-p.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return p.err
}

// The owner facts and small limits here are explicit synthetic inputs. They
// never provide a production witness or enable a default process participant.
func workerLifecycleFixture(t *testing.T, publisher *workerPublisherFixture, options ...StreamContext) (*Manager, LifecycleParticipantConfig) {
	t.Helper()
	stream := StreamContext{StreamID: "stream-01"}
	if len(options) > 1 {
		t.Fatal("one explicit fixture stream at most")
	}
	if len(options) == 1 {
		if options[0].StreamID != "stream-01" || options[0].CaptionProfileID != "" || videoOutputRequested(options[0]) {
			t.Fatal("fixture scope is the single caption/event writer")
		}
		stream = options[0]
	}
	m := NewManager(publisher, nil)
	m.ApplyRuntimeConfig(captionRuntimeConfig())
	if err := m.Start(t.Context(), stream); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close(context.Background()) })
	process, err := m.LifecycleProcessIncarnation()
	if err != nil {
		t.Fatal(err)
	}
	producer := lifecycle.ProducerRef{ComponentID: "C06", ServiceID: "worker-01", HostID: "worker-host", BootID: "worker-boot", ProcessIncarnation: process, WriterID: "caption-event-writer"}
	resource := lifecycle.ResourceRef{Kind: lifecycle.Partition, OwnerID: producer.ServiceID, ResourceID: "caption-event-partition"}
	namespace := lifecycle.Namespace{OwnerID: producer.ServiceID, Resource: resource, WorkKind: "MEDIA", ProducerIncarnation: process, SequenceEpoch: "fixture-sequence-epoch"}
	p := lifecycle.ComparisonProfile{Ref: lifecycle.ProfileRef{ProfileID: lifecycle.ClosureAssess, Revision: 1}, ResourceKinds: []lifecycle.ResourceKind{lifecycle.Partition}, Purposes: []lifecycle.Purpose{lifecycle.PurposeClosure}, RequiredBoundaries: []lifecycle.BoundaryAddress{{Kind: lifecycle.LocalPersistence, OwnerID: producer.ServiceID, Resource: resource, SubBoundary: lifecycle.QueueAdmission}}, TerminalPolicyRef: "fixture-terminal-policy", LimitsRef: "fixture-limits", Limits: lifecycle.Limits{MaxIDBytes: 192, MaxMembers: 16, MaxAxes: 32, MaxRefs: 128, MaxObligations: 16, MaxPartitions: 8, MaxRanges: 32, MaxDepth: 16}}
	var vector lifecycle.RevisionVector
	for _, axis := range []lifecycle.Axis{lifecycle.Membership, lifecycle.Admission, lifecycle.Boot, lifecycle.Process, lifecycle.Writer, lifecycle.Policy, lifecycle.Trust, lifecycle.ReleaseAxis, lifecycle.ConfigAxis, lifecycle.JobAxis} {
		key := lifecycle.AxisKey{Axis: axis, OwnerID: producer.ServiceID, Resource: resource}
		value := lifecycle.VersionedValue{Kind: lifecycle.PresentLiteral}
		switch axis {
		case lifecycle.Membership, lifecycle.Writer, lifecycle.JobAxis:
			n := uint64(1)
			value = lifecycle.VersionedValue{Kind: lifecycle.PresentUint64, Uint64: &n}
		default:
			id := lifecycle.ID("fixture-" + string(axis))
			if axis == lifecycle.Process {
				id = process
			}
			if axis == lifecycle.Boot {
				id = producer.BootID
			}
			value.Literal = &id
		}
		p.AxisRules = append(p.AxisRules, lifecycle.AxisRule{Key: key, OwnerRole: "C06", ValueKind: value.Kind, Required: true, Comparison: "EXACT"})
		vector = append(vector, lifecycle.RevisionEntry{Key: key, Incarnation: process, Value: value, SourceFrameRef: "worker-frame"})
	}
	domain := lifecycle.DomainRef{DeploymentID: "fixture-deployment", Realm: "fixture-realm", DomainID: "fixture-domain", MembershipRevision: 1, SnapshotRef: "fixture-membership", Resources: []lifecycle.ResourceRef{resource}}
	correlation := lifecycle.Correlation{RequestID: "fixture-request", OperationID: "fixture-operation", AttemptID: "fixture-attempt", StepID: "fixture-step", ExecutionKey: "fixture-execution"}
	header := func(id lifecycle.ContractID, recordID lifecycle.ID) lifecycle.Header {
		ref := lifecycle.ContractRef{ContractID: id, LogicalRevision: 1, SpecRef: lifecycle.SpecRef, ModuleCommitRef: workerLifecycleCommit}
		return lifecycle.Header{ContractRef: ref, RecordKey: lifecycle.RecordKey{ContractRef: ref, OwnerID: producer.ServiceID, RecordID: recordID}, Correlation: correlation, DomainRef: domain, ProducerRef: producer, RecordRevision: 1, ProfileRef: p.Ref}
	}
	current := lifecycle.Record{Header: header(lifecycle.CC02, "fixture-current-vector"), Vector: &lifecycle.VectorRecord{Vector: vector}}
	cp := lifecycle.ProducerRef{ComponentID: "C01", ServiceID: "cp-fixture", HostID: "cp-host", BootID: "cp-boot", ProcessIncarnation: "cp-process", WriterID: "membership-writer"}
	membershipHeader := header(lifecycle.CC01, "fixture-membership")
	membershipHeader.ProducerRef = cp
	membershipHeader.RecordKey.OwnerID = cp.ServiceID
	coverage := lifecycle.ID("fixture-complete-single-writer-roster")
	cpContext := &lifecycle.ObservationContext{OwnerID: cp.ServiceID, FrameRef: "cp-frame", Scope: domain.DomainID, SourceRevision: 1}
	membership := lifecycle.Record{Header: membershipHeader, Membership: &lifecycle.MembershipRecord{DomainKind: "MEDIA_RUN", OwnerID: cp.ServiceID, Revision: 1, PreviousSnapshot: lifecycle.OptionalRef{State: lifecycle.Absent, AbsenceFactRef: "fixture-genesis", Observation: cpContext}, CoverageKind: "COMPLETE_ROSTER", Members: []lifecycle.Member{{Producer: producer, Resource: resource, SourceFrameRef: "worker-frame"}}, Coverage: lifecycle.Fact[lifecycle.ID]{State: lifecycle.Observed, Value: &coverage, Observation: cpContext}, SourceFrames: []lifecycle.SourceFrame{{FrameRef: "cp-frame", OwnerID: cp.ServiceID, Scope: domain.DomainID, SourceRevision: 1}, {FrameRef: "worker-frame", OwnerID: producer.ServiceID, Scope: domain.DomainID, SourceRevision: 1}}}}
	admission := lifecycle.Record{Header: header(lifecycle.CC03, "fixture-admission"), Admission: &lifecycle.AdmissionRecord{ReservationRefs: lifecycle.ReservationRefs{DomainClosureRef: "fixture-domain-closure", CPReservation: lifecycle.OptionalRef{State: lifecycle.Absent, AbsenceFactRef: "fixture-no-cp-reservation", Observation: workerWitnessFact("absent").Observation}}, MembershipSnapshot: domain.SnapshotRef, AdmissionEpoch: "fixture-ADMISSION", ActorGuardRefs: []lifecycle.ID{"fixture-writer-guard"}, Cut: []lifecycle.Frontier{{Kind: "EMPTY", Namespace: namespace, EmptyFactRef: "fixture-empty-writer"}}, TerminalPolicyRef: p.TerminalPolicyRef}}
	config := LifecycleParticipantConfig{Profile: p, Membership: membership, CurrentVector: current, Admission: admission, CompletionHeader: header(lifecycle.CC04, "fixture-completion"), Namespace: namespace, StreamID: "stream-01", ClosureID: "fixture-domain-closure", EmptyFactRef: "fixture-empty-writer", RegistrationEpoch: "fixture-registration-epoch", Witness: &workerWitnessFixture{}, Publisher: publisher, ReleaseTimeout: time.Second, EffectTarget: lifecycle.ResourceRef{Kind: lifecycle.Sink, OwnerID: "encoder-fixture", ResourceID: "caption-event-sink"}}
	return m, config
}

func TestIUWS3WorkerActualCaptionWriter(t *testing.T) {
	publisher := &workerPublisherFixture{}
	m, config := workerLifecycleFixture(t, publisher)
	if err := m.InstallLifecycleParticipant(config); err != nil {
		t.Fatal(err)
	}
	if _, err := m.CaptionForGeneration(t.Context(), "stream-01", 1, "synthetic caption", "fixture-speaker", time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	if publisher.calls.Load() != 1 {
		t.Fatal("actual publisher path not reached")
	}
	if result := m.CloseLifecycleAdmission(t.Context(), config.CurrentVector); result.Kind != lifecycle.StructurallyCoherent {
		t.Fatalf("cut: %+v", result)
	}
	record, result := m.LifecycleCompletionSnapshot(config.CurrentVector)
	if result.Kind != lifecycle.PendingObligation || record.Completion == nil || len(record.Completion.Effects) != 1 || len(record.Completion.PhysicalObligations) != 0 || result.ProductionQualified() || result.AuthorizesMutation() {
		t.Fatalf("provider accept became terminal: %+v", result)
	}
	if record.Completion.Partitions[0].TerminalFrontier.LastSequence == nil || *record.Completion.Partitions[0].TerminalFrontier.LastSequence != 1 {
		t.Fatal("actual writer release missing")
	}
}

func TestIUWS3WorkerCutRejectsLateCallback(t *testing.T) {
	publisher := &workerPublisherFixture{}
	m, config := workerLifecycleFixture(t, publisher)
	if err := m.InstallLifecycleParticipant(config); err != nil {
		t.Fatal(err)
	}
	if result := m.CloseLifecycleAdmission(t.Context(), config.CurrentVector); result.Kind != lifecycle.StructurallyCoherent {
		t.Fatal(result)
	}
	_, err := m.CaptionForGeneration(t.Context(), "stream-01", 1, "late callback", "fixture-speaker", time.Now())
	var failure *WorkerLifecycleError
	if !errors.As(err, &failure) || failure.Result.Kind != lifecycle.RefusedUnchanged || publisher.calls.Load() != 0 {
		t.Fatalf("closed admission dispatched: %v", err)
	}
	record, result := m.LifecycleCompletionSnapshot(config.CurrentVector)
	if result.Kind != lifecycle.StructurallyCoherent || record.Completion == nil || record.Completion.Partitions[0].FinalAcceptedFrontier.Kind != "EMPTY" || result.ProductionQualified() {
		t.Fatalf("empty registered writer snapshot: %+v", result)
	}
	if record.Header.RecordKey.RecordID != "fixture-completion-r5" || record.Header.RecordRevision != 5 {
		t.Fatal("unexpected owner snapshot revision")
	}
	repeat, repeatResult := m.LifecycleCompletionSnapshot(config.CurrentVector)
	if repeatResult.Kind != lifecycle.StructurallyCoherent || lifecycle.CompareImmutable(record, repeat, config.Profile).Kind != lifecycle.StructurallyCoherent {
		t.Fatal("same snapshot changed immutable content")
	}
}

func TestIUWS3WorkerBusyBorrowNotQueueEmpty(t *testing.T) {
	publisher := &workerPublisherFixture{entered: make(chan struct{}), release: make(chan struct{})}
	m, config := workerLifecycleFixture(t, publisher)
	witness := config.Witness.(*workerWitnessFixture)
	witness.secondDispatch = make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(publisher.release) }) }
	defer release()
	if err := m.InstallLifecycleParticipant(config); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 2)
	go func() {
		_, err := m.CaptionForGeneration(context.Background(), "stream-01", 1, "barrier caption", "fixture-speaker", time.Now())
		done <- err
	}()
	select {
	case <-publisher.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("writer did not reach barrier")
	}
	go func() {
		_, err := m.CaptionForGeneration(context.Background(), "stream-01", 1, "second barrier caption", "fixture-speaker", time.Now())
		done <- err
	}()
	select {
	case <-witness.secondDispatch:
	case <-time.After(3 * time.Second):
		t.Fatal("second writer was not registered")
	}
	if result := m.CloseLifecycleAdmission(t.Context(), config.CurrentVector); result.Kind != lifecycle.StructurallyCoherent {
		t.Fatal(result)
	}
	record, result := m.LifecycleCompletionSnapshot(config.CurrentVector)
	if result.Kind != lifecycle.Unknown || record.Completion == nil || len(record.Completion.PhysicalObligations) != 2 || len(record.Completion.Partitions[0].Holes) != 2 {
		t.Fatalf("empty queue erased writer borrow: %+v", result)
	}
	if record.Completion.PhysicalObligations[0].ObligationID != "writer-borrow-1" || record.Completion.PhysicalObligations[1].ObligationID != "writer-borrow-2" {
		t.Fatal("owner borrow projection is not in registration order")
	}
	first, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 64; i++ {
		same, sameResult := m.LifecycleCompletionSnapshot(config.CurrentVector)
		encoded, encodeErr := json.Marshal(same)
		if encodeErr != nil || sameResult.Kind != lifecycle.Unknown || same.Header.RecordKey != record.Header.RecordKey || !bytes.Equal(first, encoded) {
			t.Fatal("same immutable snapshot ID changed its byte layout")
		}
	}
	release()
	for i := 0; i < 2; i++ {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("writer did not release")
		}
	}
	released, result := m.LifecycleCompletionSnapshot(config.CurrentVector)
	if result.Kind != lifecycle.PendingObligation {
		t.Fatalf("local release conflated with external terminal: %+v", result)
	}
	if record.Header.RecordKey.RecordID == released.Header.RecordKey.RecordID || released.Header.RecordRevision != 11 {
		t.Fatal("changed state reused immutable snapshot ID")
	}
}

func TestIUWS3WorkerGenerationAndScope(t *testing.T) {
	m, config := workerLifecycleFixture(t, &workerPublisherFixture{})
	if err := m.InstallLifecycleParticipant(config); err != nil {
		t.Fatal(err)
	}
	changed := config.CurrentVector
	data, err := jsonCloneWorker(changed)
	if err != nil {
		t.Fatal(err)
	}
	changed = data
	for i := range changed.Vector.Vector {
		if changed.Vector.Vector[i].Key.Axis == lifecycle.JobAxis {
			n := uint64(2)
			changed.Vector.Vector[i].Value.Uint64 = &n
		}
	}
	if _, result := m.LifecycleCompletionSnapshot(changed); result.Kind != lifecycle.Stale {
		t.Fatalf("generation mismatch: %+v", result)
	}
	other := NewManager(&workerPublisherFixture{}, nil)
	other.ApplyRuntimeConfig(captionRuntimeConfig())
	if err := other.Start(t.Context(), StreamContext{StreamID: "stream-01"}); err != nil {
		t.Fatal(err)
	}
	defer other.Close(context.Background())
	if _, err := other.LifecycleProcessIncarnation(); err != nil {
		t.Fatal(err)
	}
	var failure *WorkerLifecycleError
	if err := other.InstallLifecycleParticipant(config); !errors.As(err, &failure) || failure.Result.Kind != lifecycle.Stale {
		t.Fatalf("restart reused old process frame: %v", err)
	}
}

func TestIUWS3WorkerWitnessUnknownRemainsHeld(t *testing.T) {
	publisher := &workerPublisherFixture{}
	m, config := workerLifecycleFixture(t, publisher)
	witness := config.Witness.(*workerWitnessFixture)
	witness.fail = lifecycle.BeginAdmissionCut
	if err := m.InstallLifecycleParticipant(config); err != nil {
		t.Fatal(err)
	}
	if result := m.CloseLifecycleAdmission(t.Context(), config.CurrentVector); result.Kind != lifecycle.Unknown {
		t.Fatal(result)
	}
	count := witness.count
	if result := m.CloseLifecycleAdmission(t.Context(), config.CurrentVector); result.Kind != lifecycle.Unknown || witness.count != count {
		t.Fatal("unknown cut was reissued")
	}
	if _, result := m.LifecycleCompletionSnapshot(config.CurrentVector); result.Kind != lifecycle.Unknown {
		t.Fatal(result)
	}
	if _, err := m.CaptionForGeneration(t.Context(), "stream-01", 1, "blocked caption", "fixture-speaker", time.Now()); err == nil || publisher.calls.Load() != 0 {
		t.Fatal("unknown participant admitted work")
	}
}

type unconnectedCaptionSession struct{}

func (unconnectedCaptionSession) Ingest(context.Context, deepgram.AudioPacket) error { return nil }
func (unconnectedCaptionSession) Close(context.Context) error                        { return nil }

func TestIUWS3WorkerUnsupportedWritersRefused(t *testing.T) {
	t.Run("legacy_dequeue_borrow", func(t *testing.T) {
		publisher := &workerPublisherFixture{entered: make(chan struct{}), release: make(chan struct{})}
		manager, frame := workerLifecycleFixture(t, publisher)
		var releaseOnce sync.Once
		release := func() { releaseOnce.Do(func() { close(publisher.release) }) }
		defer release()
		event := encoder.Event{ID: "fixture-legacy-queued-event", StreamID: "stream-01", Generation: 1, Type: "caption.final", Payload: map[string]any{"text": "legacy synthetic caption"}, Timestamp: time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)}
		if !manager.enqueueWorkerEvent(event) {
			t.Fatal("legacy event did not enter actual queue")
		}
		select {
		case <-publisher.entered:
		case <-time.After(3 * time.Second):
			t.Fatal("legacy dequeued event did not reach publisher barrier")
		}
		manager.mu.Lock()
		queued, ordinary, borrow := len(manager.pendingEvents), manager.eventWritesInFlight, manager.legacyDeliveryBorrows
		manager.mu.Unlock()
		if queued != 0 || ordinary != 0 || borrow != 1 {
			t.Fatal("dequeue-to-publish borrow is not retained independently of queue length")
		}
		var refusal *WorkerLifecycleError
		if err := manager.InstallLifecycleParticipant(frame); !errors.As(err, &refusal) || refusal.Result.Kind != lifecycle.NeedsEvidence {
			t.Fatal("omitted legacy borrow permitted EMPTY participant")
		}
		if _, result := manager.LifecycleCompletionSnapshot(frame.CurrentVector); result.Kind != lifecycle.NeedsEvidence {
			t.Fatal("omitted legacy writer emitted completion")
		}
		release()
		waitForManager(t, 3*time.Second, func() bool {
			manager.mu.Lock()
			defer manager.mu.Unlock()
			return manager.legacyDeliveryBorrows == 0 && len(manager.events) == 1
		})
		if err := manager.InstallLifecycleParticipant(frame); err == nil {
			t.Fatal("retiring a legacy borrow erased same-job history")
		}
	})
	for _, failed := range []bool{false, true} {
		name := "legacy_success_before_install"
		if failed {
			name = "legacy_failure_before_install"
		}
		t.Run(name, func(t *testing.T) {
			publisher := &workerPublisherFixture{}
			if failed {
				publisher.err = errors.New("synthetic legacy failure")
			}
			manager, frame := workerLifecycleFixture(t, publisher)
			_, err := manager.CaptionForGeneration(t.Context(), "stream-01", 1, "prior synthetic caption", "fixture-speaker", time.Now())
			if (err != nil) != failed {
				t.Fatal("legacy test outcome did not reach the selected boundary")
			}
			manager.mu.Lock()
			queued, active := len(manager.pendingEvents), manager.eventWritesInFlight
			manager.mu.Unlock()
			if queued != 0 || active != 0 {
				t.Fatal("legacy writer did not retire")
			}
			if err := manager.InstallLifecycleParticipant(frame); err == nil {
				t.Fatal("same-job legacy history became EMPTY coverage")
			}
		})
	}
	m, config := workerLifecycleFixture(t, &workerPublisherFixture{})
	m.mu.Lock()
	m.captionSession = unconnectedCaptionSession{}
	m.mu.Unlock()
	if err := m.InstallLifecycleParticipant(config); err == nil {
		t.Fatal("active unconnected provider was omitted")
	}
	m.mu.Lock()
	m.captionSession = nil
	m.mu.Unlock()
	if err := m.InstallLifecycleParticipant(config); err != nil {
		t.Fatal(err)
	}
	var resolves atomic.Int64
	m.ApplyRuntimeConfig(captionRuntimeConfig(control.RuntimeProfile{ID: "unconnected-profile", Kind: "caption", Config: captionProfileConfig("ja")}))
	m.SetCaptionRuntime(RuntimeSecretResolverFunc(func(context.Context, string, string) (control.RuntimeSecret, error) {
		resolves.Add(1)
		return control.RuntimeSecret{}, nil
	}), nil)
	var failure *WorkerLifecycleError
	if err := m.UpdateCaptionRuntimeSettings(t.Context(), "stream-01", "unconnected-profile"); !errors.As(err, &failure) || failure.Result.Kind != lifecycle.NeedsEvidence || resolves.Load() != 0 {
		t.Fatal("provider prepared before coverage refusal")
	}
}

func jsonCloneWorker(record lifecycle.Record) (lifecycle.Record, error) {
	data, err := json.Marshal(record)
	if err != nil {
		return lifecycle.Record{}, err
	}
	var copy lifecycle.Record
	err = json.Unmarshal(data, &copy)
	return copy, err
}

func TestIUWS3WorkerProviderACKLossNoRetry(t *testing.T) {
	publisher := &workerPublisherFixture{err: encoder.NewRetryablePublishError(503, "fixture-ack-loss")}
	m, config := workerLifecycleFixture(t, publisher)
	if err := m.InstallLifecycleParticipant(config); err != nil {
		t.Fatal(err)
	}
	if _, err := m.CaptionForGeneration(t.Context(), "stream-01", 1, "uncertain caption", "fixture-speaker", time.Now()); err == nil {
		t.Fatal("missing ACK became success")
	}
	m.mu.Lock()
	queued := len(m.pendingEvents)
	m.mu.Unlock()
	if queued != 0 || publisher.calls.Load() != 1 {
		t.Fatal("uncertain event entered legacy retry queue")
	}
	if result := m.CloseLifecycleAdmission(t.Context(), config.CurrentVector); result.Kind != lifecycle.StructurallyCoherent {
		t.Fatal(result)
	}
	_, result := m.LifecycleCompletionSnapshot(config.CurrentVector)
	if result.Kind != lifecycle.Unknown {
		t.Fatalf("provider UNKNOWN collapsed: %+v", result)
	}
}
