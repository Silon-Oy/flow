package orchestrator

import (
	"context"
	"errors"
	"testing"

	"github.com/Silon-Oy/flow/internal/runstate"
)

type fakeSink struct {
	patches    []map[string]any
	events     []runstate.Event
	failAppend bool
}

func (f *fakeSink) PatchRun(_ context.Context, _ string, patch map[string]any) error {
	f.patches = append(f.patches, patch)
	return nil
}
func (f *fakeSink) AppendEvents(_ context.Context, _ string, events []runstate.Event) error {
	if f.failAppend {
		return errors.New("central down")
	}
	f.events = append(f.events, events...)
	return nil
}

// TestReporterBuffersOnFailure: a failed flush retains events so the next
// successful flush delivers them — the §5 telemetry-buffering invariant.
func TestReporterBuffersOnFailure(t *testing.T) {
	sink := &fakeSink{failAppend: true}
	r := NewHTTPReporter(sink, "run-1")
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		_ = r.Event(ctx, "evt", map[string]string{"i": "x"})
	}
	// Central is down: flush fails, events stay buffered.
	if err := r.Flush(ctx); err == nil {
		t.Errorf("flush should fail while central is down")
	}
	if len(sink.events) != 0 {
		t.Errorf("no events should have landed while down, got %d", len(sink.events))
	}

	// Central recovers: the buffered events flush.
	sink.failAppend = false
	if err := r.Flush(ctx); err != nil {
		t.Fatalf("flush after recovery: %v", err)
	}
	if len(sink.events) != 3 {
		t.Errorf("recovered events = %d, want 3", len(sink.events))
	}
}

// TestReporterBatchAtCap: reaching the batch cap auto-flushes.
func TestReporterBatchAtCap(t *testing.T) {
	sink := &fakeSink{}
	r := NewHTTPReporter(sink, "run-2")
	r.maxBatch = 5
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		_ = r.Event(ctx, "evt", nil)
	}
	if len(sink.events) != 5 {
		t.Errorf("auto-flush at cap = %d events, want 5", len(sink.events))
	}
}

// TestReporterFinalize: flushes then patches terminal status.
func TestReporterFinalize(t *testing.T) {
	sink := &fakeSink{}
	r := NewHTTPReporter(sink, "run-3")
	ctx := context.Background()
	_ = r.Event(ctx, "evt", nil)
	if err := r.Finalize(ctx, "completed", ""); err != nil {
		t.Fatal(err)
	}
	if len(sink.events) != 1 {
		t.Errorf("finalize should flush buffered events")
	}
	last := sink.patches[len(sink.patches)-1]
	if last["status"] != "completed" || last["finished"] != true {
		t.Errorf("finalize patch = %v", last)
	}
}
