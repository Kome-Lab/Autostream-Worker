//go:build iu_crossrepo

package jobs

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	lifecycle "github.com/Kome-Lab/Autostream-Contracts/v3lifecycle"
	"github.com/example/autostream-worker/internal/deepgram"
	"github.com/example/autostream-worker/internal/encoder"
)

type workerCrossLogs struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (l *workerCrossLogs) Write(data []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buffer.Write(data)
}
func (l *workerCrossLogs) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.buffer.String() }

type workerCrossState struct {
	Posts               int64
	LogLines            int64
	CaptionCues         int64
	TranscriptEntries   int64
	ProductionQualified bool
}

// This test adapter calls the existing product HTTP client once. Its explicit
// RetryMax=0 never inherits the production client's environment configuration.
// A legacy 2xx is only a transport ACK; the Manager keeps its effect pending.
type workerCrossPublisher struct{ client encoder.Client }

func (p workerCrossPublisher) PublishOnce(ctx context.Context, event encoder.Event) error {
	return p.client.Publish(ctx, event)
}

func startWorkerCrossEncoder(t *testing.T) (context.Context, string, *http.Client) {
	t.Helper()
	binary, source := os.Getenv("IU_WS3_ER_BINARY"), os.Getenv("IU_WS3_ER_SOURCE")
	if !filepath.IsAbs(binary) || !filepath.IsAbs(source) {
		t.Fatal("explicit fixed-source Encoder fixture binary required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Second)
	t.Cleanup(cancel)
	dir := t.TempDir()
	command := exec.CommandContext(ctx, binary, "-test.run=^TestIUWS3CrossRepositoryEncoderProcess$", "-test.v")
	command.Dir = source
	// The synthetic subprocess receives only its explicit fixture namespace;
	// it does not inherit provider credentials or runtime configuration.
	command.Env = []string{"IU_WS3_FIXTURE_DIR=" + dir, "IU_WS3_ER_SOURCE=" + source}
	var logs workerCrossLogs
	command.Stdout, command.Stderr = &logs, &logs
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	var waitError error
	go func() { waitError = command.Wait(); close(done) }()
	httpClient := &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	var endpoint string
	t.Cleanup(func() {
		if endpoint != "" {
			finishCtx, finishCancel := context.WithTimeout(context.Background(), 5*time.Second)
			request, err := http.NewRequestWithContext(finishCtx, http.MethodPost, endpoint+"/fixture/finish", nil)
			if err == nil {
				if response, requestErr := httpClient.Do(request); requestErr == nil {
					_ = response.Body.Close()
				}
			}
			finishCancel()
		} else {
			cancel()
		}
		select {
		case <-done:
			if waitError != nil {
				t.Errorf("Encoder fixture process failed: %v; %s", waitError, logs.String())
			}
		case <-time.After(10 * time.Second):
			cancel()
			t.Error("Encoder fixture did not terminate")
		}
	})
	for endpoint == "" {
		data, err := os.ReadFile(filepath.Join(dir, "server.url"))
		if err == nil && len(data) > 0 {
			parsed, parseErr := url.Parse(strings.TrimSpace(string(data)))
			if parseErr != nil || parsed.Scheme != "http" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Path != "" {
				t.Fatal("fixture endpoint is not a plain loopback HTTP origin")
			}
			ip := net.ParseIP(parsed.Hostname())
			if ip == nil || !ip.IsLoopback() {
				t.Fatal("fixture endpoint is not loopback")
			}
			endpoint = parsed.String()
			break
		}
		select {
		case <-done:
			t.Fatalf("Encoder fixture exited before readiness: %v; %s", waitError, logs.String())
		case <-ctx.Done():
			t.Fatal("Encoder fixture readiness unavailable")
		case <-time.After(20 * time.Millisecond):
		}
	}
	return ctx, endpoint, httpClient
}

func readWorkerCrossState(t *testing.T, ctx context.Context, client *http.Client, endpoint string) workerCrossState {
	t.Helper()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/fixture/worker-state", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 4097))
	if err != nil || len(data) > 4096 || response.StatusCode != http.StatusOK {
		t.Fatal("invalid bounded Worker fixture state")
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil {
		t.Fatal("Worker fixture state is not JSON")
	}
	for _, key := range []string{"Posts", "LogLines", "CaptionCues", "TranscriptEntries", "ProductionQualified"} {
		if len(fields[key]) == 0 || string(fields[key]) == "null" {
			t.Fatalf("required fixture state field absent: %s", key)
		}
	}
	var state workerCrossState
	if json.Unmarshal(data, &state) != nil || state.ProductionQualified {
		t.Fatal("invalid or qualified synthetic Worker fixture state")
	}
	return state
}

func runWorkerCrossMedia(t *testing.T, ackLoss bool) {
	t.Helper()
	ctx, endpoint, httpClient := startWorkerCrossEncoder(t)
	before := readWorkerCrossState(t, ctx, httpClient, endpoint)
	// The fake stream starts through the real product path. ER's
	// streamproc.writeStartMetadata writes one stream_process.started JSONL row
	// before any Worker dispatch; it is not a Worker recording or a receipt.
	const startupLogLines int64 = 1
	if before.Posts != 0 || before.LogLines != startupLogLines || before.CaptionCues != 0 || before.TranscriptEntries != 0 {
		t.Fatalf("unexpected source-defined startup baseline: %+v", before)
	}
	expectedLogLines := before.LogLines + 1
	prefix := "/worker-normal"
	if ackLoss {
		prefix = "/worker-ack-loss"
	}
	const token = "synthetic-only-not-a-credential"
	legacySentinel := &workerPublisherFixture{}
	manager, config := workerLifecycleFixture(t, legacySentinel, StreamContext{StreamID: "stream-01", EncoderRecorderURL: endpoint + prefix, StreamIngestToken: token})
	config.Publisher = workerCrossPublisher{client: encoder.Client{Config: encoder.Config{ServiceID: "worker-01", Timeout: 3 * time.Second, RetryMax: 0, RetryBaseDelay: time.Millisecond}, HTTP: httpClient}}
	if err := manager.InstallLifecycleParticipant(config); err != nil {
		t.Fatal(err)
	}
	stamp := time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)
	_, publishError := manager.CaptionTranscriptDetailedForGeneration(ctx, "stream-01", 1, deepgram.Transcript{Text: "cross-repository synthetic caption", SpeakerUserID: "fixture-speaker", UtteranceID: "fixture-utterance", Revision: 1, Final: true, ReceivedAt: stamp, UpdatedAt: stamp, StartedAt: stamp, EndedAt: stamp.Add(time.Second), Source: "fixture"})
	if ackLoss && publishError == nil {
		t.Fatal("lost Media ACK became success")
	}
	if !ackLoss && publishError != nil {
		t.Fatal(publishError)
	}
	if legacySentinel.calls.Load() != 0 {
		t.Fatal("legacy Manager publisher bypassed explicit single-attempt port")
	}
	manager.mu.Lock()
	pendingRetries := len(manager.pendingEvents)
	manager.mu.Unlock()
	if pendingRetries != 0 {
		t.Fatal("uncertain effect entered automatic retry queue")
	}
	after := readWorkerCrossState(t, ctx, httpClient, endpoint)
	// These are observed temporary file counts. They do not assert fsync,
	// durable receipt, shared CC08 peer integration, or provider completion.
	if after.Posts != 1 || after.LogLines != expectedLogLines || after.CaptionCues != 1 || after.TranscriptEntries != 1 {
		t.Fatalf("expected exactly one new Worker recording above startup baseline: %+v -> %+v", before, after)
	}
	if result := manager.CloseLifecycleAdmission(ctx, config.CurrentVector); result.Kind != lifecycle.StructurallyCoherent {
		t.Fatalf("local writer cut: %+v", result)
	}
	proof, result := manager.LifecycleCompletionSnapshot(config.CurrentVector)
	expected := lifecycle.PendingObligation
	if ackLoss {
		expected = lifecycle.Unknown
	}
	if result.Kind != expected || result.ProductionQualified() || result.AuthorizesMutation() || proof.Completion == nil || len(proof.Completion.Effects) != 1 || len(proof.Completion.RequiredEffects) != 1 || len(proof.Completion.PhysicalObligations) != 0 {
		t.Fatalf("legacy ACK or local release became remote completion: %+v", result)
	}
	if _, queryResult := manager.LifecycleCompletionSnapshot(config.CurrentVector); queryResult.Kind != expected {
		t.Fatal("result query changed knowledge")
	}
	if observed := readWorkerCrossState(t, ctx, httpClient, endpoint); observed.Posts != 1 || observed.LogLines != expectedLogLines || observed.CaptionCues != 1 || observed.TranscriptEntries != 1 {
		t.Fatal("query or cut replayed the external effect")
	}
}

func TestIUWS3CrossRepositoryWorkerMediaRecorded(t *testing.T) { runWorkerCrossMedia(t, false) }
func TestIUWS3CrossRepositoryWorkerMediaACKLoss(t *testing.T)  { runWorkerCrossMedia(t, true) }
