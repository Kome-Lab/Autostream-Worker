package manualhold

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"time"

	lifecycle "github.com/Kome-Lab/Autostream-Contracts/v3lifecycle"
)

const maxJournalBytes = 1 << 20
const maxJournalEntries = 128
const acquisitionTimeout = 5 * time.Second

type Producer func() (lifecycle.ProducerRef, error)

// Guard owns local exclusion and persistence, not CP authority. A lock file
// left by an interrupted owner is never expired, broken, or auto-reconciled.
type Guard struct {
	directory string
	inspector Inspector
	producer  Producer
	// Require lets trusted runtime composition reject a legacy/headerless request
	// even before a particular Stream has a local managed record.
	Require func() bool
}

type journalEntry struct {
	Record  lifecycle.Record
	Outcome lifecycle.BoundaryObservation
}

// journal is product-owned metadata persistence. Every frame/result inside it
// is an unchanged G0 DTO; no payload, secret, credential, or authority is stored.
type journal struct {
	ClosedFrame     *lifecycle.Record
	Acknowledgement *lifecycle.Record
	Entries         []journalEntry
}

type Lease struct {
	guard         *Guard
	streamID      string
	record        lifecycle.Record
	state         journal
	index         int
	release       func()
	launchChecked bool
	finished      bool
}

type leaseContextKey struct{}

// Enter may join only the private lease of this exact target guard and Stream.
// HTTP acceptance and the actual manager therefore use one physical exclusion.
func (g *Guard) Enter(ctx context.Context, streamID string) (context.Context, *Lease, error) {
	if ctx != nil {
		if lease, ok := ctx.Value(leaseContextKey{}).(*Lease); ok {
			if lease == nil || lease.guard != g || lease.streamID != streamID || lease.finished {
				return nil, nil, ErrRefused
			}
			return ctx, lease, nil
		}
	}
	lease, err := g.Begin(ctx, streamID, RecordFromContext(ctx))
	if err != nil || lease == nil {
		return ctx, lease, err
	}
	return context.WithValue(ctx, leaseContextKey{}, lease), lease, nil
}

func (l *Lease) Done() bool { return l == nil || l.finished }

// WithCurrent protects a later publication of an already-started preparation.
// It never registers or re-executes Start and cannot reopen a closed lane.
func (g *Guard) WithCurrent(ctx context.Context, streamID string, original lifecycle.Record, fn func() error) error {
	if g == nil || fn == nil || Validate(original, streamID) != nil {
		return ErrRefused
	}
	release, err := g.acquire(ctx, streamID)
	if err != nil {
		return err
	}
	defer release()
	state, err := g.load(streamID)
	if err != nil {
		return err
	}
	if original.Header.RecordRevision < highestRevision(state) || (state.ClosedFrame != nil && original.Header.RecordRevision <= state.ClosedFrame.Header.RecordRevision && original.Header.Correlation.OperationID != "manual-start") {
		return ErrRefused
	}
	observed := false
	for _, entry := range state.Entries {
		if Equal(entry.Record, original) {
			observed = true
		}
	}
	if !observed {
		return ErrUnknown
	}
	producer, err := g.ProducerRef()
	if err != nil {
		return ErrUnknown
	}
	if targetCurrent(original, producer) != nil {
		return ErrRefused
	}
	current, err := g.inspector.Check(ctx, original)
	if err != nil {
		return err
	}
	if !Equal(original, current) {
		return ErrRefused
	}
	return fn()
}

func New(directory string, inspector Inspector, producer Producer) (*Guard, error) {
	if strings.TrimSpace(directory) == "" || !filepath.IsAbs(directory) || inspector == nil || producer == nil {
		return nil, ErrUnknown
	}
	return &Guard{directory: filepath.Clean(directory), inspector: inspector, producer: producer}, nil
}

func (g *Guard) ProducerRef() (lifecycle.ProducerRef, error) {
	if g == nil || g.producer == nil {
		return lifecycle.ProducerRef{}, ErrUnknown
	}
	return g.producer()
}

// LocalProducer observes the actual Linux boot and process start identity.
// It does not qualify a Workload credential, Root, Clock, or authority.
func LocalProducer(component lifecycle.ComponentID, serviceID string) (lifecycle.ProducerRef, error) {
	if serviceID == "" || (component != "C01" && component != "C04" && component != "C06") || runtime.GOOS != "linux" {
		return lifecycle.ProducerRef{}, ErrUnknown
	}
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil || strings.TrimSpace(string(boot)) == "" {
		return lifecycle.ProducerRef{}, ErrUnknown
	}
	stat, err := os.ReadFile("/proc/self/stat")
	if err != nil {
		return lifecycle.ProducerRef{}, ErrUnknown
	}
	end := strings.LastIndexByte(string(stat), ')')
	if end < 0 {
		return lifecycle.ProducerRef{}, ErrUnknown
	}
	fields := strings.Fields(string(stat[end+1:]))
	if len(fields) <= 19 || fields[19] == "" {
		return lifecycle.ProducerRef{}, ErrUnknown
	}
	host, err := os.Hostname()
	if err != nil || host == "" {
		return lifecycle.ProducerRef{}, ErrUnknown
	}
	return lifecycle.ProducerRef{ComponentID: component, ServiceID: lifecycle.ID(serviceID), HostID: lifecycle.ID(host), BootID: lifecycle.ID(strings.TrimSpace(string(boot))), ProcessIncarnation: lifecycle.ID(fmt.Sprintf("pid-%d-start-%s", os.Getpid(), fields[19])), WriterID: "manual-start-admission"}, nil
}

func targetCurrent(record lifecycle.Record, producer lifecycle.ProducerRef) error {
	resource, err := Resource(record)
	if err != nil || resource.OwnerID != producer.ServiceID || producer.BootID == "" || producer.ProcessIncarnation == "" {
		return ErrRefused
	}
	for _, entry := range record.Vector.Vector {
		if entry.Key.Axis == lifecycle.Boot && (entry.Value.Literal == nil || *entry.Value.Literal != producer.BootID) {
			return ErrRefused
		}
		if entry.Key.Axis == lifecycle.Process && (entry.Value.Literal == nil || *entry.Value.Literal != producer.ProcessIncarnation) {
			return ErrRefused
		}
	}
	return nil
}

func idName(streamID string) string {
	h := sha256.Sum256([]byte(streamID)) // Stream and request IDs are non-secret product metadata.
	return hex.EncodeToString(h[:])
}

func (g *Guard) paths(streamID string) (string, string) {
	name := idName(streamID)
	return filepath.Join(g.directory, name+".json"), filepath.Join(g.directory, name+".lock")
}

func ensureDirectory(directory string) error {
	parent := filepath.Dir(directory)
	if parent != directory {
		info, err := os.Lstat(parent)
		if errors.Is(err, os.ErrNotExist) {
			if err := ensureDirectory(parent); err != nil {
				return err
			}
		} else if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return ErrUnknown
		}
	}
	if err := os.Mkdir(directory, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return ErrUnknown
	}
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return ErrUnknown
	}
	// Persist the parent link before a journal can depend on this directory.
	if parent != directory && syncDirectory(parent) != nil {
		return ErrUnknown
	}
	return nil
}

func syncDirectory(directory string) error {
	if runtime.GOOS == "windows" {
		// Windows native tests do not qualify POSIX directory-fsync durability.
		// Production storage qualification remains a separate explicit obligation.
		return nil
	}
	f, err := os.Open(directory)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func (g *Guard) acquire(ctx context.Context, streamID string) (func(), error) {
	if g == nil || ctx == nil || ctx.Err() != nil || streamID == "" {
		return nil, ErrUnknown
	}
	if err := ensureDirectory(g.directory); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, acquisitionTimeout)
	defer cancel()
	_, lockPath := g.paths(streamID)
	for {
		f, err := os.OpenFile(lockPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			var owner [24]byte
			if _, err = rand.Read(owner[:]); err == nil {
				_, err = f.Write(owner[:])
			}
			if err == nil {
				err = f.Sync()
			}
			info, infoErr := f.Stat()
			closeErr := f.Close()
			if err != nil || infoErr != nil || closeErr != nil || syncDirectory(g.directory) != nil {
				// The lock's durability is uncertain. Do not erase it as recovery.
				return nil, ErrUnknown
			}
			var once sync.Once
			return func() {
				once.Do(func() {
					current, inspectErr := os.Lstat(lockPath)
					if inspectErr == nil && current.Mode().IsRegular() && os.SameFile(info, current) {
						_ = os.Remove(lockPath)
						_ = syncDirectory(g.directory)
					}
				})
			}, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, ErrUnknown
		}
		// Waiting for the same exclusion is not replaying Start or breaking a
		// crashed owner's lock. Cancellation/timeout leaves it untouched.
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ErrUnknown
		case <-timer.C:
		}
	}
}

// checkDirectory reads every ancestor without creating storage during Inspect.
func checkDirectory(directory string) error {
	parent := filepath.Dir(directory)
	if parent != directory {
		if err := checkDirectory(parent); err != nil {
			return err
		}
	}
	info, err := os.Lstat(directory)
	if errors.Is(err, os.ErrNotExist) {
		return os.ErrNotExist
	}
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return ErrUnknown
	}
	return nil
}

func (g *Guard) load(streamID string) (journal, error) {
	if err := checkDirectory(g.directory); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return journal{}, nil
		}
		return journal{}, ErrUnknown
	}
	name, _ := g.paths(streamID)
	info, err := os.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return journal{}, nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return journal{}, ErrUnknown
	}
	f, err := os.Open(name)
	if err != nil {
		return journal{}, ErrUnknown
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return journal{}, ErrUnknown
	}
	data, err := io.ReadAll(io.LimitReader(f, maxJournalBytes+1))
	if err != nil || len(data) > maxJournalBytes {
		return journal{}, ErrUnknown
	}
	var state journal
	d := json.NewDecoder(strings.NewReader(string(data)))
	d.DisallowUnknownFields()
	if d.Decode(&state) != nil || len(state.Entries) > maxJournalEntries {
		return journal{}, ErrUnknown
	}
	var trailing any
	if !errors.Is(d.Decode(&trailing), io.EOF) {
		return journal{}, ErrUnknown
	}
	if (state.ClosedFrame == nil) != (state.Acknowledgement == nil) {
		return journal{}, ErrUnknown
	}
	if state.ClosedFrame != nil && (Validate(*state.ClosedFrame, streamID) != nil || Validate(*state.Acknowledgement, streamID) != nil || !SameSource(*state.ClosedFrame, *state.Acknowledgement) || state.Acknowledgement.Header.RecordKey.OwnerID != state.Acknowledgement.Header.ProducerRef.ServiceID || (state.Acknowledgement.Header.ProducerRef.ComponentID != "C04" && state.Acknowledgement.Header.ProducerRef.ComponentID != "C06")) {
		return journal{}, ErrUnknown
	}
	seen := map[lifecycle.ID]bool{}
	for _, entry := range state.Entries {
		id := entry.Record.Header.Correlation.RequestID
		observationKind := lifecycle.ValidateBoundaryObservation(entry.Outcome, Limits()).Kind
		if seen[id] || Validate(entry.Record, streamID) != nil || (observationKind != lifecycle.StructurallyCoherent && observationKind != lifecycle.Unknown) {
			return journal{}, ErrUnknown
		}
		seen[id] = true
	}
	return state, nil
}

func (g *Guard) save(streamID string, state journal) error {
	name, _ := g.paths(streamID)
	if info, err := os.Lstat(name); err == nil {
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return ErrUnknown
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return ErrUnknown
	}
	data, err := json.Marshal(state)
	if err != nil || len(data) > maxJournalBytes {
		return ErrUnknown
	}
	f, err := os.CreateTemp(g.directory, ".start-admission-*")
	if err != nil {
		return ErrUnknown
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err = f.Chmod(0o600); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		return ErrUnknown
	}
	if os.Rename(tmp, name) != nil || syncDirectory(g.directory) != nil {
		return ErrUnknown
	}
	return nil
}

func highestRevision(state journal) uint64 {
	var rev uint64
	if state.ClosedFrame != nil {
		rev = state.ClosedFrame.Header.RecordRevision
	}
	for _, entry := range state.Entries {
		if entry.Record.Header.RecordRevision > rev {
			rev = entry.Record.Header.RecordRevision
		}
	}
	return rev
}

func (g *Guard) Managed(streamID string) (bool, error) {
	if g == nil {
		return false, nil
	}
	state, err := g.load(streamID)
	return state.ClosedFrame != nil || len(state.Entries) != 0, err
}

func (g *Guard) Begin(ctx context.Context, streamID string, original *lifecycle.Record) (*Lease, error) {
	if original == nil {
		managed, err := g.Managed(streamID)
		if err != nil {
			return nil, ErrUnknown
		}
		enrolled, enrollmentErr := g.enrolled()
		if enrollmentErr != nil {
			return nil, ErrUnknown
		}
		required := g != nil && g.Require != nil && g.Require()
		if required {
			if err := g.Enroll(); err != nil {
				return nil, err
			}
		}
		if managed || enrolled || required {
			return nil, ErrRefused
		}
		return nil, nil // Unmanaged legacy path retains its pre-existing behavior.
	}
	if g == nil || Validate(*original, streamID) != nil {
		return nil, ErrRefused
	}
	producer, err := g.ProducerRef()
	if err != nil {
		return nil, ErrUnknown
	}
	if targetCurrent(*original, producer) != nil {
		return nil, ErrRefused
	}
	release, err := g.acquire(ctx, streamID)
	if err != nil {
		return nil, err
	}
	state, err := g.load(streamID)
	if err != nil {
		release()
		return nil, err
	}
	if original.Header.RecordRevision < highestRevision(state) {
		release()
		return nil, ErrRefused
	}
	for _, entry := range state.Entries {
		if entry.Record.Header.Correlation.RequestID == original.Header.Correlation.RequestID {
			release()
			return nil, ErrRefused // Never repeat even an identical raw private payload.
		}
	}
	if state.ClosedFrame != nil && state.ClosedFrame.Header.Correlation.RequestID == original.Header.Correlation.RequestID {
		release()
		return nil, ErrRefused
	}
	if _, exists, historyErr := g.readHistory(streamID, original.Header.Correlation.RequestID); historyErr != nil {
		release()
		return nil, historyErr
	} else if exists {
		release()
		return nil, ErrRefused
	}
	if state.ClosedFrame != nil && original.Header.RecordRevision <= state.ClosedFrame.Header.RecordRevision && original.Header.Correlation.OperationID != "manual-start" {
		release()
		return nil, ErrRefused
	}
	current, err := g.inspector.Check(ctx, *original)
	if err != nil || !Equal(*original, current) {
		release()
		if !errors.Is(err, ErrUnknown) && (errors.Is(err, ErrRefused) || (err == nil && !Equal(*original, current))) {
			return nil, ErrRefused
		}
		return nil, ErrUnknown
	}
	if err := g.Enroll(); err != nil {
		release()
		return nil, err
	}
	if len(state.Entries) >= maxJournalEntries {
		if err := g.archiveEntry(streamID, state.Entries[0]); err != nil {
			release()
			return nil, err
		}
		state.Entries = append([]journalEntry(nil), state.Entries[1:]...)
	}
	state.Entries = append(state.Entries, journalEntry{Record: *original, Outcome: pendingObservation(*original, producer)})
	if err := g.save(streamID, state); err != nil {
		release()
		return nil, err
	}
	return &Lease{guard: g, streamID: streamID, record: *original, state: state, index: len(state.Entries) - 1, release: release}, nil
}

func (l *Lease) BeforeLaunch(ctx context.Context) error {
	if l == nil {
		return nil
	}
	if l.finished || ctx == nil || ctx.Err() != nil {
		return ErrUnknown
	}
	producer, err := l.guard.ProducerRef()
	if err != nil {
		return ErrUnknown
	}
	if targetCurrent(l.record, producer) != nil {
		return ErrRefused
	}
	current, err := l.guard.inspector.Check(ctx, l.record)
	if err != nil {
		return err
	}
	if !Equal(l.record, current) {
		return ErrRefused
	}
	l.launchChecked = true
	return nil
}

func pendingObservation(record lifecycle.Record, producer lifecycle.ProducerRef) lifecycle.BoundaryObservation {
	resource, _ := Resource(record)
	resource.OwnerID = producer.ServiceID
	return lifecycle.BoundaryObservation{Address: lifecycle.BoundaryAddress{Kind: lifecycle.BackendMutation, OwnerID: producer.ServiceID, Resource: resource, SubBoundary: lifecycle.ServiceApply}, Knowledge: lifecycle.KnowledgeUnknown, Presence: lifecycle.NotObserved, Outcome: lifecycle.Fact[lifecycle.Outcome]{State: lifecycle.Unavailable, Unavailable: &lifecycle.UnavailableReason{Code: "TARGET_START_PENDING"}}}
}

func (l *Lease) Finish(cause error) error {
	if l == nil {
		return cause
	}
	if l.finished {
		return ErrUnknown
	}
	l.finished = true
	defer l.release()
	producer, err := l.guard.producer()
	if err != nil {
		return errors.Join(cause, ErrUnknown)
	}
	observation := pendingObservation(l.record, producer)
	var value lifecycle.Outcome
	if !l.launchChecked && cause != nil {
		value = lifecycle.NoEffectProven
		observation.DetailRef = l.record.Header.RecordKey.RecordID
	} else if l.launchChecked && cause == nil {
		value = lifecycle.SuccessAtBoundary
	}
	if value != "" {
		observation.Knowledge, observation.Presence = lifecycle.Known, lifecycle.Present
		observation.Outcome = lifecycle.Fact[lifecycle.Outcome]{State: lifecycle.Observed, Value: &value, Observation: &lifecycle.ObservationContext{OwnerID: producer.ServiceID, FrameRef: l.record.Header.RecordKey.RecordID, Scope: lifecycle.ID(l.streamID), SourceRevision: l.record.Header.RecordRevision}}
	}
	l.state.Entries[l.index].Outcome = observation
	if l.guard.save(l.streamID, l.state) != nil {
		return errors.Join(cause, ErrUnknown)
	}
	return cause
}

// Close is an actual target-local guard acknowledgement. It never terminates an
// already-running process/job. CP must observe all target ACKs before confirming
// its Hold; a failed/lost ACK remains UNKNOWN, not a reason to resend Start.
func (g *Guard) Close(ctx context.Context, streamID string, original lifecycle.Record) (lifecycle.Record, error) {
	if g == nil || Validate(original, streamID) != nil {
		return lifecycle.Record{}, ErrRefused
	}
	producer, err := g.ProducerRef()
	if err != nil {
		return lifecycle.Record{}, ErrUnknown
	}
	if targetCurrent(original, producer) != nil {
		return lifecycle.Record{}, ErrRefused
	}
	release, err := g.acquire(ctx, streamID)
	if err != nil {
		return lifecycle.Record{}, err
	}
	defer release()
	state, err := g.load(streamID)
	if err != nil {
		return lifecycle.Record{}, err
	}
	if state.ClosedFrame != nil && Equal(*state.ClosedFrame, original) {
		return *state.Acknowledgement, nil
	}
	if original.Header.RecordRevision <= highestRevision(state) {
		return lifecycle.Record{}, ErrRefused
	}
	if producer.ServiceID == "" || (producer.ComponentID != "C04" && producer.ComponentID != "C06") {
		return lifecycle.Record{}, ErrUnknown
	}
	ack := original
	ack.Header.ProducerRef = producer
	ack.Header.RecordKey.OwnerID = producer.ServiceID
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return lifecycle.Record{}, ErrUnknown
	}
	ack.Header.RecordKey.RecordID = lifecycle.ID("target-ack-" + hex.EncodeToString(nonce[:]))
	if Validate(ack, streamID) != nil {
		return lifecycle.Record{}, ErrUnknown
	}
	if err := g.Enroll(); err != nil {
		return lifecycle.Record{}, err
	}
	state.ClosedFrame, state.Acknowledgement = &original, &ack
	if err := g.save(streamID, state); err != nil {
		return lifecycle.Record{}, err
	}
	return ack, nil
}

// Inspect reads only the stored original Attempt and never changes a lane,
// journal, lock, target, or request. Caller authentication is owned by its API.
func (g *Guard) Inspect(streamID string, requestID lifecycle.ID) (lifecycle.Record, *lifecycle.BoundaryObservation, error) {
	if g == nil || requestID == "" {
		return lifecycle.Record{}, nil, ErrUnknown
	}
	state, err := g.load(streamID)
	if err != nil {
		return lifecycle.Record{}, nil, err
	}
	if state.ClosedFrame != nil && state.ClosedFrame.Header.Correlation.RequestID == requestID {
		return *state.Acknowledgement, nil, nil
	}
	for _, entry := range state.Entries {
		if entry.Record.Header.Correlation.RequestID == requestID {
			outcome := entry.Outcome
			return entry.Record, &outcome, nil
		}
	}
	if entry, exists, err := g.readHistory(streamID, requestID); err != nil {
		return lifecycle.Record{}, nil, err
	} else if exists {
		return entry.Record, &entry.Outcome, nil
	}
	return lifecycle.Record{}, nil, fmt.Errorf("%w: original attempt not observed", ErrUnknown)
}

const enrollmentContents = "IU-WS2-10-managed-runtime-v1\n"

// Enroll records only a trusted local protocol enrollment. It is not a Permit,
// release of Hold, or a credential. Absence of runtime config cannot erase it.
func (g *Guard) Enroll() error {
	if g == nil {
		return ErrUnknown
	}
	if err := ensureDirectory(g.directory); err != nil {
		return err
	}
	name := filepath.Join(g.directory, "managed-runtime")
	f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if errors.Is(err, os.ErrExist) {
		_, err = g.enrolled()
		return err
	}
	if err != nil {
		return ErrUnknown
	}
	_, writeErr := f.Write([]byte(enrollmentContents))
	syncErr := f.Sync()
	closeErr := f.Close()
	if writeErr != nil || syncErr != nil || closeErr != nil || syncDirectory(g.directory) != nil {
		return ErrUnknown
	}
	return nil
}
func (g *Guard) enrolled() (bool, error) {
	if g == nil {
		return false, nil
	}
	if err := checkDirectory(g.directory); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, ErrUnknown
	}
	name := filepath.Join(g.directory, "managed-runtime")
	info, err := os.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Size() != int64(len(enrollmentContents)) {
		return false, ErrUnknown
	}
	b, err := os.ReadFile(name)
	if err != nil || string(b) != enrollmentContents {
		return false, ErrUnknown
	}
	return true, nil
}
func (g *Guard) historyPath(streamID string, requestID lifecycle.ID) string {
	return filepath.Join(g.directory, idName(streamID)+".requests", idName(string(requestID))+".json")
}
func (g *Guard) readHistory(streamID string, requestID lifecycle.ID) (journalEntry, bool, error) {
	name := g.historyPath(streamID, requestID)
	if err := checkDirectory(filepath.Dir(name)); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return journalEntry{}, false, nil
		}
		return journalEntry{}, false, ErrUnknown
	}
	info, err := os.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return journalEntry{}, false, nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Size() > MaxRecordBytes*2 {
		return journalEntry{}, false, ErrUnknown
	}
	f, err := os.Open(name)
	if err != nil {
		return journalEntry{}, false, ErrUnknown
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return journalEntry{}, false, ErrUnknown
	}
	data, err := io.ReadAll(io.LimitReader(f, MaxRecordBytes*2+1))
	if err != nil || len(data) > MaxRecordBytes*2 {
		return journalEntry{}, false, ErrUnknown
	}
	var entry journalEntry
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&entry) != nil {
		return journalEntry{}, false, ErrUnknown
	}
	var extra any
	if !errors.Is(decoder.Decode(&extra), io.EOF) {
		return journalEntry{}, false, ErrUnknown
	}
	kind := lifecycle.ValidateBoundaryObservation(entry.Outcome, Limits()).Kind
	if entry.Record.Header.Correlation.RequestID != requestID || Validate(entry.Record, streamID) != nil || (kind != lifecycle.StructurallyCoherent && kind != lifecycle.Unknown) {
		return journalEntry{}, false, ErrUnknown
	}
	return entry, true, nil
}

// archiveEntry preserves every immutable seen request. Bounded active memory is
// not a lifetime Start cap or permission to forget an UNKNOWN attempt.
func (g *Guard) archiveEntry(streamID string, entry journalEntry) error {
	requestID := entry.Record.Header.Correlation.RequestID
	if previous, ok, err := g.readHistory(streamID, requestID); err != nil {
		return err
	} else if ok {
		if !Equal(previous.Record, entry.Record) || !reflect.DeepEqual(previous.Outcome, entry.Outcome) {
			return ErrUnknown
		}
		return nil
	}
	name := g.historyPath(streamID, requestID)
	directory := filepath.Dir(name)
	if err := ensureDirectory(directory); err != nil {
		return err
	}
	data, err := json.Marshal(entry)
	if err != nil || len(data) > MaxRecordBytes*2 {
		return ErrUnknown
	}
	file, err := os.CreateTemp(directory, ".request-*")
	if err != nil {
		return ErrUnknown
	}
	temporary := file.Name()
	defer os.Remove(temporary)
	if err = file.Chmod(0600); err == nil {
		_, err = file.Write(data)
	}
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil || closeErr != nil {
		return ErrUnknown
	}
	if os.Rename(temporary, name) != nil || syncDirectory(directory) != nil {
		return ErrUnknown
	}
	return nil
}
