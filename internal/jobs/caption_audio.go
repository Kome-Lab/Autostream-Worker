package jobs

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/example/autostream-worker/internal/deepgram"
)

type captionAudioBatch struct {
	streamID             string
	generation           uint64
	connectionGeneration uint64
	packets              []deepgram.AudioPacket
	bytes                int
}

type captionAudioIngress struct {
	ctx     context.Context
	cancel  context.CancelFunc
	done    chan struct{}
	batches chan captionAudioBatch

	mu            sync.Mutex
	queuedPackets int
	queuedBytes   int
}

func newCaptionAudioIngress() *captionAudioIngress {
	ctx, cancel := context.WithCancel(context.Background())
	return &captionAudioIngress{
		ctx:     ctx,
		cancel:  cancel,
		done:    make(chan struct{}),
		batches: make(chan captionAudioBatch, captionAudioQueueBatches),
	}
}

func (q *captionAudioIngress) enqueue(batch captionAudioBatch) bool {
	if len(batch.packets) == 0 || len(batch.packets) > captionAudioQueuePackets || batch.bytes < 0 || batch.bytes > captionAudioQueueBytes {
		return false
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.ctx.Err() != nil || q.queuedPackets+len(batch.packets) > captionAudioQueuePackets || q.queuedBytes+batch.bytes > captionAudioQueueBytes {
		return false
	}
	select {
	case q.batches <- batch:
		q.queuedPackets += len(batch.packets)
		q.queuedBytes += batch.bytes
		return true
	default:
		return false
	}
}

func (q *captionAudioIngress) markDequeued(batch captionAudioBatch) {
	q.mu.Lock()
	q.queuedPackets -= len(batch.packets)
	q.queuedBytes -= batch.bytes
	if q.queuedPackets < 0 {
		q.queuedPackets = 0
	}
	if q.queuedBytes < 0 {
		q.queuedBytes = 0
	}
	q.mu.Unlock()
}

func (q *captionAudioIngress) snapshot() (batches, packets, bytes int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.batches), q.queuedPackets, q.queuedBytes
}

// classifyCaptionAudioError intentionally returns only a stable, safe class.
// Deepgram's transport errors can contain endpoint details, while the packet
// itself may carry user-associated identifiers; neither belongs in telemetry.
func classifyCaptionAudioError(err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return "context_canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "context_deadline"
	case errors.Is(err, deepgram.ErrEmptyAudio):
		return "invalid_audio"
	case errors.Is(err, deepgram.ErrClosed):
		return "deepgram_session_closed"
	case errors.Is(err, deepgram.ErrStaleConnectionGeneration):
		return "connection_generation_stale"
	}
	if errorClass, ok := deepgram.SafeErrorClass(err); ok {
		return errorClass
	}
	if errors.Is(err, deepgram.ErrUnavailable) {
		return "deepgram_unavailable"
	}
	return "caption_audio_ingest_failed"
}

func (m *Manager) runCaptionAudioIngress(ingress *captionAudioIngress) {
	defer close(ingress.done)
	for {
		select {
		case <-ingress.ctx.Done():
			return
		case batch := <-ingress.batches:
			ingress.markDequeued(batch)
			if ingress.ctx.Err() != nil {
				return
			}
			batchCtx, cancel := context.WithTimeout(ingress.ctx, captionAudioBatchTimeout)
			m.processCaptionAudioBatch(batchCtx, ingress, batch)
			cancel()
		}
	}
}

type captionAudioFailure struct {
	packet     deepgram.AudioPacket
	errorClass string
	httpStatus int
	retryable  bool
	batchWide  bool
}

func newCaptionAudioFailure(packet deepgram.AudioPacket, err error) captionAudioFailure {
	httpStatus, _ := deepgram.SafeHTTPStatus(err)
	return captionAudioFailure{
		packet:     packet,
		errorClass: classifyCaptionAudioError(err),
		httpStatus: httpStatus,
		retryable:  deepgram.IsRetryable(err),
		batchWide:  errors.Is(err, deepgram.ErrUnavailable) || errors.Is(err, deepgram.ErrClosed),
	}
}

func (m *Manager) processCaptionAudioBatch(ctx context.Context, ingress *captionAudioIngress, batch captionAudioBatch) {
	remaining := batch.packets
	retryCount := 0
	var failures []captionAudioFailure
	for attempt := 1; attempt <= captionAudioRetryMax; attempt++ {
		if !m.captionAudioGenerationCurrent(batch.streamID, batch.generation, batch.connectionGeneration, ingress) {
			return
		}
		var err error
		failures, _, err = m.ingestCaptionAudioAttempt(ctx, batch.streamID, batch.generation, batch.connectionGeneration, remaining)
		if err != nil || len(failures) == 0 || ingress.ctx.Err() != nil {
			return
		}
		if !captionAudioFailuresRetryable(failures) {
			break
		}
		if attempt == captionAudioRetryMax || ctx.Err() != nil {
			break
		}
		timer := time.NewTimer(captionAudioRetryDelay(retryCount + 1))
		select {
		case <-ingress.ctx.Done():
			timer.Stop()
			return
		case <-ctx.Done():
			timer.Stop()
			m.recordCaptionAudioProviderDrop(batch.streamID, batch.generation, batch.connectionGeneration, failures, retryCount)
			return
		case <-timer.C:
		}
		retryCount++
		if !m.recordCaptionAudioRetry(batch.streamID, batch.generation, batch.connectionGeneration, ingress) {
			return
		}
		remaining = captionAudioFailurePackets(failures)
	}
	if ingress.ctx.Err() == nil {
		m.recordCaptionAudioProviderDrop(batch.streamID, batch.generation, batch.connectionGeneration, failures, retryCount)
	}
}

func (m *Manager) captionAudioGenerationCurrent(streamID string, generation, connectionGeneration uint64, ingress *captionAudioIngress) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.current.StreamID == streamID &&
		m.jobGeneration == generation &&
		m.captionAudioConnectionGeneration == connectionGeneration &&
		m.captionIngress == ingress &&
		!m.stopping
}

func captionAudioRetryDelay(retryCount int) time.Duration {
	delay := captionAudioRetryBase
	for i := 1; i < retryCount; i++ {
		delay *= 2
	}
	return delay
}

func captionAudioFailurePackets(failures []captionAudioFailure) []deepgram.AudioPacket {
	packets := make([]deepgram.AudioPacket, len(failures))
	for i, failure := range failures {
		packets[i] = failure.packet
	}
	return packets
}

func captionAudioFailureClass(failures []captionAudioFailure) string {
	if len(failures) == 0 {
		return "unknown"
	}
	class := failures[0].errorClass
	for _, failure := range failures[1:] {
		if failure.errorClass != class {
			return "mixed"
		}
	}
	return class
}

func captionAudioFailuresRetryable(failures []captionAudioFailure) bool {
	if len(failures) == 0 {
		return false
	}
	for _, failure := range failures {
		if !failure.retryable {
			return false
		}
	}
	return true
}

func captionAudioFailureHTTPStatus(failures []captionAudioFailure) int {
	if len(failures) == 0 || failures[0].httpStatus == 0 {
		return 0
	}
	status := failures[0].httpStatus
	for _, failure := range failures[1:] {
		if failure.httpStatus != status {
			return 0
		}
	}
	return status
}

func (m *Manager) recordCaptionAudioRetry(streamID string, generation, connectionGeneration uint64, ingress *captionAudioIngress) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.current.StreamID != streamID || m.jobGeneration != generation || m.captionAudioConnectionGeneration != connectionGeneration || m.captionIngress != ingress || m.stopping {
		return false
	}
	m.captionAudioRetries++
	return true
}

func (m *Manager) recordCaptionAudioProviderDrop(streamID string, generation, connectionGeneration uint64, failures []captionAudioFailure, retryCount int) {
	if len(failures) == 0 {
		return
	}
	m.mu.Lock()
	if m.current.StreamID != streamID || m.jobGeneration != generation || m.captionAudioConnectionGeneration != connectionGeneration || m.stopping {
		m.mu.Unlock()
		return
	}
	m.captionAudioProviderDrops += len(failures)
	m.mu.Unlock()
	diagnosticCtx, cancel := context.WithTimeout(context.Background(), captionDiagnosticTimeout)
	defer cancel()
	attributes := map[string]any{
		"job_generation":        generation,
		"connection_generation": connectionGeneration,
		"packet_count":          len(failures),
		"retry_count":           retryCount,
		"error_class":           captionAudioFailureClass(failures),
		"retryable":             captionAudioFailuresRetryable(failures),
	}
	if httpStatus := captionAudioFailureHTTPStatus(failures); httpStatus != 0 {
		attributes["http_status"] = httpStatus
	}
	m.report(diagnosticCtx, streamID, "worker.caption.audio_dropped", "failed", attributes)
}

func stopCaptionAudioIngress(ctx context.Context, ingress *captionAudioIngress) {
	if ingress == nil {
		return
	}
	ingress.cancel()
	select {
	case <-ingress.done:
	case <-ctx.Done():
	}
}

func validateCaptionAudioPackets(packets []deepgram.AudioPacket, generation, connectionGeneration uint64) (int, error) {
	if len(packets) == 0 {
		return 0, errors.New("at least one opus packet is required")
	}
	retainedBytes := 0
	for _, packet := range packets {
		if len(packet.Opus) == 0 || len(packet.UserID) > captionAudioMaxUserIDBytes {
			return 0, ErrCaptionAudioPayloadInvalid
		}
		if packet.JobGeneration == 0 {
			return 0, ErrCaptionAudioGenerationRequired
		}
		if packet.JobGeneration != generation {
			return 0, ErrJobGenerationMismatch
		}
		if packet.ConnectionGeneration == 0 {
			return 0, ErrCaptionAudioConnectionGenerationRequired
		}
		if packet.ConnectionGeneration != connectionGeneration {
			return 0, ErrCaptionAudioPayloadInvalid
		}
		retainedBytes += len(packet.Opus) + len(packet.UserID) + captionAudioPacketAccountingBytes
		if retainedBytes > captionAudioQueueBytes {
			return 0, ErrCaptionAudioQueueFull
		}
	}
	return retainedBytes, nil
}

func cloneCaptionAudioPackets(packets []deepgram.AudioPacket, generation, connectionGeneration uint64) []deepgram.AudioPacket {
	cloned := make([]deepgram.AudioPacket, len(packets))
	for i, packet := range packets {
		cloned[i] = packet
		cloned[i].JobGeneration = generation
		cloned[i].ConnectionGeneration = connectionGeneration
		cloned[i].Opus = append([]byte(nil), packet.Opus...)
	}
	return cloned
}

func (m *Manager) EnqueueCaptionAudioForGeneration(ctx context.Context, streamID string, generation uint64, packets []deepgram.AudioPacket) error {
	if generation == 0 {
		return ErrCaptionAudioGenerationRequired
	}
	if len(packets) == 0 {
		return ErrCaptionAudioPayloadInvalid
	}
	connectionGeneration := packets[0].ConnectionGeneration
	if connectionGeneration == 0 {
		return ErrCaptionAudioConnectionGenerationRequired
	}
	retainedBytes, err := validateCaptionAudioPackets(packets, generation, connectionGeneration)
	if err != nil {
		return err
	}

	m.mu.Lock()
	if err := m.ensureStreamLocked(streamID); err != nil {
		m.mu.Unlock()
		return err
	}
	if generation != m.jobGeneration {
		m.mu.Unlock()
		return ErrJobGenerationMismatch
	}
	if m.captionSession == nil {
		m.mu.Unlock()
		return ErrCaptionNotConfigured
	}
	ingress := m.captionIngress
	if ingress == nil {
		m.mu.Unlock()
		return ErrCaptionAudioUnavailable
	}
	currentConnectionGeneration := m.captionAudioConnectionGeneration
	if currentConnectionGeneration > connectionGeneration {
		m.mu.Unlock()
		return ErrCaptionAudioConnectionGenerationStale
	}
	var supersededIngress *captionAudioIngress
	var supersededPackets int
	if currentConnectionGeneration == 0 {
		m.captionAudioConnectionGeneration = connectionGeneration
	} else if connectionGeneration > currentConnectionGeneration {
		supersededIngress = ingress
		_, supersededPackets, _ = supersededIngress.snapshot()
		supersededIngress.cancel()
		ingress = newCaptionAudioIngress()
		m.captionIngress = ingress
		m.captionAudioConnectionGeneration = connectionGeneration
		m.captionAudioSupersededDrops += supersededPackets
	}
	currentGeneration := m.jobGeneration
	m.mu.Unlock()
	if supersededIngress != nil {
		go m.runCaptionAudioIngress(ingress)
		go m.reportCaptionAudioSuperseded(streamID, currentGeneration, currentConnectionGeneration, connectionGeneration, supersededPackets)
	}

	cloned := cloneCaptionAudioPackets(packets, generation, connectionGeneration)
	m.mu.Lock()
	if err := m.ensureStreamLocked(streamID); err != nil {
		m.mu.Unlock()
		return err
	}
	if generation != m.jobGeneration {
		m.mu.Unlock()
		return ErrJobGenerationMismatch
	}
	if m.captionSession == nil {
		m.mu.Unlock()
		return ErrCaptionNotConfigured
	}
	if m.captionAudioConnectionGeneration > connectionGeneration {
		m.mu.Unlock()
		return ErrCaptionAudioConnectionGenerationStale
	}
	if m.captionAudioConnectionGeneration != connectionGeneration || m.captionIngress != ingress {
		m.mu.Unlock()
		return ErrCaptionAudioUnavailable
	}
	accepted := ingress.enqueue(captionAudioBatch{
		streamID:             streamID,
		generation:           currentGeneration,
		connectionGeneration: connectionGeneration,
		packets:              cloned,
		bytes:                retainedBytes,
	})
	if accepted {
		m.mu.Unlock()
		return nil
	}
	m.captionAudioQueueDrops++
	queuedBatches, queuedPackets, queuedBytes := ingress.snapshot()
	m.mu.Unlock()
	m.report(ctx, streamID, "worker.caption.audio_queue_full", "failed", map[string]any{
		"job_generation":        currentGeneration,
		"connection_generation": connectionGeneration,
		"packet_count":          len(packets),
		"queued_batches":        queuedBatches,
		"queued_packets":        queuedPackets,
		"queued_bytes":          queuedBytes,
	})
	return ErrCaptionAudioQueueFull
}

func (m *Manager) reportCaptionAudioSuperseded(streamID string, generation, previousConnectionGeneration, connectionGeneration uint64, packetCount int) {
	ctx, cancel := context.WithTimeout(context.Background(), captionDiagnosticTimeout)
	defer cancel()
	m.report(ctx, streamID, "worker.caption.audio_generation_superseded", "superseded", map[string]any{
		"job_generation":                 generation,
		"previous_connection_generation": previousConnectionGeneration,
		"connection_generation":          connectionGeneration,
		"packet_count":                   packetCount,
	})
}

func (m *Manager) IngestCaptionAudio(ctx context.Context, streamID string, packets []deepgram.AudioPacket) error {
	return m.IngestCaptionAudioForGeneration(ctx, streamID, 0, packets)
}

func (m *Manager) IngestCaptionAudioForGeneration(ctx context.Context, streamID string, generation uint64, packets []deepgram.AudioPacket) error {
	failures, _, err := m.ingestCaptionAudioAttempt(ctx, streamID, generation, 0, packets)
	if err != nil {
		return err
	}
	for _, failure := range failures {
		attributes := map[string]any{
			"error_class": failure.errorClass,
			"retryable":   failure.retryable,
		}
		if failure.httpStatus != 0 {
			attributes["http_status"] = failure.httpStatus
		}
		m.report(ctx, streamID, "worker.caption.audio_failed", "failed", attributes)
	}
	if len(failures) > 0 {
		return ErrCaptionAudioUnavailable
	}
	return nil
}

func (m *Manager) ingestCaptionAudioAttempt(ctx context.Context, streamID string, generation, connectionGeneration uint64, packets []deepgram.AudioPacket) ([]captionAudioFailure, int, error) {
	if len(packets) == 0 {
		return nil, 0, errors.New("at least one opus packet is required")
	}
	m.mu.Lock()
	if err := m.ensureStreamLocked(streamID); err != nil {
		m.mu.Unlock()
		return nil, 0, err
	}
	if generation != 0 && generation != m.jobGeneration {
		m.mu.Unlock()
		return nil, 0, ErrJobGenerationMismatch
	}
	if connectionGeneration != 0 && connectionGeneration != m.captionAudioConnectionGeneration {
		m.mu.Unlock()
		return nil, 0, ErrCaptionAudioConnectionGenerationStale
	}
	captionSession := m.captionSession
	currentGeneration := m.jobGeneration
	m.mu.Unlock()
	if captionSession == nil {
		return nil, 0, ErrCaptionNotConfigured
	}
	failures := make([]captionAudioFailure, 0)
	accepted := 0
	for index, packet := range packets {
		if err := ctx.Err(); err != nil {
			return failures, accepted, err
		}
		if len(packet.Opus) == 0 {
			return nil, accepted, ErrCaptionAudioPayloadInvalid
		}
		if err := captionSession.Ingest(ctx, packet); err != nil {
			failure := newCaptionAudioFailure(packet, err)
			failures = append(failures, failure)
			if failure.batchWide {
				for _, remainingPacket := range packets[index+1:] {
					remainingFailure := failure
					remainingFailure.packet = remainingPacket
					failures = append(failures, remainingFailure)
				}
				break
			}
			continue
		}
		accepted++
	}
	if accepted > 0 {
		m.mu.Lock()
		shouldReportAudioStarted := m.current.StreamID == streamID && m.jobGeneration == currentGeneration && !m.stopping && m.captionAudioStartedGeneration != currentGeneration
		if shouldReportAudioStarted {
			m.captionAudioStartedGeneration = currentGeneration
		}
		m.mu.Unlock()
		if shouldReportAudioStarted {
			m.report(ctx, streamID, "worker.caption.audio_started", "accepted", map[string]any{"packet_count": accepted})
		}
	}
	return failures, accepted, nil
}
