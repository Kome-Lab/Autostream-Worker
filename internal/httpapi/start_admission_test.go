package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	lifecycle "github.com/Kome-Lab/Autostream-Contracts/v3lifecycle"
	"image"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/example/autostream-worker/internal/encoder"
	"github.com/example/autostream-worker/internal/events"
	"github.com/example/autostream-worker/internal/jobs"
	"github.com/example/autostream-worker/internal/manualhold"
	"github.com/example/autostream-worker/internal/observability"
)

func msrTargetProducer(component lifecycle.ComponentID, serviceID string) lifecycle.ProducerRef {
	return lifecycle.ProducerRef{ComponentID: component, ServiceID: lifecycle.ID(serviceID), HostID: "synthetic-target", BootID: "synthetic-boot", ProcessIncarnation: lifecycle.ID("synthetic-process-" + strconv.Itoa(os.Getpid())), WriterID: "test-only-no-authority"}
}
func msrTargetRecord(t *testing.T, producer lifecycle.ProducerRef, streamID string, revision uint64, request, operation string) lifecycle.Record {
	t.Helper()
	resource := lifecycle.ResourceRef{Kind: lifecycle.Stream, OwnerID: producer.ServiceID, ResourceID: lifecycle.ID(streamID)}
	ref := lifecycle.ContractRef{ContractID: lifecycle.CC02, LogicalRevision: 1, SpecRef: lifecycle.SpecRef, ModuleCommitRef: manualhold.ModuleCommit}
	record := lifecycle.Record{Header: lifecycle.Header{ContractRef: ref, RecordKey: lifecycle.RecordKey{ContractRef: ref, OwnerID: "synthetic-cp", RecordID: lifecycle.ID(request)}, Correlation: lifecycle.Correlation{RequestID: lifecycle.ID(request), OperationID: lifecycle.ID(operation), AttemptID: lifecycle.ID("attempt-" + request), StepID: "start", ExecutionKey: lifecycle.ID("execution-" + request)}, DomainRef: lifecycle.DomainRef{DeploymentID: "synthetic-deployment", Realm: "synthetic-realm", DomainID: "synthetic-domain", MembershipRevision: 1, SnapshotRef: "membership-test", Resources: []lifecycle.ResourceRef{resource}}, ProducerRef: msrTargetProducer("C01", "synthetic-cp"), RecordRevision: revision, ProfileRef: lifecycle.ProfileRef{ProfileID: lifecycle.ExplicitResume, Revision: 1}}, Vector: &lifecycle.VectorRecord{}}
	for _, axis := range []lifecycle.Axis{lifecycle.Membership, lifecycle.Admission, lifecycle.Ownership, lifecycle.Fence, lifecycle.Boot, lifecycle.Process, lifecycle.RunAxis, lifecycle.ConfigAxis} {
		literal := lifecycle.ID("observed-" + string(axis))
		value := lifecycle.VersionedValue{Kind: lifecycle.PresentLiteral, Literal: &literal}
		if axis == lifecycle.Membership {
			v := uint64(1)
			value = lifecycle.VersionedValue{Kind: lifecycle.PresentUint64, Uint64: &v}
		}
		if axis == lifecycle.RunAxis {
			v := lifecycle.ID("run-test")
			value.Literal = &v
		}
		if axis == lifecycle.Boot {
			v := producer.BootID
			value.Literal = &v
		}
		if axis == lifecycle.Process {
			v := producer.ProcessIncarnation
			value.Literal = &v
		}
		record.Vector.Vector = append(record.Vector.Vector, lifecycle.RevisionEntry{Key: lifecycle.AxisKey{Axis: axis, OwnerID: resource.OwnerID, Resource: resource}, Incarnation: "synthetic-owner", Value: value, SourceFrameRef: "synthetic-cp-frame"})
	}
	if err := manualhold.Validate(record, streamID); err != nil {
		t.Fatal(err)
	}
	return record
}
func msrTargetGuard(t *testing.T, directory string, producer lifecycle.ProducerRef, inspector manualhold.Inspector) *manualhold.Guard {
	t.Helper()
	guard, err := manualhold.New(directory, inspector, func() (lifecycle.ProducerRef, error) { return producer, nil })
	if err != nil {
		t.Fatal(err)
	}
	guard.Require = func() bool { return true }
	return guard
}
func msrTargetRequest(t *testing.T, baseURL, route, token, body string, record *lifecycle.Record) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, baseURL+route, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	if record != nil {
		encoded, err := manualhold.Encode(*record)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set(manualhold.Header, encoded)
	}
	response, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, data
}

var msrLaunchMu sync.Mutex

func msrSyntheticLaunch(ctx context.Context) error {
	if barrier := os.Getenv("AUTOSTREAM_MSR_LAUNCH_BARRIER_FILE"); barrier != "" {
		if err := os.WriteFile(barrier+".reached", []byte("actual-launch-boundary"), 0600); err != nil {
			return err
		}
		for {
			if _, err := os.Stat(barrier + ".release"); err == nil {
				break
			} else if !errors.Is(err, os.ErrNotExist) {
				return err
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(5 * time.Millisecond):
			}
		}
	}
	file := os.Getenv("AUTOSTREAM_MSR_START_COUNT_FILE")
	if file == "" {
		return nil
	}
	msrLaunchMu.Lock()
	defer msrLaunchMu.Unlock()
	var count int
	if data, err := os.ReadFile(file); err == nil {
		count, err = strconv.Atoi(strings.TrimSpace(string(data)))
		if err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	f, err := os.OpenFile(file, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, err = f.WriteString(strconv.Itoa(count + 1))
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	return closeErr
}
func msrServeProcess(t *testing.T, handler http.Handler) {
	t.Helper()
	listen := os.Getenv("AUTOSTREAM_MSR_LISTEN")
	ready := os.Getenv("AUTOSTREAM_MSR_READY_FILE")
	if listen == "" || ready == "" {
		t.Fatal("explicit synthetic listener and ready file required")
	}
	listener, err := net.Listen("tcp", listen)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err = os.WriteFile(ready, []byte("http://"+listener.Addr().String()), 0600); err != nil {
		t.Fatal(err)
	}
	server := http.Server{Handler: handler, ReadHeaderTimeout: 3 * time.Second}
	if err = server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		t.Fatal(err)
	}
}

type msrActualVideoOutput struct{ calls atomic.Int32 }

func (v *msrActualVideoOutput) Start(ctx context.Context, _ jobs.StreamContext, _ jobs.VideoSceneConfig) error {
	if err := msrSyntheticLaunch(ctx); err != nil {
		return err
	}
	v.calls.Add(1)
	return nil
}
func (v *msrActualVideoOutput) Stop(context.Context, string) error { return nil }
func msrWorkerHTTP(t *testing.T, guard *manualhold.Guard, video *msrActualVideoOutput, token string) http.Handler {
	t.Setenv("AUTOSTREAM_NODE_CONFIG", filepath.Join(t.TempDir(), "synthetic-pending-node.yml"))
	manager, err := jobs.NewManagerWithStoppedTargetReceiptFile(encoder.NoopPublisher{}, observability.Client{}, filepath.Join(t.TempDir(), "stopped.json"))
	if err != nil {
		t.Fatal(err)
	}
	manager.StartAdmission = guard
	manager.SetVideoOutput(video)
	manager.SetSceneRenderer(msrBlankRenderer{})
	return NewServerWithRuntimeConfig("worker", manager, TokenVerifier{PlainToken: token}, nil)
}

const msrWorkerStartBody = "{\"stream_id\":\"stream-test\",\"archive_run_id\":\"run-test\",\"stream_name\":\"Synthetic MSR\",\"encoder_profile_id\":\"synthetic-encoder-profile\",\"video_width\":854,\"video_height\":480,\"video_fps\":30,\"video_ingest_url\":\"srt://synthetic.invalid:10000\",\"video_ingest_passphrase\":\"synthetic-test-only-passphrase\",\"video_ingest_pbkeylen\":16}"

func TestMSRWorkerActualHTTPFence(t *testing.T) {
	producer := msrTargetProducer("C06", "worker-test")
	var checks atomic.Int32
	inspector := manualhold.InspectorFunc(func(_ context.Context, r lifecycle.Record) (lifecycle.Record, error) { checks.Add(1); return r, nil })
	guard := msrTargetGuard(t, t.TempDir(), producer, inspector)
	video := &msrActualVideoOutput{}
	server := httptest.NewServer(msrWorkerHTTP(t, guard, video, "synthetic-service-token"))
	defer server.Close()
	start := msrTargetRecord(t, producer, "stream-test", 1, "old-start", "automatic-start")
	closeRecord := msrTargetRecord(t, producer, "stream-test", 2, "manual-stop", "manual-stop")
	raw, _ := json.Marshal(closeRecord)
	code, body := msrTargetRequest(t, server.URL, "/streams/stream-test/start-admission/close", "synthetic-service-token", string(raw), nil)
	if code != 200 {
		t.Fatalf("close %d %s", code, body)
	}
	code, body = msrTargetRequest(t, server.URL, "/jobs/start", "synthetic-service-token", msrWorkerStartBody, &start)
	if code != 409 || video.calls.Load() != 0 {
		t.Fatalf("delayed actual Job: %d %s launches=%d", code, body, video.calls.Load())
	}
	manual := msrTargetRecord(t, producer, "stream-test", 2, "manual-start", "manual-start")
	code, body = msrTargetRequest(t, server.URL, "/jobs/start", "synthetic-service-token", msrWorkerStartBody, &manual)
	if code != 202 || video.calls.Load() != 1 || checks.Load() < 3 {
		t.Fatalf("manual actual Job: %d %s launches=%d checks=%d", code, body, video.calls.Load(), checks.Load())
	}
	changed := strings.Replace(msrWorkerStartBody, "synthetic-test-only-passphrase", "different-test-only-passphrase", 1)
	code, body = msrTargetRequest(t, server.URL, "/jobs/start", "synthetic-service-token", changed, &manual)
	if code != 409 || video.calls.Load() != 1 {
		t.Fatalf("same request changed secret replay: %d %s", code, body)
	}
	_, _ = msrTargetRequest(t, server.URL, "/jobs/stream-test/stop", "synthetic-service-token", "{}", nil)
}
func TestMSRWorkerActualPrelaunchCurrentness(t *testing.T) {
	uncertain := httptest.NewRecorder()
	writeStartAdmissionError(uncertain, errors.Join(manualhold.ErrRefused, manualhold.ErrUnknown))
	if uncertain.Code != http.StatusServiceUnavailable {
		t.Fatal("UNKNOWN persistence result became refused unchanged")
	}
	producer := msrTargetProducer("C06", "worker-test")
	var checks atomic.Int32
	inspector := manualhold.InspectorFunc(func(_ context.Context, r lifecycle.Record) (lifecycle.Record, error) {
		if checks.Add(1) > 2 {
			return lifecycle.Record{}, manualhold.ErrRefused
		}
		return r, nil
	})
	guard := msrTargetGuard(t, t.TempDir(), producer, inspector)
	video := &msrActualVideoOutput{}
	server := httptest.NewServer(msrWorkerHTTP(t, guard, video, "synthetic-service-token"))
	defer server.Close()
	start := msrTargetRecord(t, producer, "stream-test", 1, "start", "automatic-start")
	code, body := msrTargetRequest(t, server.URL, "/jobs/start", "synthetic-service-token", msrWorkerStartBody, &start)
	if code != 409 || video.calls.Load() != 0 || checks.Load() != 3 {
		t.Fatalf("prelaunch fence: %d %s launches=%d checks=%d", code, body, video.calls.Load(), checks.Load())
	}
}
func TestMSRTargetProcess(t *testing.T) {
	if os.Getenv("AUTOSTREAM_MSR_TARGET_HELPER") != "1" {
		return
	}
	service := os.Getenv("AUTOSTREAM_MSR_SERVICE_ID")
	token := os.Getenv("AUTOSTREAM_MSR_TOKEN")
	directory := os.Getenv("AUTOSTREAM_MSR_GUARD_DIR")
	baseURL := os.Getenv("AUTOSTREAM_MSR_CP_URL")
	if service == "" || token == "" || directory == "" || baseURL == "" || !filepath.IsAbs(directory) {
		t.Fatal("explicit synthetic target configuration required")
	}
	inspector := manualhold.HTTPInspector{Config: func() (string, string, error) { return baseURL, token, nil }, HTTP: &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	guard := msrTargetGuard(t, directory, msrTargetProducer("C06", service), inspector)
	msrServeProcess(t, msrWorkerHTTP(t, guard, &msrActualVideoOutput{}, token))
}

type msrBlankRenderer struct{}

func (msrBlankRenderer) ConfigureDisplay(int, time.Duration, time.Duration, time.Duration, bool) {}
func (msrBlankRenderer) Reset(uint64, string, string)                                            {}
func (msrBlankRenderer) Clear(string)                                                            {}
func (msrBlankRenderer) Apply(uint64, events.OverlayEvent) error                                 { return nil }
func (msrBlankRenderer) RenderSize(width, height int, _ time.Time) (*image.RGBA, error) {
	return image.NewRGBA(image.Rect(0, 0, width, height)), nil
}
func (msrBlankRenderer) AvatarRefreshInterval() time.Duration { return time.Hour }
func (msrBlankRenderer) RefreshAvatars()                      {}
