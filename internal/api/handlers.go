package api

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/Silon-Oy/flow/internal/auth"
	"github.com/Silon-Oy/flow/internal/lease"
	"github.com/Silon-Oy/flow/internal/runstate"
)

// --- runners ---------------------------------------------------------------

type runnerRegisterReq struct {
	Hostname     string         `json:"hostname"`
	Capacity     int            `json:"capacity"`
	Capabilities map[string]any `json:"capabilities,omitempty"`
}

type runnerRegisterResp struct {
	RunnerID    string `json:"runner_id"`
	RunnerToken string `json:"runner_token"`
}

// handleRunnerRegister is a SYSTEM route (no tenant middleware): a fresh
// runner has no credential yet, so the bootstrap tenant on the Server is
// stamped onto the new row. Vaihe 2 replaces this with the OAuth-issued
// tenant of the registering user.
//
// §7(b): the central mints a long-lived runner token here and stores only its
// SHA-256 hash (runner.token_hash). The raw token is returned exactly once in
// the response; afterwards the runner-token middleware authenticates by hashing
// the presented bearer and matching this column. The token is scoped to
// runner-write endpoints — it confers no CLI/dashboard access (see Routes()).
func (s *Server) handleRunnerRegister(w http.ResponseWriter, r *http.Request) {
	var req runnerRegisterReq
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	if req.Hostname == "" {
		writeErr(w, http.StatusBadRequest, "hostname required")
		return
	}
	if req.Capacity <= 0 {
		req.Capacity = 1
	}
	ctx, cancel := withTimeout(r, 5*time.Second)
	defer cancel()

	// Mint the runner token and persist only its hash (§7(b)). The raw token
	// leaves the central exactly once, in the response below.
	token := randomToken()
	tokenHash := auth.HashToken(token)
	var runnerID string
	err := s.Pool.QueryRow(ctx, `
		INSERT INTO runner (tenant_id, hostname, capacity, status, last_heartbeat, capabilities, token_hash)
		VALUES ($1, $2, $3, 'online', now(), COALESCE($4, '{}'::jsonb), $5)
		RETURNING id::text`,
		s.TenantID, req.Hostname, req.Capacity, capsJSON(req.Capabilities), tokenHash).Scan(&runnerID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "register: "+err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, runnerRegisterResp{RunnerID: runnerID, RunnerToken: token})
}

func (s *Server) handleRunnerHeartbeat(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	tenantID := tenantFromCtx(r.Context())
	ctx, cancel := withTimeout(r, 5*time.Second)
	defer cancel()
	tag, err := s.Pool.Exec(ctx,
		`UPDATE runner SET last_heartbeat = now(), status = 'online'
		  WHERE id = $1 AND tenant_id = $2`, id, tenantID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if tag.RowsAffected() == 0 {
		// 404 even if the runner exists under another tenant — existence does
		// not leak across the §7 boundary.
		writeErr(w, http.StatusNotFound, "unknown runner")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleRunnersList(w http.ResponseWriter, r *http.Request) {
	tenantID := tenantFromCtx(r.Context())
	ctx, cancel := withTimeout(r, 5*time.Second)
	defer cancel()
	rows, err := s.Pool.Query(ctx, `
		SELECT id::text, hostname, capacity, active_leases, last_heartbeat, status
		  FROM runner WHERE tenant_id = $1 ORDER BY hostname`, tenantID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()
	type runnerView struct {
		ID            string     `json:"id"`
		Hostname      string     `json:"hostname"`
		Capacity      int        `json:"capacity"`
		ActiveLeases  int        `json:"active_leases"`
		LastHeartbeat *time.Time `json:"last_heartbeat,omitempty"`
		Status        string     `json:"status"`
	}
	var out []runnerView
	for rows.Next() {
		var v runnerView
		if err := rows.Scan(&v.ID, &v.Hostname, &v.Capacity, &v.ActiveLeases, &v.LastHeartbeat, &v.Status); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, map[string]any{"runners": out})
}

// --- leases ----------------------------------------------------------------

type leaseAcquireReq struct {
	RunnerID string   `json:"runner_id"`
	Kinds    []string `json:"kinds"`
}

type leaseAcquireResp struct {
	Lease *lease.Lease `json:"lease"`
	Work  *lease.Work  `json:"work"`
}

func (s *Server) handleLeaseAcquire(w http.ResponseWriter, r *http.Request) {
	var req leaseAcquireReq
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	if req.RunnerID == "" {
		writeErr(w, http.StatusBadRequest, "runner_id required")
		return
	}
	if len(req.Kinds) == 0 {
		req.Kinds = []string{"develop"}
	}
	tenantID := tenantFromCtx(r.Context())
	ctx, cancel := withTimeout(r, 10*time.Second)
	defer cancel()

	l, work, err := s.Leases.Acquire(ctx, tenantID, req.RunnerID, req.Kinds)
	if errors.Is(err, lease.ErrNoWork) {
		// 204: nothing to do. The runner backs off — NOT an error (fail-closed
		// applies to DB unavailability, not to an empty queue).
		writeJSON(w, http.StatusNoContent, nil)
		return
	}
	if err != nil {
		// DB error => fail-closed: surface it, do not hand out work.
		writeErr(w, http.StatusServiceUnavailable, "lease acquire failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, leaseAcquireResp{Lease: l, Work: work})
}

func (s *Server) handleLeaseHeartbeat(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	tenantID := tenantFromCtx(r.Context())
	ctx, cancel := withTimeout(r, 5*time.Second)
	defer cancel()
	ok, err := s.Leases.Heartbeat(ctx, tenantID, id)
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	if !ok {
		// Lease expired, reaped, or owned by another tenant — the runner must
		// stop work either way. 409 keeps the existing wire contract.
		writeErr(w, http.StatusConflict, "lease no longer active")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleLeaseRelease(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	tenantID := tenantFromCtx(r.Context())
	ctx, cancel := withTimeout(r, 5*time.Second)
	defer cancel()
	if err := s.Leases.Release(ctx, tenantID, id); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "released"})
}

// --- runs ------------------------------------------------------------------

type runCreateReq struct {
	ProjectID   string `json:"project_id"`
	Remote      string `json:"remote"`
	IssueNumber int    `json:"issue_number"`
}

func (s *Server) handleRunCreate(w http.ResponseWriter, r *http.Request) {
	var req runCreateReq
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	if req.ProjectID == "" {
		writeErr(w, http.StatusBadRequest, "project_id required")
		return
	}
	if req.Remote == "" {
		req.Remote = "origin"
	}
	tenantID := tenantFromCtx(r.Context())
	ctx, cancel := withTimeout(r, 5*time.Second)
	defer cancel()
	id, err := s.Runs.CreateRun(ctx, tenantID, req.ProjectID, req.Remote, req.IssueNumber)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"run_id": id})
}

// runPatchReq mirrors runstate.Patch on the wire; pointers => "leave unchanged".
type runPatchReq struct {
	Status             *runstate.Status `json:"status,omitempty"`
	CurrentState       *string          `json:"current_state,omitempty"`
	Branch             *string          `json:"branch,omitempty"`
	PRURL              *string          `json:"pr_url,omitempty"`
	BlockedReason      *string          `json:"blocked_reason,omitempty"`
	RetryCount         *int             `json:"retry_count,omitempty"`
	TimeoutPhase       *string          `json:"timeout_phase,omitempty"`
	ClarificationRound *int             `json:"clarification_round,omitempty"`
	RunnerID           *string          `json:"runner_id,omitempty"`
	LeaseID            *string          `json:"lease_id,omitempty"`
	Finished           bool             `json:"finished,omitempty"`
}

func (s *Server) handleRunPatch(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req runPatchReq
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	tenantID := tenantFromCtx(r.Context())
	ctx, cancel := withTimeout(r, 5*time.Second)
	defer cancel()
	err := s.Runs.PatchRun(ctx, tenantID, id, runstate.Patch{
		Status: req.Status, CurrentState: req.CurrentState, Branch: req.Branch,
		PRURL: req.PRURL, BlockedReason: req.BlockedReason, RetryCount: req.RetryCount,
		TimeoutPhase: req.TimeoutPhase, ClarificationRound: req.ClarificationRound,
		RunnerID: req.RunnerID, LeaseID: req.LeaseID, Finished: req.Finished,
	})
	if errors.Is(err, runstate.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "unknown run")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

type runEventsReq struct {
	Events []runstate.Event `json:"events"`
}

func (s *Server) handleRunEvents(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req runEventsReq
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	tenantID := tenantFromCtx(r.Context())
	ctx, cancel := withTimeout(r, 5*time.Second)
	defer cancel()
	if err := s.Runs.AppendEvents(ctx, tenantID, id, req.Events); err != nil {
		if errors.Is(err, runstate.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "unknown run")
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Fan out to any SSE subscribers for live tail.
	s.hub.publish(id, req.Events)
	writeJSON(w, http.StatusAccepted, map[string]int{"accepted": len(req.Events)})
}

func (s *Server) handleRunGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	tenantID := tenantFromCtx(r.Context())
	ctx, cancel := withTimeout(r, 5*time.Second)
	defer cancel()
	run, err := s.Runs.GetRun(ctx, tenantID, id)
	if errors.Is(err, runstate.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "unknown run")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, run)
}

func (s *Server) handleRunsList(w http.ResponseWriter, r *http.Request) {
	tenantID := tenantFromCtx(r.Context())
	ctx, cancel := withTimeout(r, 5*time.Second)
	defer cancel()
	statusFilter := r.URL.Query().Get("status")
	rows, err := s.Pool.Query(ctx, `
		SELECT id::text, project_id::text, remote, issue_number, status,
		       current_state, branch, pr_url, started_at, finished_at
		  FROM run
		 WHERE tenant_id = $1
		   AND ($2 = '' OR status::text = $2)
		 ORDER BY started_at DESC
		 LIMIT 200`, tenantID, statusFilter)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()
	type runView struct {
		ID           string     `json:"id"`
		ProjectID    string     `json:"project_id"`
		Remote       string     `json:"remote"`
		IssueNumber  int        `json:"issue_number"`
		Status       string     `json:"status"`
		CurrentState *string    `json:"current_state,omitempty"`
		Branch       *string    `json:"branch,omitempty"`
		PRURL        *string    `json:"pr_url,omitempty"`
		StartedAt    time.Time  `json:"started_at"`
		FinishedAt   *time.Time `json:"finished_at,omitempty"`
	}
	var out []runView
	for rows.Next() {
		var v runView
		if err := rows.Scan(&v.ID, &v.ProjectID, &v.Remote, &v.IssueNumber, &v.Status,
			&v.CurrentState, &v.Branch, &v.PRURL, &v.StartedAt, &v.FinishedAt); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": out})
}

// --- egress log ------------------------------------------------------------

// egressIngestReq is the runner-side wire shape the shipper posts. Each entry
// carries host + allowed + ts only — never content, bytes, or credentials
// (§11.6 invariant). lease_id / run_id are NOT populated by the runner: squid
// does not know which lease originated a request, so the bootstrap tenant is
// stamped here and lease/run linkage is deferred to Vaihe 2 (per-run sidecar
// proxies).
type egressIngestReq struct {
	Entries []egressIngestEntry `json:"entries"`
}

type egressIngestEntry struct {
	Host    string    `json:"host"`
	Allowed bool      `json:"allowed"`
	TS      time.Time `json:"ts"`
}

func (s *Server) handleEgressIngest(w http.ResponseWriter, r *http.Request) {
	var req egressIngestReq
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	if len(req.Entries) == 0 {
		writeJSON(w, http.StatusAccepted, map[string]int{"accepted": 0})
		return
	}
	ctx, cancel := withTimeout(r, 10*time.Second)
	defer cancel()

	// Build the bulk insert via UNNEST — one round-trip regardless of batch size.
	hosts := make([]string, len(req.Entries))
	allowed := make([]bool, len(req.Entries))
	tss := make([]time.Time, len(req.Entries))
	for i, e := range req.Entries {
		if e.Host == "" {
			writeErr(w, http.StatusBadRequest, "entry host required")
			return
		}
		hosts[i] = e.Host
		allowed[i] = e.Allowed
		if e.TS.IsZero() {
			tss[i] = time.Now().UTC()
		} else {
			tss[i] = e.TS
		}
	}
	_, err := s.Pool.Exec(ctx, `
		INSERT INTO egress_log (tenant_id, host, allowed, ts)
		SELECT $1, h, a, t
		  FROM UNNEST($2::text[], $3::boolean[], $4::timestamptz[]) AS u(h, a, t)`,
		s.TenantID, hosts, allowed, tss)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "egress ingest: "+err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]int{"accepted": len(req.Entries)})
}

func (s *Server) handleEgressList(w http.ResponseWriter, r *http.Request) {
	tenantID := tenantFromCtx(r.Context())
	ctx, cancel := withTimeout(r, 5*time.Second)
	defer cancel()
	runID := r.URL.Query().Get("run")
	rows, err := s.Pool.Query(ctx, `
		SELECT run_id::text, host, allowed, ts
		  FROM egress_log
		 WHERE tenant_id = $1 AND ($2 = '' OR run_id::text = $2)
		 ORDER BY ts DESC LIMIT 500`, tenantID, runID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()
	type egressView struct {
		RunID   *string   `json:"run_id,omitempty"`
		Host    string    `json:"host"`
		Allowed bool      `json:"allowed"`
		TS      time.Time `json:"ts"`
	}
	var out []egressView
	for rows.Next() {
		var v egressView
		if err := rows.Scan(&v.RunID, &v.Host, &v.Allowed, &v.TS); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, map[string]any{"egress": out})
}

// --- small helpers ---------------------------------------------------------

func randomToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func capsJSON(m map[string]any) []byte {
	if m == nil {
		return nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil
	}
	return b
}
