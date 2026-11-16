package manualhold

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	lifecycle "github.com/Kome-Lab/Autostream-Contracts/v3lifecycle"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// Every producer and observation here is synthetic test-only metadata.
func testProducer() lifecycle.ProducerRef {
	return lifecycle.ProducerRef{ComponentID: "C04", ServiceID: "encoder-test", HostID: "synthetic-host", BootID: "synthetic-boot", ProcessIncarnation: "synthetic-process", WriterID: "test-only"}
}
func testRecord(t *testing.T, revision uint64, request, operation string) lifecycle.Record {
	t.Helper()
	producer := testProducer()
	resource := lifecycle.ResourceRef{Kind: lifecycle.Stream, OwnerID: producer.ServiceID, ResourceID: "stream-test"}
	ref := lifecycle.ContractRef{ContractID: lifecycle.CC02, LogicalRevision: 1, SpecRef: lifecycle.SpecRef, ModuleCommitRef: ModuleCommit}
	record := lifecycle.Record{Header: lifecycle.Header{ContractRef: ref, RecordKey: lifecycle.RecordKey{ContractRef: ref, OwnerID: "cp-test", RecordID: lifecycle.ID(request)}, Correlation: lifecycle.Correlation{RequestID: lifecycle.ID(request), OperationID: lifecycle.ID(operation), AttemptID: lifecycle.ID("attempt-" + request), StepID: "start", ExecutionKey: lifecycle.ID("execution-" + request)}, DomainRef: lifecycle.DomainRef{DeploymentID: "synthetic-deployment", Realm: "synthetic-realm", DomainID: "synthetic-domain", MembershipRevision: 1, SnapshotRef: "membership-test", Resources: []lifecycle.ResourceRef{resource}}, ProducerRef: lifecycle.ProducerRef{ComponentID: "C01", ServiceID: "cp-test", HostID: "synthetic-cp", BootID: "cp-boot", ProcessIncarnation: "cp-process", WriterID: "test-only"}, RecordRevision: revision, ProfileRef: lifecycle.ProfileRef{ProfileID: lifecycle.ExplicitResume, Revision: 1}}, Vector: &lifecycle.VectorRecord{}}
	for _, axis := range requiredAxes {
		literal := lifecycle.ID("observed-" + string(axis))
		value := lifecycle.VersionedValue{Kind: lifecycle.PresentLiteral, Literal: &literal}
		if axis == lifecycle.Membership {
			v := uint64(1)
			value = lifecycle.VersionedValue{Kind: lifecycle.PresentUint64, Uint64: &v}
		}
		if axis == lifecycle.Boot {
			v := producer.BootID
			value.Literal = &v
		}
		if axis == lifecycle.Process {
			v := producer.ProcessIncarnation
			value.Literal = &v
		}
		record.Vector.Vector = append(record.Vector.Vector, lifecycle.RevisionEntry{Key: lifecycle.AxisKey{Axis: axis, OwnerID: resource.OwnerID, Resource: resource}, Incarnation: "synthetic-owner-incarnation", Value: value, SourceFrameRef: "cp-current-observation"})
	}
	if got := lifecycle.ValidateRecord(record, Profile(record)); got.Kind != lifecycle.StructurallyCoherent {
		t.Fatalf("invalid test record: %+v", got)
	}
	return record
}
func newTestGuard(t *testing.T, directory string, inspector Inspector) *Guard {
	t.Helper()
	guard, err := New(directory, inspector, func() (lifecycle.ProducerRef, error) { return testProducer(), nil })
	if err != nil {
		t.Fatal(err)
	}
	return guard
}
func echoInspector() Inspector {
	return InspectorFunc(func(_ context.Context, r lifecycle.Record) (lifecycle.Record, error) { return r, nil })
}

func TestMSRTargetClosedFrameSurvivesRestartAndRejectsDelayedStart(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "admission")
	guard := newTestGuard(t, directory, echoInspector())
	closeRecord := testRecord(t, 2, "stop-2", "manual-stop")
	ack, err := guard.Close(t.Context(), "stream-test", closeRecord)
	if err != nil {
		t.Fatal(err)
	}
	if ack.Header.ProducerRef != testProducer() || ack.Header.RecordKey.OwnerID != testProducer().ServiceID || ack.Header.RecordKey.RecordID == closeRecord.Header.RecordKey.RecordID || !SameSource(closeRecord, ack) {
		t.Fatalf("not a target-owned acknowledgement: %+v", ack.Header)
	}
	restarted := newTestGuard(t, directory, echoInspector())
	if _, err := restarted.Begin(t.Context(), "stream-test", nil); !errors.Is(err, ErrRefused) {
		t.Fatalf("headerless managed fallback: %v", err)
	}
	old := testRecord(t, 1, "delayed-1", "automatic-start")
	if _, err := restarted.Begin(t.Context(), "stream-test", &old); !errors.Is(err, ErrRefused) {
		t.Fatalf("old frame passed: %v", err)
	}
	before, _ := os.ReadFile(func() string { p, _ := guard.paths("stream-test"); return p }())
	observed, outcome, err := restarted.Inspect("stream-test", closeRecord.Header.Correlation.RequestID)
	if err != nil || outcome != nil || !Equal(ack, observed) {
		t.Fatalf("inspect: %v", err)
	}
	after, _ := os.ReadFile(func() string { p, _ := guard.paths("stream-test"); return p }())
	if !bytes.Equal(before, after) {
		t.Fatal("Inspect wrote journal")
	}
	again, err := restarted.Close(t.Context(), "stream-test", closeRecord)
	if err != nil || !Equal(again, ack) {
		t.Fatalf("close acknowledgement changed: %v", err)
	}
}

func TestMSRTargetManualStartPreservesHoldAndRequestCannotReplay(t *testing.T) {
	guard := newTestGuard(t, t.TempDir(), echoInspector())
	closed := testRecord(t, 2, "stop", "manual-stop")
	if _, err := guard.Close(t.Context(), "stream-test", closed); err != nil {
		t.Fatal(err)
	}
	automatic := testRecord(t, 2, "auto", "automatic-start")
	if _, err := guard.Begin(t.Context(), "stream-test", &automatic); !errors.Is(err, ErrRefused) {
		t.Fatalf("automatic frame passed: %v", err)
	}
	manual := testRecord(t, 2, "manual", "manual-start")
	lease, err := guard.Begin(t.Context(), "stream-test", &manual)
	if err != nil {
		t.Fatal(err)
	}
	if err = lease.BeforeLaunch(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err = lease.Finish(nil); err != nil {
		t.Fatal(err)
	}
	if _, err = guard.Begin(t.Context(), "stream-test", &manual); !errors.Is(err, ErrRefused) {
		t.Fatalf("same request replayed: %v", err)
	}
	changed := manual
	changed.Header.Correlation.OperationID = "automatic-start"
	if _, err = guard.Begin(t.Context(), "stream-test", &changed); !errors.Is(err, ErrRefused) {
		t.Fatalf("same ID different operation replayed: %v", err)
	}
	state, err := guard.load("stream-test")
	if err != nil || state.ClosedFrame == nil || !Equal(*state.ClosedFrame, closed) || len(state.Entries) != 1 {
		t.Fatalf("manual Start altered Hold: %v", err)
	}
	_, outcome, err := guard.Inspect("stream-test", manual.Header.Correlation.RequestID)
	if err != nil || outcome == nil || outcome.Outcome.Value == nil || *outcome.Outcome.Value != lifecycle.SuccessAtBoundary {
		t.Fatalf("outcome missing: %v %+v", err, outcome)
	}
	resumed := testRecord(t, 3, "resumed-auto", "automatic-start")
	lease, err = guard.Begin(t.Context(), "stream-test", &resumed)
	if err != nil {
		t.Fatal(err)
	}
	if err = lease.BeforeLaunch(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err = lease.Finish(nil); err != nil {
		t.Fatal(err)
	}
}

func TestMSRTargetSecondLiveCheckAndUnknownRemainDistinct(t *testing.T) {
	t.Run("product_preparation_entered", func(t *testing.T) {
		var checks atomic.Int32
		guard := newTestGuard(t, t.TempDir(), InspectorFunc(func(_ context.Context, r lifecycle.Record) (lifecycle.Record, error) {
			if checks.Add(1) > 1 {
				return lifecycle.Record{}, ErrRefused
			}
			return r, nil
		}))
		original := testRecord(t, 1, "product-preparation", "automatic-start")
		lease, err := guard.Begin(t.Context(), "stream-test", &original)
		if err != nil {
			t.Fatal(err)
		}
		lease.MarkEffectsPossible()
		if err = lease.BeforeLaunch(t.Context()); !errors.Is(err, ErrRefused) {
			t.Fatalf("late currentness rejection: %v", err)
		}
		if err = lease.Finish(err); !errors.Is(err, ErrUnknown) || !errors.Is(err, ErrRefused) {
			t.Fatalf("possible product effects became unchanged: %v", err)
		}
		file, _ := guard.paths("stream-test")
		before, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		record, observation, err := guard.Inspect("stream-test", original.Header.Correlation.RequestID)
		if err != nil || !Equal(record, original) || observation == nil || observation.Knowledge != lifecycle.KnowledgeUnknown || observation.Outcome.State != lifecycle.Unavailable || observation.Outcome.Value != nil {
			t.Fatalf("possible effects did not remain UNKNOWN: %v %+v", err, observation)
		}
		after, err := os.ReadFile(file)
		if err != nil || !bytes.Equal(before, after) {
			t.Fatal("Inspect changed possible-effects record")
		}
	})
	var checks atomic.Int32
	guard := newTestGuard(t, t.TempDir(), InspectorFunc(func(_ context.Context, r lifecycle.Record) (lifecycle.Record, error) {
		if checks.Add(1) == 2 {
			return lifecycle.Record{}, ErrUnknown
		}
		return r, nil
	}))
	record := testRecord(t, 1, "start", "automatic-start")
	lease, err := guard.Begin(t.Context(), "stream-test", &record)
	if err != nil {
		t.Fatal(err)
	}
	if err = lease.BeforeLaunch(t.Context()); !errors.Is(err, ErrUnknown) {
		t.Fatalf("lost CP must be unknown: %v", err)
	}
	if err = lease.Finish(ErrUnknown); !errors.Is(err, ErrUnknown) {
		t.Fatal(err)
	}
	_, observed, err := guard.Inspect("stream-test", record.Header.Correlation.RequestID)
	if err != nil || observed == nil || observed.Outcome.Value == nil || *observed.Outcome.Value != lifecycle.NoEffectProven {
		t.Fatalf("launch was not entered: %v %+v", err, observed)
	}
	if _, err = guard.Begin(t.Context(), "stream-test", &record); !errors.Is(err, ErrRefused) {
		t.Fatal("unknown request replayed")
	}
}

func TestMSRTargetHoldWaitsForLaunchExclusionAcrossGuards(t *testing.T) {
	t.Run("stop_joins_owned_lease", func(t *testing.T) {
		guard := newTestGuard(t, t.TempDir(), echoInspector())
		original := testRecord(t, 1, "internal-stop", "manual-start")
		ctx, lease, err := guard.Enter(WithRecord(t.Context(), original), "stream-test")
		if err != nil {
			t.Fatal(err)
		}
		calls := 0
		joined, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
		defer cancel()
		if err = guard.WithStop(joined, "stream-test", func() error { calls++; return nil }); err != nil || calls != 1 {
			t.Fatalf("owned Stop did not join: %v calls=%d", err, calls)
		}
		if err = lease.Finish(ErrRefused); !errors.Is(err, ErrUnknown) {
			t.Fatal("joined Stop effects became unchanged")
		}
		state, err := guard.load("stream-test")
		if err != nil || state.ClosedFrame != nil {
			t.Fatalf("Stop created Hold: %v", err)
		}
		var legacy *Guard
		if err = legacy.WithStop(t.Context(), "", func() error { calls++; return nil }); err != nil || calls != 2 {
			t.Fatal("nil guard changed legacy Stop")
		}
	})
	directory := t.TempDir()
	first := newTestGuard(t, directory, echoInspector())
	second := newTestGuard(t, directory, echoInspector())
	start := testRecord(t, 1, "start", "automatic-start")
	lease, err := first.Begin(t.Context(), "stream-test", &start)
	if err != nil {
		t.Fatal(err)
	}
	if err = lease.BeforeLaunch(t.Context()); err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	done := make(chan error, 1)
	closed := testRecord(t, 2, "close", "manual-stop")
	go func() { close(entered); _, err := second.Close(t.Context(), "stream-test", closed); done <- err }()
	<-entered
	select {
	case err := <-done:
		t.Fatalf("Hold acknowledged while launch lease held: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	if err = lease.Finish(nil); err != nil {
		t.Fatal(err)
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	if _, err = first.Begin(t.Context(), "stream-test", &start); !errors.Is(err, ErrRefused) {
		t.Fatalf("old request after ACK: %v", err)
	}
}

func TestMSRTargetRestartIdentityAndCorruptStoreReject(t *testing.T) {
	directory := t.TempDir()
	guard := newTestGuard(t, directory, echoInspector())
	record := testRecord(t, 1, "start", "automatic-start")
	restarted := newTestGuard(t, directory, echoInspector())
	restarted.producer = func() (lifecycle.ProducerRef, error) {
		p := testProducer()
		p.ProcessIncarnation = "restarted-process"
		return p, nil
	}
	if _, err := restarted.Begin(t.Context(), "stream-test", &record); !errors.Is(err, ErrRefused) {
		t.Fatalf("old process frame passed: %v", err)
	}
	if _, err := guard.Close(t.Context(), "stream-test", testRecord(t, 2, "close", "manual-stop")); err != nil {
		t.Fatal(err)
	}
	file, _ := guard.paths("stream-test")
	if err := os.WriteFile(file, []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := guard.Begin(t.Context(), "stream-test", nil); !errors.Is(err, ErrUnknown) {
		t.Fatalf("corrupt managed store became legacy: %v", err)
	}
}

func TestMSRTargetHTTPInspectorUsesTrustedOriginAndNoRetries(t *testing.T) {
	record := testRecord(t, 1, "start", "automatic-start")
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/services/streams/stream-test/start-admission/inspect" || r.Header.Get("Authorization") != "Bearer test-service-only" {
			t.Error("trusted request binding lost")
			w.WriteHeader(403)
			return
		}
		_ = json.NewEncoder(w).Encode(record)
	}))
	defer server.Close()
	inspector := HTTPInspector{Config: func() (string, string, error) { return server.URL, "test-service-only", nil }, HTTP: server.Client()}
	if current, err := inspector.Check(t.Context(), record); err != nil || !Equal(current, record) {
		t.Fatalf("inspect: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatal("unexpected retries")
	}
	encoded, err := Encode(record)
	if err != nil {
		t.Fatal(err)
	}
	if decoded, err := Decode(encoded); err != nil || !Equal(decoded, record) {
		t.Fatalf("header roundtrip: %v", err)
	}
	raw, _ := json.Marshal(record)
	duplicate := append([]byte("{\"Header\":{},"), raw[1:]...)
	if _, err := DecodeJSON(duplicate); !errors.Is(err, ErrRefused) {
		t.Fatal("duplicate key accepted")
	}
}

func TestMSRTargetEnrollmentSurvivesConfigurationLoss(t *testing.T) {
	directory := t.TempDir()
	guard := newTestGuard(t, directory, echoInspector())
	guard.Require = func() bool { return true }
	if _, err := guard.Begin(t.Context(), "not-yet-started", nil); !errors.Is(err, ErrRefused) {
		t.Fatal(err)
	}
	restarted := newTestGuard(t, directory, echoInspector())
	restarted.Require = func() bool { return false }
	if _, err := restarted.Begin(t.Context(), "different-stream", nil); !errors.Is(err, ErrRefused) {
		t.Fatalf("configuration loss restored legacy: %v", err)
	}
}

func TestMSRTargetHistorySegmentationNeverForgetsRequest(t *testing.T) {
	directory := t.TempDir()
	guard := newTestGuard(t, directory, echoInspector())
	original := testRecord(t, 1, "request-0", "automatic-start")
	for i := 0; i <= maxJournalEntries; i++ {
		record := testRecord(t, uint64(i+1), fmt.Sprintf("request-%d", i), "automatic-start")
		lease, err := guard.Begin(t.Context(), "stream-test", &record)
		if err != nil {
			t.Fatalf("attempt %d: %v", i, err)
		}
		if err = lease.BeforeLaunch(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err = lease.Finish(nil); err != nil {
			t.Fatal(err)
		}
	}
	state, err := guard.load("stream-test")
	if err != nil || len(state.Entries) != maxJournalEntries {
		t.Fatalf("active index unbounded: %v", err)
	}
	restarted := newTestGuard(t, directory, echoInspector())
	observed, outcome, err := restarted.Inspect("stream-test", original.Header.Correlation.RequestID)
	if err != nil || !Equal(observed, original) || outcome == nil || outcome.Outcome.Value == nil || *outcome.Outcome.Value != lifecycle.SuccessAtBoundary {
		t.Fatalf("segmented original missing: %v", err)
	}
	original.Header.RecordRevision = uint64(maxJournalEntries + 10)
	if _, err = restarted.Begin(t.Context(), "stream-test", &original); !errors.Is(err, ErrRefused) {
		t.Fatalf("archived RequestID replayed: %v", err)
	}
}

func TestMSRTargetProcessBarrier(t *testing.T) {
	if directory := os.Getenv("AUTOSTREAM_MSR_GUARD_CHILD_DIR"); directory != "" {
		connection, err := net.DialTimeout("tcp", os.Getenv("AUTOSTREAM_MSR_GUARD_BARRIER"), time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer connection.Close()
		if _, err = connection.Write([]byte{1}); err != nil {
			t.Fatal(err)
		}
		guard := newTestGuard(t, directory, echoInspector())
		closed := testRecord(t, 2, "process-close", "manual-stop")
		if os.Getenv("AUTOSTREAM_MSR_GUARD_STOP") == "true" {
			err = guard.WithStop(t.Context(), "stream-test", func() error { return nil })
		} else {
			_, err = guard.Close(t.Context(), "stream-test", closed)
		}
		if err != nil {
			t.Fatal(err)
		}
		if _, err = connection.Write([]byte{2}); err != nil {
			t.Fatal(err)
		}
		return
	}
	t.Run("hold_close", func(t *testing.T) { runMSRProcessBarrier(t, false) })
	t.Run("stop_exclusion", func(t *testing.T) { runMSRProcessBarrier(t, true) })
}

func runMSRProcessBarrier(t *testing.T, stopOnly bool) {
	directory := t.TempDir()
	guard := newTestGuard(t, directory, echoInspector())
	start := testRecord(t, 1, "process-start", "automatic-start")
	lease, err := guard.Begin(t.Context(), "stream-test", &start)
	if err != nil {
		t.Fatal(err)
	}
	if err = lease.BeforeLaunch(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if !lease.Done() {
			_ = lease.Finish(ErrUnknown)
		}
	}()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestMSRTargetProcessBarrier$")
	command.Env = append(os.Environ(), "AUTOSTREAM_MSR_GUARD_CHILD_DIR="+directory, "AUTOSTREAM_MSR_GUARD_BARRIER="+listener.Addr().String(), "AUTOSTREAM_MSR_GUARD_STOP="+fmt.Sprint(stopOnly))
	var output bytes.Buffer
	command.Stdout = &output
	command.Stderr = &output
	if err = command.Start(); err != nil {
		t.Fatal(err)
	}
	connection, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	_ = connection.SetReadDeadline(time.Now().Add(2 * time.Second))
	var signal [1]byte
	if _, err = io.ReadFull(connection, signal[:]); err != nil || signal[0] != 1 {
		t.Fatalf("child barrier: %v", err)
	}
	_ = connection.SetReadDeadline(time.Now().Add(30 * time.Millisecond))
	if _, err = io.ReadFull(connection, signal[:]); err == nil {
		t.Fatal("independent process ACK crossed held launch boundary")
	} else if timeout, ok := err.(net.Error); !ok || !timeout.Timeout() {
		t.Fatal(err)
	}
	if err = lease.Finish(nil); err != nil {
		t.Fatal(err)
	}
	_ = connection.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err = io.ReadFull(connection, signal[:]); err != nil || signal[0] != 2 {
		t.Fatalf("child close ACK: %v", err)
	}
	if err = command.Wait(); err != nil {
		t.Fatalf("child: %v %s", err, output.String())
	}
	if stopOnly {
		state, err := guard.load("stream-test")
		if err != nil || state.ClosedFrame != nil {
			t.Fatalf("ordinary Stop created Hold: %v", err)
		}
		if _, err = guard.Begin(t.Context(), "stream-test", &start); !errors.Is(err, ErrRefused) {
			t.Fatalf("completed Start replayed after Stop ACK: %v", err)
		}
		return
	}
	old := testRecord(t, 1, "late-process-start", "automatic-start")
	if _, err = guard.Begin(t.Context(), "stream-test", &old); !errors.Is(err, ErrRefused) {
		t.Fatalf("late frame crossed closed process guard: %v", err)
	}
}

func TestMSRTargetProcessACKLossKeepsUnknownAndExclusion(t *testing.T) {
	if directory := os.Getenv("AUTOSTREAM_MSR_GUARD_CRASH_DIR"); directory != "" {
		guard := newTestGuard(t, directory, echoInspector())
		original := testRecord(t, 1, "lost-ack-start", "automatic-start")
		if _, err := guard.Begin(t.Context(), "stream-test", &original); err != nil {
			t.Fatal(err)
		}
		connection, err := net.DialTimeout("tcp", os.Getenv("AUTOSTREAM_MSR_GUARD_BARRIER"), time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = connection.Write([]byte{1}); err != nil {
			t.Fatal(err)
		}
		_ = connection.Close()
		os.Exit(0) // Intentional process loss after durable pending registration; no Finish.
	}
	directory := t.TempDir()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestMSRTargetProcessACKLossKeepsUnknownAndExclusion$")
	command.Env = append(os.Environ(), "AUTOSTREAM_MSR_GUARD_CRASH_DIR="+directory, "AUTOSTREAM_MSR_GUARD_BARRIER="+listener.Addr().String())
	var output bytes.Buffer
	command.Stdout = &output
	command.Stderr = &output
	if err = command.Start(); err != nil {
		t.Fatal(err)
	}
	connection, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	_ = connection.SetReadDeadline(time.Now().Add(2 * time.Second))
	var signal [1]byte
	if _, err = io.ReadFull(connection, signal[:]); err != nil || signal[0] != 1 {
		t.Fatalf("durable-registration barrier: %v", err)
	}
	_ = connection.Close()
	if err = command.Wait(); err != nil {
		t.Fatalf("child loss: %v %s", err, output.String())
	}
	restarted := newTestGuard(t, directory, echoInspector())
	_, outcome, err := restarted.Inspect("stream-test", "lost-ack-start")
	if err != nil || outcome == nil || outcome.Knowledge != lifecycle.KnowledgeUnknown || outcome.Outcome.State != lifecycle.Unavailable {
		t.Fatalf("lost ACK became known: %v %+v", err, outcome)
	}
	request := testRecord(t, 2, "new-request", "automatic-start")
	wait, cancelWait := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancelWait()
	if _, err = restarted.Begin(wait, "stream-test", &request); !errors.Is(err, ErrUnknown) {
		t.Fatalf("crashed-owner exclusion bypassed: %v", err)
	}
	_, lock := restarted.paths("stream-test")
	if _, err = os.Stat(lock); err != nil {
		t.Fatal("crashed lock was automatically removed")
	}
	state, err := restarted.load("stream-test")
	if err != nil || len(state.Entries) != 1 || state.ClosedFrame != nil {
		t.Fatalf("UNKNOWN inspection/rejection changed state: %v", err)
	}
}
