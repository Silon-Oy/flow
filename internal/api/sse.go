package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/Silon-Oy/flow/internal/runstate"
)

// logHub fans out run events to SSE subscribers, keyed by run id. It holds no
// history — a subscriber first gets the persisted backlog (read from the DB by
// the handler), then live events published here. This keeps the hub O(active
// subscribers) in memory rather than buffering every run's full log.
type logHub struct {
	mu   sync.Mutex
	subs map[string]map[chan runstate.Event]struct{}
}

func newLogHub() *logHub {
	return &logHub{subs: make(map[string]map[chan runstate.Event]struct{})}
}

func (h *logHub) subscribe(runID string) chan runstate.Event {
	ch := make(chan runstate.Event, 64)
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.subs[runID] == nil {
		h.subs[runID] = make(map[chan runstate.Event]struct{})
	}
	h.subs[runID][ch] = struct{}{}
	return ch
}

func (h *logHub) unsubscribe(runID string, ch chan runstate.Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if subs := h.subs[runID]; subs != nil {
		delete(subs, ch)
		if len(subs) == 0 {
			delete(h.subs, runID)
		}
	}
	close(ch)
}

func (h *logHub) publish(runID string, events []runstate.Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs[runID] {
		for _, e := range events {
			select {
			case ch <- e:
			default:
				// Slow subscriber: drop rather than block the telemetry path.
				// The dashboard can re-fetch the backlog to recover.
			}
		}
	}
}

// handleRunLogs streams a run's events as SSE: first the persisted backlog, then
// live events. Uses http.Flusher directly (no SSE library, per §6).
func (s *Server) handleRunLogs(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("id")
	tenantID := tenantFromCtx(r.Context())

	// Verify the run belongs to the caller's tenant BEFORE opening the stream:
	// otherwise a cross-tenant runID would yield an empty backlog and an open
	// SSE channel. Returning 404 here keeps existence from leaking and prevents
	// a tenant from latching onto another tenant's runID via the hub.
	if _, err := s.Runs.GetRun(r.Context(), tenantID, runID); err != nil {
		writeErr(w, http.StatusNotFound, "unknown run")
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	// Subscribe BEFORE reading the backlog so no event slips through the gap
	// between backlog read and live subscription.
	ch := s.hub.subscribe(runID)
	defer s.hub.unsubscribe(runID, ch)

	backlog, err := s.Runs.ListEvents(r.Context(), tenantID, runID)
	if err == nil {
		for _, e := range backlog {
			writeSSE(w, e)
		}
		flusher.Flush()
	}

	keepalive := time.NewTicker(15 * time.Second)
	defer keepalive.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case e := <-ch:
			writeSSE(w, e)
			flusher.Flush()
		case <-keepalive.C:
			// SSE comment line keeps the connection alive through proxies.
			fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		}
	}
}

func writeSSE(w http.ResponseWriter, e runstate.Event) {
	payload, err := json.Marshal(e)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", e.Event, payload)
}
