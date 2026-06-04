// Package api is the flowd HTTP surface (§6): REST for runner/lease/run
// operations plus an SSE log stream. Vaihe 1 is single-tenant in data, so the
// server resolves a bootstrap tenant; the tenant-isolation middleware lands in
// Vaihe 2.
//
// No SSE library: the log stream uses http.Flusher directly (§6).
package api

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Silon-Oy/flow/internal/auth"
	"github.com/Silon-Oy/flow/internal/githubapp"
	"github.com/Silon-Oy/flow/internal/lease"
	"github.com/Silon-Oy/flow/internal/runstate"
	"github.com/Silon-Oy/flow/internal/secrets"
)

// Server holds the dependencies the handlers share.
type Server struct {
	Pool        *pgxpool.Pool
	Leases      *lease.Manager
	Runs        *runstate.Store
	Auth        *auth.Service
	GHApp       *githubapp.Broker
	TenantID    string // bootstrap tenant (single-tenant Vaihe 1)
	BrokerToken string // pre-shared bearer for §7.3 token broker (FLOW_BROKER_TOKEN)

	// hub fans out run events to SSE subscribers.
	hub *logHub
}

// New builds a Server over the given pool with a resolved bootstrap tenant. The
// GitHub OAuth client_id is read from FLOW_GITHUB_OAUTH_CLIENT_ID; when empty
// the device-flow endpoints return 503 (the rest of the API still works).
//
// The GitHub App broker is wired with the env-backed secrets resolver: Vaihe 1
// reads `private_key_ref` as an env var name, issue #10 swaps it for a
// pgcrypto resolver behind the same interface.
//
// BrokerToken is the shared bearer that gates /v1/github-app/token. It's a
// stop-gap until issue #6 lands per-runner tokens stored in the runner table;
// empty means the endpoint refuses every call (fail-closed — the central
// never mints App tokens for unauthenticated callers, even on a private
// network).
func New(pool *pgxpool.Pool, tenantID string) *Server {
	return &Server{
		Pool:        pool,
		Leases:      lease.NewManager(pool),
		Runs:        runstate.New(pool),
		Auth:        auth.New(pool, tenantID, os.Getenv("FLOW_GITHUB_OAUTH_CLIENT_ID")),
		GHApp:       githubapp.NewBroker(pool, secrets.EnvResolver{}),
		TenantID:    tenantID,
		BrokerToken: os.Getenv("FLOW_BROKER_TOKEN"),
		hub:         newLogHub(),
	}
}

// Routes returns the configured mux. Go 1.22+ method-prefixed patterns keep the
// routing table declarative.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", s.handleHealthz)

	// Runner <-> central (machine identity).
	mux.HandleFunc("POST /v1/runners/register", s.handleRunnerRegister)
	mux.HandleFunc("POST /v1/runners/{id}/heartbeat", s.handleRunnerHeartbeat)
	mux.HandleFunc("GET /v1/runners", s.handleRunnersList)

	// Lease lifecycle.
	mux.HandleFunc("POST /v1/leases/acquire", s.handleLeaseAcquire)
	mux.HandleFunc("POST /v1/leases/{id}/heartbeat", s.handleLeaseHeartbeat)
	mux.HandleFunc("POST /v1/leases/{id}/release", s.handleLeaseRelease)

	// Run telemetry.
	mux.HandleFunc("POST /v1/runs", s.handleRunCreate)
	mux.HandleFunc("PATCH /v1/runs/{id}", s.handleRunPatch)
	mux.HandleFunc("POST /v1/runs/{id}/events", s.handleRunEvents)
	mux.HandleFunc("GET /v1/runs", s.handleRunsList)
	mux.HandleFunc("GET /v1/runs/{id}", s.handleRunGet)
	mux.HandleFunc("GET /v1/runs/{id}/logs", s.handleRunLogs) // SSE

	// Egress log: dashboard read + shipper ingest (§11.6).
	mux.HandleFunc("GET /v1/egress", s.handleEgressList)
	mux.HandleFunc("POST /v1/egress", s.handleEgressIngest)

	// Human auth: §7(a) GitHub OAuth device flow. Unauthenticated by design —
	// they are the bootstrap path that produces the session token everything
	// else will require in Vaihe 2.
	mux.HandleFunc("POST /v1/auth/device/start", s.handleDeviceStart)
	mux.HandleFunc("POST /v1/auth/device/poll", s.handleDevicePoll)

	// §7.3 GitHub App token broker. Gated by FLOW_BROKER_TOKEN shared secret
	// until issue #6 lands per-runner tokens in the runner table.
	mux.HandleFunc("GET /v1/github-app/token", s.handleGitHubAppToken)

	return logRequests(mux)
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// --- helpers ---------------------------------------------------------------

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
