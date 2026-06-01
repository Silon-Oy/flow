package orchestrator

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/Silon-Oy/flow/internal/runstate"
)

// telemetrySink is the subset of the central client the Reporter needs. Defined
// here (consumer-side) so internal/orchestrator does not import the HTTP client
// directly — keeping the dependency direction runner -> orchestrator -> sink.
type telemetrySink interface {
	PatchRun(ctx context.Context, runID string, patch map[string]any) error
	AppendEvents(ctx context.Context, runID string, events []runstate.Event) error
}

// HTTPReporter pushes telemetry to the central service, batching events (§6:
// 5s / 20 events) and buffering on the runner so a central outage does not lose
// telemetry — buffered events flush on the next successful push (§5 degradation:
// an in-flight run continues; telemetry buffers).
type HTTPReporter struct {
	sink  telemetrySink
	runID string

	mu       sync.Mutex
	buf      []runstate.Event
	maxBatch int
}

// NewHTTPReporter builds a reporter for a run.
func NewHTTPReporter(sink telemetrySink, runID string) *HTTPReporter {
	return &HTTPReporter{sink: sink, runID: runID, maxBatch: 20}
}

// SetState records the current step.
func (r *HTTPReporter) SetState(ctx context.Context, step Step) error {
	return r.sink.PatchRun(ctx, r.runID, map[string]any{"current_state": string(step)})
}

// Event buffers an event and flushes when the batch is full. The flush is
// best-effort: a failed flush keeps the events buffered for the next attempt.
func (r *HTTPReporter) Event(ctx context.Context, event string, data map[string]string) error {
	var raw json.RawMessage
	if len(data) > 0 {
		b, _ := json.Marshal(data)
		raw = b
	}
	r.mu.Lock()
	r.buf = append(r.buf, runstate.Event{Event: event, Data: raw, TS: time.Now().UTC()})
	full := len(r.buf) >= r.maxBatch
	r.mu.Unlock()
	if full {
		return r.Flush(ctx)
	}
	return nil
}

// Flush sends buffered events. On success the buffer is cleared; on failure the
// events stay buffered (telemetry is never dropped on a central blip).
func (r *HTTPReporter) Flush(ctx context.Context) error {
	r.mu.Lock()
	if len(r.buf) == 0 {
		r.mu.Unlock()
		return nil
	}
	batch := make([]runstate.Event, len(r.buf))
	copy(batch, r.buf)
	r.mu.Unlock()

	if err := r.sink.AppendEvents(ctx, r.runID, batch); err != nil {
		return err // keep buffered
	}
	r.mu.Lock()
	r.buf = r.buf[len(batch):]
	r.mu.Unlock()
	return nil
}

// Finalize flushes remaining events, then patches the terminal status.
func (r *HTTPReporter) Finalize(ctx context.Context, status, reason string) error {
	_ = r.Flush(ctx)
	patch := map[string]any{"status": status, "finished": true}
	if reason != "" {
		patch["blocked_reason"] = reason
	}
	return r.sink.PatchRun(ctx, r.runID, patch)
}
