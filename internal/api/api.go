// Package api is the flowd HTTP surface (§6): REST for runner/lease/run
// operations plus an SSE log stream. The tenant-isolation seam (§7, invariant
// #4) lives here: tenant-scoped routes go through auth.WithTenant before
// reaching a handler, so handlers read the tenant from request context and
// every DB call is tenant-filtered.
//
// No SSE library: the log stream uses http.Flusher directly (§6).
package api

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Silon-Oy/flow/internal/auth"
	"github.com/Silon-Oy/flow/internal/lease"
	"github.com/Silon-Oy/flow/internal/runstate"
)

// Server holds the dependencies the handlers share.
type Server struct {
	Pool   *pgxpool.Pool
	Leases *lease.Manager
	Runs   *runstate.Store
	// TenantID is the bootstrap tenant fed to the Vaihe 1 header-stub extractor:
	// requests that omit X-Flow-Tenant-ID fall back to this so the existing
	// single-tenant clients (and tests) keep working. The scanner (system-level,
	// outside the middleware) also reads this directly; Vaihe 2 swaps both.
	TenantID string

	// hub fans out run events to SSE subscribers.
	hub *logHub
}

// New builds a Server over the given pool with a resolved bootstrap tenant.
func New(pool *pgxpool.Pool, tenantID string) *Server {
	return &Server{
		Pool:     pool,
		Leases:   lease.NewManager(pool),
		Runs:     runstate.New(pool),
		TenantID: tenantID,
		hub:      newLogHub(),
	}
}

// Routes returns the configured mux. Go 1.22+ method-prefixed patterns keep the
// routing table declarative.
//
// Two route groups (§7 / invariant #4):
//   - System routes: /healthz, POST /v1/runners/register, POST /v1/egress.
//     These run BEFORE a tenant credential is meaningful (pre-auth / runner
//     shipper ingest) and are stamped with the bootstrap tenant directly.
//   - Tenant-scoped routes: everything else. WithTenant pins the resolved
//     tenant into the request context; handlers read it via tenantFromCtx.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()

	// System routes — no tenant middleware (see invariant #4 commentary).
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("POST /v1/runners/register", s.handleRunnerRegister)
	// POST /v1/egress is the runner-shipper ingest; the shipper has no user
	// credential, so the ingest stamps the bootstrap tenant. Vaihe 2 swaps this
	// for a runner-token-derived tenant.
	mux.HandleFunc("POST /v1/egress", s.handleEgressIngest)

	// Tenant-scoped routes — wrapped per-route so a future runner-token
	// extractor can be plugged in without touching the routing table.
	tenant := auth.WithTenant(auth.HeaderExtractor(s.TenantID))
	scoped := func(method, pattern string, h http.HandlerFunc) {
		mux.Handle(method+" "+pattern, tenant(h))
	}

	// Runner <-> central (machine identity).
	scoped("POST", "/v1/runners/{id}/heartbeat", s.handleRunnerHeartbeat)
	scoped("GET", "/v1/runners", s.handleRunnersList)

	// Lease lifecycle.
	scoped("POST", "/v1/leases/acquire", s.handleLeaseAcquire)
	scoped("POST", "/v1/leases/{id}/heartbeat", s.handleLeaseHeartbeat)
	scoped("POST", "/v1/leases/{id}/release", s.handleLeaseRelease)

	// Run telemetry.
	scoped("POST", "/v1/runs", s.handleRunCreate)
	scoped("PATCH", "/v1/runs/{id}", s.handleRunPatch)
	scoped("POST", "/v1/runs/{id}/events", s.handleRunEvents)
	scoped("GET", "/v1/runs", s.handleRunsList)
	scoped("GET", "/v1/runs/{id}", s.handleRunGet)
	scoped("GET", "/v1/runs/{id}/logs", s.handleRunLogs) // SSE

	// Egress log read (dashboard). POST is system-level (see above), GET is
	// tenant-scoped so dashboards never read across tenants.
	scoped("GET", "/v1/egress", s.handleEgressList)

	return logRequests(mux)
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// --- helpers ---------------------------------------------------------------

// tenantFromCtx returns the tenant pinned by WithTenant. Handlers wired into
// the tenant-scoped route group are guaranteed to find it; system handlers
// must not call this.
func tenantFromCtx(ctx context.Context) string {
	tid, _ := auth.TenantFromContext(ctx)
	return tid
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if v != nil {
		_ = json.NewEncoder(w).Encode(v)
	}
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func decodeJSON(r *http.Request, v any) error {
	defer r.Body.Close()
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start).Round(time.Millisecond))
	})
}

// withTimeout derives a request-scoped context bounded by d.
func withTimeout(r *http.Request, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), d)
}

var errNotFound = errors.New("not found")
