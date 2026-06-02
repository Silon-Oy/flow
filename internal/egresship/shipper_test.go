package egresship

import (
	"context"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type fakeSink struct {
	mu      sync.Mutex
	batches [][]Entry
}

func (f *fakeSink) ShipEgress(ctx context.Context, entries []Entry) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := make([]Entry, len(entries))
	copy(cp, entries)
	f.batches = append(f.batches, cp)
	return nil
}

func (f *fakeSink) all() []Entry {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []Entry
	for _, b := range f.batches {
		out = append(out, b...)
	}
	return out
}

func waitFor(t *testing.T, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s", d)
}

// TestRunTailsAndShips covers the common path: shipper starts before squid has
// written anything, then sees a few lines appended, batches them, and continues
// after a partial line is completed across two writes.
func TestRunTailsAndShips(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "access.log")
	// Pre-create the file so the shipper opens it immediately; the seek-to-end
	// invariant means historical content is skipped.
	if err := os.WriteFile(path, []byte("1700000000.000 1 10.0.0.1 TCP_TUNNEL/200 1 CONNECT skipped.example.com:443 - - -\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	sink := &fakeSink{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		_ = Run(ctx, Config{
			Path:          path,
			Sink:          sink,
			BatchSize:     2,
			FlushInterval: 200 * time.Millisecond,
			PollInterval:  20 * time.Millisecond,
			Logger:        log.New(io.Discard, "", 0),
		})
		close(done)
	}()

	// Give the shipper a tick to open + seek-to-end.
	time.Sleep(80 * time.Millisecond)

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	_, _ = f.WriteString("1717250000.123 1 10.0.0.5 TCP_TUNNEL/200 1 CONNECT github.com:443 - - -\n")
	_, _ = f.WriteString("1717250001.456 1 10.0.0.5 TCP_DENIED/403 1 CONNECT evil.example.com:443 - - -\n")

	// Partial line, then completion: shipper must rewind and finish the line.
	_, _ = f.WriteString("1717250002.000 1 10.0.0.5 TCP_MISS/200 1 GET http://registry.npmjs.")
	time.Sleep(80 * time.Millisecond)
	_, _ = f.WriteString("org/x - - -\n")

	waitFor(t, 3*time.Second, func() bool { return len(sink.all()) >= 3 })

	cancel()
	<-done

	got := sink.all()
	wantHosts := []string{"github.com", "evil.example.com", "registry.npmjs.org"}
	if len(got) != len(wantHosts) {
		t.Fatalf("entries = %d, want %d (%+v)", len(got), len(wantHosts), got)
	}
	for i, want := range wantHosts {
		if got[i].Host != want {
			t.Errorf("entry %d host = %q, want %q", i, got[i].Host, want)
		}
	}
	if got[1].Allowed {
		t.Errorf("evil.example.com should be denied")
	}
}

// TestRunHandlesRotation covers copytruncate (file shrinks in place) — the
// shipper must reopen from offset 0 and pick up the post-rotate lines.
func TestRunHandlesRotation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "access.log")
	if err := os.WriteFile(path, []byte{}, 0o644); err != nil {
		t.Fatal(err)
	}

	sink := &fakeSink{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		_ = Run(ctx, Config{
			Path:          path,
			Sink:          sink,
			BatchSize:     1,
			FlushInterval: 100 * time.Millisecond,
			PollInterval:  20 * time.Millisecond,
			Logger:        log.New(io.Discard, "", 0),
		})
		close(done)
	}()

	time.Sleep(80 * time.Millisecond)

	// Pre-rotation line.
	if err := os.WriteFile(path, []byte("1717250010.000 1 10.0.0.5 TCP_TUNNEL/200 1 CONNECT before.example.com:443 - - -\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 2*time.Second, func() bool { return len(sink.all()) >= 1 })

	// Simulate copytruncate: truncate to 0, then write a new line.
	if err := os.Truncate(path, 0); err != nil {
		t.Fatal(err)
	}
	// Give the shipper a poll cycle to notice the shrink.
	time.Sleep(120 * time.Millisecond)
	if err := os.WriteFile(path, []byte("1717250020.000 1 10.0.0.5 TCP_TUNNEL/200 1 CONNECT after.example.com:443 - - -\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	waitFor(t, 2*time.Second, func() bool { return len(sink.all()) >= 2 })

	cancel()
	<-done

	all := sink.all()
	if all[0].Host != "before.example.com" || all[1].Host != "after.example.com" {
		t.Errorf("hosts = %q + %q, want before/after", all[0].Host, all[1].Host)
	}
}
