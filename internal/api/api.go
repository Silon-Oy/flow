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
	Pool   *pgxpool.Pool
	Leases *lease.Manager
	Runs   *runstate.Store
	Auth   *auth.Service
	GHApp  *githubapp.Broker
	// TenantID is the bootstrap tenant fed to the Vaihe 1 header-stub extractor:
	// requests that omit X-Flow-Tenant-ID fall back to this so the existing
	// single-tenant clients (and tests) keep working. The scanner (system-level,
	// outside the middleware) also reads this directly; Vaihe 2 swaps both.
	TenantID    string
	BrokerToken string // pre-shared bearer for §7.3 token broker (FLOW_BROKER_TOKEN)

	// BranchValidator validates per-remote base_branch existence on POST
	// /v1/projects (§8 wizard). Default is the broker-backed validator; tests
	// inject a stub so they don't need a live GitHub.
	BranchValidator BranchValidator

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
// BrokerToken is the shared bearer that gates /v1/github-app/token. Issue #6
// landed per-runner tokens on the runner-write endpoints, but this broker
// endpoint still rides the shared bearer (folding it onto runner tokens is a
// follow-up); empty means the endpoint refuses every call (fail-closed — the central
// never mints App tokens for unauthenticated callers, even on a private
// network).
func New(pool *pgxpool.Pool, tenantID string) *Server {
	broker := githubapp.NewBroker(pool, secrets.EnvResolver{})
	return &Server{
		Pool:            pool,
		Leases:          lease.NewManager(pool),
		Runs:            runstate.New(pool),
		Auth:            auth.New(pool, tenantID, os.Getenv("FLOW_GITHUB_OAUTH_CLIENT_ID")),
		GHApp:           broker,
		TenantID:        tenantID,
		BrokerToken:     os.Getenv("FLOW_BROKER_TOKEN"),
		BranchValidator: NewBranchValidator(broker),
		hub:             newLogHub(),
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

	// Two tenant-scoped route groups, split by the credential that pins the
	// tenant (§7(b)):
	//
	//   - runnerWrite: the machine path. The per-runner token minted at register
	//     authenticates the request; runnerTokenExtractor resolves its tenant.
	//     Wiring the extractor here and NOWHERE else is what scopes the runner
	//     token to runner endpoints only — it grants no read/dashboard access.
	//   - scoped (read/dashboard): the human path. Still the Vaihe 1 header stub;
	//     user-session enforcement is issue #7. A runner token presented here is
	//     simply not honored as a credential (the header extractor ignores it).
	runnerAuth := auth.WithTenant(s.runnerTokenExtractor)
	runnerWrite := func(method, pattern string, h http.HandlerFunc) {
		mux.Handle(method+" "+pattern, runnerAuth(h))
	}
	tenant := auth.WithTenant(auth.HeaderExtractor(s.TenantID))
	scoped := func(method, pattern string, h http.HandlerFunc) {
		mux.Handle(method+" "+pattern, tenant(h))
	}

	// RBAC-scoped routes (§7): tenant → RequireAuth → RequireRole → handler.
	// The lookup resolves session tokens to principals; an unauthenticated
	// request lands on 401 before it can probe the capability.
	lookup := auth.PrincipalLookupFromPool(s.Pool)
	rbacScoped := func(method, pattern string, capability auth.Capability, h http.HandlerFunc) {
		chain := auth.RequireRole(capability)(h)
		chain = auth.RequireAuth(lookup)(chain)
		mux.Handle(method+" "+pattern, tenant(chain))
	}
	// authedScoped is the "any authenticated user" variant: tenant → RequireAuth
	// → handler. No capability gate — used for /v1/me where every authenticated
	// caller is entitled to read their own identity.
	authedScoped := func(method, pattern string, h http.HandlerFunc) {
		chain := auth.RequireAuth(lookup)(h)
		mux.Handle(method+" "+pattern, tenant(chain))
	}

	// Runner-write endpoints (§7(b) runner-token scope): machine identity,
	// lease lifecycle, run telemetry. Each REQUIRES a valid runner token.
	runnerWrite("POST", "/v1/runners/{id}/heartbeat", s.handleRunnerHeartbeat)
	runnerWrite("POST", "/v1/leases/acquire", s.handleLeaseAcquire)
	runnerWrite("POST", "/v1/leases/{id}/heartbeat", s.handleLeaseHeartbeat)
	runnerWrite("POST", "/v1/leases/{id}/release", s.handleLeaseRelease)
	runnerWrite("POST", "/v1/runs", s.handleRunCreate)
	runnerWrite("PATCH", "/v1/runs/{id}", s.handleRunPatch)
	runnerWrite("POST", "/v1/runs/{id}/events", s.handleRunEvents)

	// Read/dashboard endpoints — RBAC (§7). The runner token is NOT a credential
	// here; RequireAuth resolves the user session and RequireRole gates the
	// capability.
	// §7 row "Rekisteröi projekti (wizard)" — `flowctl init`. Both admin and
	// developer hold CapProjectRegister; the central validates §8 before
	// inserting (acceptance criterion: validation lives in the central, not
	// just in the CLI).
	rbacScoped("POST", "/v1/projects", auth.CapProjectRegister, s.handleProjectCreate)
	// §7 row "Hallitsee jaettuja runnereita" — admin-only list.
	rbacScoped("GET", "/v1/runners", auth.CapRunnersManageShared, s.handleRunnersList)
	// §7 rows "Näkee omat ajot" / "Näkee koko tenantin ajot" — capability
	// gates the endpoint; the handler then filters by principal.UserID for
	// developers (admins see the whole tenant).
	rbacScoped("GET", "/v1/runs", auth.CapRunsViewOwn, s.handleRunsList)
	scoped("GET", "/v1/runs/{id}", s.handleRunGet)
	scoped("GET", "/v1/runs/{id}/logs", s.handleRunLogs) // SSE

	// Egress log read (dashboard). POST is system-level (see above), GET is
	// tenant-scoped so dashboards never read across tenants.
	scoped("GET", "/v1/egress", s.handleEgressList)

	// /v1/me — every authenticated user reads their own identity (no capability
	// gate). The dashboard polls this on load to branch on role/capabilities and
	// to render the signed-in user's name in the header.
	authedScoped("GET", "/v1/me", s.handleMe)

	// Human auth: §7(a) GitHub OAuth device flow. Unauthenticated by design —
	// they are the bootstrap path that produces the session token everything
	// else will require in Vaihe 2.
	mux.HandleFunc("POST /v1/auth/device/start", s.handleDeviceStart)
	mux.HandleFunc("POST /v1/auth/device/poll", s.handleDevicePoll)

	// §7.3 GitHub App token broker. Still gated by the FLOW_BROKER_TOKEN shared
	// secret; #6 added per-runner tokens elsewhere but this endpoint's switch to
	// them is a follow-up.
	mux.HandleFunc("GET /v1/github-app/token", s.handleGitHubAppToken)

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
