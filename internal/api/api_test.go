package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Silon-Oy/flow/internal/auth"
	"github.com/Silon-Oy/flow/internal/store"
)

// seedAdminSession creates an admin app_user in tenant + a fresh user_session
// row, returning the raw bearer token a test client should send as
// `Authorization: Bearer <token>` to satisfy RequireAuth/RequireRole on the
// RBAC-gated routes (§7 — `/v1/runs`, `/v1/runners`).
func seedAdminSession(t *testing.T, pool *pgxpool.Pool, tenantID string) string {
	t.Helper()
	return seedSession(t, pool, tenantID, "admin", "admin-"+tenantID)
}

func seedSession(t *testing.T, pool *pgxpool.Pool, tenantID, role, login string) string {
	t.Helper()
	ctx := context.Background()
	var userID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO app_user (tenant_id, github_login, role)
		VALUES ($1, $2, $3::user_role)
		ON CONFLICT (tenant_id, github_login) DO UPDATE SET role = EXCLUDED.role
		RETURNING id::text`, tenantID, login, role).Scan(&userID); err != nil {
		t.Fatalf("seed app_user: %v", err)
	}
	raw := fmt.Sprintf("test-token-%d-%s", time.Now().UnixNano(), userID)
	hash := auth.HashToken(raw)
	if _, err := pool.Exec(ctx, `
		INSERT INTO user_session (user_id, token_hash, expires_at)
		VALUES ($1, $2, now() + interval '1 hour')`, userID, hash); err != nil {
		t.Fatalf("seed user_session: %v", err)
	}
	return raw
}

func getWithToken(t *testing.T, url, token string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	return resp
}

func newTestServer(t *testing.T) (*httptest.Server, *pgxpool.Pool, string, string) {
	t.Helper()
	dsn := os.Getenv("FLOW_TEST_DSN")
	if dsn == "" {
		t.Skip("FLOW_TEST_DSN not set — skipping api integration test")
	}
	if err := store.Migrate(dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)

	name := fmt.Sprintf("api-%d", time.Now().UnixNano())
	var tenantID, projectID string
	if err := pool.QueryRow(ctx, `INSERT INTO tenant (name) VALUES ($1) RETURNING id::text`, name).Scan(&tenantID); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO project (tenant_id, name, owner_repo) VALUES ($1, $2, 'o/r') RETURNING id::text`, tenantID, name).Scan(&projectID); err != nil {
		t.Fatalf("seed project: %v", err)
	}

	srv := New(pool, tenantID)
	ts := httptest.NewServer(srv.Routes())
	t.Cleanup(ts.Close)
	return ts, pool, tenantID, projectID
}

func post(t *testing.T, url string, body any) (*http.Response, []byte) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	resp, err := http.Post(url, "application/json", &buf)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	data := readBody(t, resp)
	return resp, data
}

// postAuth posts with a runner-token bearer (§7(b)). Runner-write endpoints
// require it; the register response is the only place the raw token is handed
// out.
func postAuth(t *testing.T, url, token string, body any) (*http.Response, []byte) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	req, err := http.NewRequest(http.MethodPost, url, &buf)
	if err != nil {
		t.Fatalf("new request %s: %v", url, err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	return resp, readBody(t, resp)
}

func readBody(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	defer resp.Body.Close()
	buf := new(bytes.Buffer)
	_, _ = buf.ReadFrom(resp.Body)
	return buf.Bytes()
}

// TestFullRunnerWorkflow walks the runner's wire contract end to end.
func TestFullRunnerWorkflow(t *testing.T) {
	ts, pool, tenantID, projectID := newTestServer(t)
	ctx := context.Background()

	// 1. Register a runner.
	resp, data := post(t, ts.URL+"/v1/runners/register", map[string]any{"hostname": "studio", "capacity": 2})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("register: %d %s", resp.StatusCode, data)
	}
	var reg struct {
		RunnerID    string `json:"runner_id"`
		RunnerToken string `json:"runner_token"`
	}
	mustJSON(t, data, &reg)
	if reg.RunnerID == "" || reg.RunnerToken == "" {
		t.Fatalf("register returned empty id/token: %s", data)
	}

	// 2. Empty queue -> 204 (not an error). All runner-write calls below carry
	// the §7(b) runner token minted at register.
	resp, _ = postAuth(t, ts.URL+"/v1/leases/acquire", reg.RunnerToken, map[string]any{"runner_id": reg.RunnerID, "kinds": []string{"develop"}})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("acquire empty: %d, want 204", resp.StatusCode)
	}

	// 3. Seed work, then acquire it.
	wk := "api-wk-" + tenantID
	if _, err := pool.Exec(ctx,
		`INSERT INTO claimable_work (tenant_id, project_id, work_key, remote, issue_number, kind)
		 VALUES ($1,$2,$3,'origin',55,'develop')`, tenantID, projectID, wk); err != nil {
		t.Fatal(err)
	}
	resp, data = postAuth(t, ts.URL+"/v1/leases/acquire", reg.RunnerToken, map[string]any{"runner_id": reg.RunnerID, "kinds": []string{"develop"}})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("acquire: %d %s", resp.StatusCode, data)
	}
	var acq struct {
		Lease struct {
			ID string `json:"ID"`
		} `json:"lease"`
		Work struct {
			IssueNumber int `json:"IssueNumber"`
		} `json:"work"`
	}
	mustJSON(t, data, &acq)
	if acq.Work.IssueNumber != 55 {
		t.Errorf("acquired issue = %d, want 55", acq.Work.IssueNumber)
	}

	// 4. Lease heartbeat.
	resp, _ = postAuth(t, ts.URL+"/v1/leases/"+acq.Lease.ID+"/heartbeat", reg.RunnerToken, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("lease heartbeat: %d", resp.StatusCode)
	}

	// 5. Create a run.
	resp, data = postAuth(t, ts.URL+"/v1/runs", reg.RunnerToken, map[string]any{"project_id": projectID, "remote": "origin", "issue_number": 55})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create run: %d %s", resp.StatusCode, data)
	}
	var rc struct {
		RunID string `json:"run_id"`
	}
	mustJSON(t, data, &rc)

	// 6. Patch the run (branch + status).
	patchReq, _ := http.NewRequest(http.MethodPatch, ts.URL+"/v1/runs/"+rc.RunID,
		bytes.NewBufferString(`{"branch":"auto-run/issue-55","current_state":"S8"}`))
	patchReq.Header.Set("Content-Type", "application/json")
	patchReq.Header.Set("Authorization", "Bearer "+reg.RunnerToken)
	pr, err := http.DefaultClient.Do(patchReq)
	if err != nil {
		t.Fatal(err)
	}
	if pr.StatusCode != http.StatusOK {
		t.Fatalf("patch run: %d %s", pr.StatusCode, readBody(t, pr))
	}
	pr.Body.Close()

	// 7. Push events.
	resp, data = postAuth(t, ts.URL+"/v1/runs/"+rc.RunID+"/events", reg.RunnerToken, map[string]any{
		"events": []map[string]any{
			{"event": "claimed", "data": map[string]string{"work_key": wk}},
			{"event": "implementer_result", "data": map[string]string{"result": "SUCCESS"}},
		},
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("events: %d %s", resp.StatusCode, data)
	}

	// 8. GET the run, confirm patched state.
	gr, err := http.Get(ts.URL + "/v1/runs/" + rc.RunID)
	if err != nil {
		t.Fatal(err)
	}
	grData := readBody(t, gr)
	var run struct {
		Branch *string `json:"branch"`
		Status string  `json:"status"`
	}
	mustJSON(t, grData, &run)
	if run.Branch == nil || *run.Branch != "auto-run/issue-55" {
		t.Errorf("run branch = %v, want auto-run/issue-55", run.Branch)
	}

	// 9. Release lease.
	resp, _ = postAuth(t, ts.URL+"/v1/leases/"+acq.Lease.ID+"/release", reg.RunnerToken, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("release: %d", resp.StatusCode)
	}

	// 10. List runs (filtered). /v1/runs is RBAC-gated (§7) so we need an
	// admin session to see the whole tenant.
	adminToken := seedAdminSession(t, pool, tenantID)
	lr := getWithToken(t, ts.URL+"/v1/runs", adminToken)
	var list struct {
		Runs []map[string]any `json:"runs"`
	}
	mustJSON(t, readBody(t, lr), &list)
	if len(list.Runs) == 0 {
		t.Errorf("expected at least one run in list")
	}
}

// TestRunnerTokenScope is the §7(b) acceptance test: the per-runner token
// minted at register is REQUIRED on runner-write endpoints (full enforcement —
// a request with no token, or a bogus token, is rejected) and the token is
// scoped to those endpoints only. The dashboard/read endpoints do not honor it
// as a credential — their user-session enforcement is issue #7 — so the runner
// token can never be the thing that unlocks CLI/dashboard data.
func TestRunnerTokenScope(t *testing.T) {
	ts, pool, tenantID, _ := newTestServer(t)
	ctx := context.Background()

	// Register → mint a real scoped runner token; its hash (not the raw token)
	// is persisted on the runner row.
	resp, data := post(t, ts.URL+"/v1/runners/register", map[string]any{"hostname": "scope-host", "capacity": 1})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("register: %d %s", resp.StatusCode, data)
	}
	var reg struct {
		RunnerID    string `json:"runner_id"`
		RunnerToken string `json:"runner_token"`
	}
	mustJSON(t, data, &reg)
	if reg.RunnerToken == "" {
		t.Fatalf("register returned empty token: %s", data)
	}

	// The raw token is NOT stored — only its SHA-256 hash, and it matches.
	var storedHash []byte
	if err := pool.QueryRow(ctx, `SELECT token_hash FROM runner WHERE id = $1`, reg.RunnerID).Scan(&storedHash); err != nil {
		t.Fatalf("read token_hash: %v", err)
	}
	if !bytes.Equal(storedHash, auth.HashToken(reg.RunnerToken)) {
		t.Errorf("stored token_hash does not match sha256(raw token)")
	}
	if bytes.Equal(storedHash, []byte(reg.RunnerToken)) {
		t.Errorf("raw token was persisted verbatim — must store only the hash")
	}

	acquireBody := map[string]any{"runner_id": reg.RunnerID, "kinds": []string{"develop"}}

	// 1. Runner-write with NO token → 401 (the gate is closed by default).
	resp, _ = post(t, ts.URL+"/v1/leases/acquire", acquireBody)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("acquire without token = %d, want 401", resp.StatusCode)
	}

	// 2. Runner-write with a BOGUS token → 401.
	resp, _ = postAuth(t, ts.URL+"/v1/leases/acquire", "not-a-real-token", acquireBody)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("acquire with bogus token = %d, want 401", resp.StatusCode)
	}

	// 3. Runner-write with the VALID token → accepted (204 = empty queue, which
	//    is success: the request passed auth and reached the handler).
	resp, _ = postAuth(t, ts.URL+"/v1/leases/acquire", reg.RunnerToken, acquireBody)
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("acquire with valid token = %d, want 204", resp.StatusCode)
	}

	// 4. The same valid runner token, presented to a runner-write endpoint of
	//    another shape (POST /v1/runs), is likewise accepted — proving the token
	//    is a first-class runner credential across the runner-write group.
	resp, data = postAuth(t, ts.URL+"/v1/runs", reg.RunnerToken,
		map[string]any{"project_id": seedProject(t, pool, tenantID), "remote": "origin", "issue_number": 7})
	if resp.StatusCode != http.StatusCreated {
		t.Errorf("create run with valid token = %d, want 201 (%s)", resp.StatusCode, data)
	}
}

// seedProject inserts a throwaway project under the given tenant and returns its
// id — runner-write tests need a project_id that belongs to the bootstrap
// tenant the runner token resolves to.
func seedProject(t *testing.T, pool *pgxpool.Pool, tenantID string) string {
	t.Helper()
	var id string
	name := fmt.Sprintf("scope-proj-%d", time.Now().UnixNano())
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO project (tenant_id, name, owner_repo) VALUES ($1, $2, 'o/r') RETURNING id::text`,
		tenantID, name).Scan(&id); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	return id
}

// TestEgressIngestAndList exercises the §11.6 shipper wire contract end-to-end:
// POST /v1/egress accepts a batch, the rows land on the bootstrap tenant, and
// GET /v1/egress surfaces them (allowed + denied) for the dashboard.
func TestEgressIngestAndList(t *testing.T) {
	ts, _, _, _ := newTestServer(t)

	now := time.Now().UTC().Truncate(time.Second)
	resp, data := post(t, ts.URL+"/v1/egress", map[string]any{
		"entries": []map[string]any{
			{"host": "github.com", "allowed": true, "ts": now.Format(time.RFC3339Nano)},
			{"host": "evil.example.com", "allowed": false, "ts": now.Add(time.Second).Format(time.RFC3339Nano)},
		},
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("ingest: %d %s", resp.StatusCode, data)
	}
	var ack struct {
		Accepted int `json:"accepted"`
	}
	mustJSON(t, data, &ack)
	if ack.Accepted != 2 {
		t.Errorf("accepted = %d, want 2", ack.Accepted)
	}

	gr, err := http.Get(ts.URL + "/v1/egress")
	if err != nil {
		t.Fatal(err)
	}
	var list struct {
		Egress []struct {
			Host    string `json:"host"`
			Allowed bool   `json:"allowed"`
		} `json:"egress"`
	}
	mustJSON(t, readBody(t, gr), &list)

	var sawAllowed, sawDenied bool
	for _, e := range list.Egress {
		if e.Host == "github.com" && e.Allowed {
			sawAllowed = true
		}
		if e.Host == "evil.example.com" && !e.Allowed {
			sawDenied = true
		}
	}
	if !sawAllowed || !sawDenied {
		t.Errorf("list missing entries: allowed=%v denied=%v rows=%+v", sawAllowed, sawDenied, list.Egress)
	}
}

func mustJSON(t *testing.T, data []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(data, v); err != nil {
		t.Fatalf("unmarshal %s: %v", data, err)
	}
}

// TestTenantIsolation is the load-bearing §7 invariant test: two tenants live
// in the same DB, each with their own run + runner, and a request bearing one
// tenant's X-Flow-Tenant-ID MUST NOT read or mutate the other tenant's data
// through any endpoint. This is the property the WithTenant middleware exists
// to guarantee.
func TestTenantIsolation(t *testing.T) {
	dsn := os.Getenv("FLOW_TEST_DSN")
	if dsn == "" {
		t.Skip("FLOW_TEST_DSN not set — skipping tenant isolation test")
	}
	ctx := context.Background()
	pool, _, _, _ := newTestServerForIsolation(t, dsn)

	// Two tenants, two projects, two runs. Seed via SQL — the wire path is
	// what we want to test, not the seeding plumbing.
	stamp := time.Now().UnixNano()
	seedTenant := func(name string) (tenantID, projectID, runnerID string) {
		t.Helper()
		if err := pool.QueryRow(ctx, `INSERT INTO tenant (name) VALUES ($1) RETURNING id::text`, name).Scan(&tenantID); err != nil {
			t.Fatalf("seed tenant %s: %v", name, err)
		}
		if err := pool.QueryRow(ctx,
			`INSERT INTO project (tenant_id, name, owner_repo) VALUES ($1, $2, 'o/r') RETURNING id::text`,
			tenantID, name).Scan(&projectID); err != nil {
			t.Fatalf("seed project %s: %v", name, err)
		}
		if err := pool.QueryRow(ctx,
			`INSERT INTO runner (tenant_id, hostname) VALUES ($1, $2) RETURNING id::text`,
			tenantID, "runner-"+name).Scan(&runnerID); err != nil {
			t.Fatalf("seed runner %s: %v", name, err)
		}
		return
	}
	tA, pA, rA := seedTenant(fmt.Sprintf("iso-A-%d", stamp))
	tB, pB, rB := seedTenant(fmt.Sprintf("iso-B-%d", stamp))

	// Mint a runner token for A's runner so the §7(b) runner-write path can be
	// exercised cross-tenant: the token resolves to tenant A, so presenting it
	// against B's runner must 404 on the tenant filter. Isolation on runner-write
	// endpoints now rides the token-derived tenant, not the X-Flow-Tenant-ID
	// header (the runner-token extractor ignores that header entirely).
	runnerTokenA := fmt.Sprintf("iso-runner-token-A-%d", stamp)
	if _, err := pool.Exec(ctx, `UPDATE runner SET token_hash = $1 WHERE id = $2`,
		auth.HashToken(runnerTokenA), rA); err != nil {
		t.Fatalf("seed runner token: %v", err)
	}

	// The Server is constructed with tenant-A as the bootstrap fallback so
	// requests without a header default to A — proves the test is actually
	// exercising the explicit header path for B (not falling back).
	srv := New(pool, tA)
	ts := httptest.NewServer(srv.Routes())
	t.Cleanup(ts.Close)

	// Create one run per tenant via SQL so we know each tenant's run id without
	// going through the API (which itself we are about to test).
	runFor := func(tenantID, projectID string, issue int) string {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx,
			`INSERT INTO run (tenant_id, project_id, remote, issue_number, status, current_state)
			 VALUES ($1, $2, 'origin', $3, 'initialized', 'S0_Idle') RETURNING id::text`,
			tenantID, projectID, issue).Scan(&id); err != nil {
			t.Fatalf("seed run: %v", err)
		}
		return id
	}
	runA := runFor(tA, pA, 101)
	runB := runFor(tB, pB, 202)

	// Seed an admin session per tenant so the RBAC-gated routes (§7) can be
	// exercised. The bearer token's tenant matches the X-Flow-Tenant-ID
	// header — RequireAuth's cross-tenant guard rejects mismatches.
	tokenA := seedSession(t, pool, tA, "admin", "admin-A-"+tA)
	tokenB := seedSession(t, pool, tB, "admin", "admin-B-"+tB)
	tokenFor := func(tenantHeader string) string {
		switch tenantHeader {
		case tA:
			return tokenA
		case tB:
			return tokenB
		default:
			return ""
		}
	}

	do := func(method, path, tenantHeader string, body any) *http.Response {
		t.Helper()
		var buf bytes.Buffer
		if body != nil {
			if err := json.NewEncoder(&buf).Encode(body); err != nil {
				t.Fatal(err)
			}
		}
		req, err := http.NewRequest(method, ts.URL+path, &buf)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		if tenantHeader != "" {
			req.Header.Set("X-Flow-Tenant-ID", tenantHeader)
			if tok := tokenFor(tenantHeader); tok != "" {
				req.Header.Set("Authorization", "Bearer "+tok)
			}
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		return resp
	}

	// doToken is the runner-write variant: it authenticates with a §7(b) runner
	// token bearer instead of the X-Flow-Tenant-ID header. Runner-write endpoints
	// derive the tenant from the token, so cross-tenant isolation on those routes
	// is proven by presenting A's token against B's resources.
	doToken := func(method, path, token string, body any) *http.Response {
		t.Helper()
		var buf bytes.Buffer
		if body != nil {
			if err := json.NewEncoder(&buf).Encode(body); err != nil {
				t.Fatal(err)
			}
		}
		req, err := http.NewRequest(method, ts.URL+path, &buf)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		return resp
	}

	// 1. GET /v1/runs/{tenant-B-run} with tenant-A header MUST be 404.
	resp := do(http.MethodGet, "/v1/runs/"+runB, tA, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("A reads B's run = %d, want 404 (cross-tenant leak)", resp.StatusCode)
	}
	resp.Body.Close()

	// 2. Same path with tenant-B header MUST succeed (proving the 404 above was
	//    tenancy, not a fixture bug).
	resp = do(http.MethodGet, "/v1/runs/"+runB, tB, nil)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("B reads B's run = %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()

	// 3. PATCH cross-tenant MUST be 404 and MUST NOT mutate the row. This is a
	//    runner-write endpoint: A presents its runner token (→ tenant A) against
	//    B's run; the tenant filter rejects it as not-found.
	resp = doToken(http.MethodPatch, "/v1/runs/"+runB, runnerTokenA,
		map[string]any{"current_state": "ATTACKER_WROTE_THIS"})
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("A patches B's run = %d, want 404", resp.StatusCode)
	}
	resp.Body.Close()
	var cur string
	if err := pool.QueryRow(ctx, `SELECT current_state FROM run WHERE id = $1`, runB).Scan(&cur); err != nil {
		t.Fatal(err)
	}
	if cur == "ATTACKER_WROTE_THIS" {
		t.Errorf("cross-tenant PATCH actually wrote: current_state=%q", cur)
	}

	// 4. POST events cross-tenant MUST be 404 and MUST NOT insert a row.
	var beforeEvents int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM run_event WHERE run_id = $1`, runB).Scan(&beforeEvents); err != nil {
		t.Fatal(err)
	}
	resp = doToken(http.MethodPost, "/v1/runs/"+runB+"/events", runnerTokenA, map[string]any{
		"events": []map[string]any{{"event": "attacker", "data": map[string]string{"x": "y"}}},
	})
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("A appends to B's events = %d, want 404", resp.StatusCode)
	}
	resp.Body.Close()
	var afterEvents int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM run_event WHERE run_id = $1`, runB).Scan(&afterEvents); err != nil {
		t.Fatal(err)
	}
	if afterEvents != beforeEvents {
		t.Errorf("cross-tenant event insert leaked: before=%d after=%d", beforeEvents, afterEvents)
	}

	// 5. GET /v1/runs with tenant-A MUST NOT include B's run, and vice versa.
	resp = do(http.MethodGet, "/v1/runs", tA, nil)
	var listA struct {
		Runs []struct {
			ID string `json:"id"`
		} `json:"runs"`
	}
	mustJSON(t, readBody(t, resp), &listA)
	for _, r := range listA.Runs {
		if r.ID == runB {
			t.Errorf("A's run list contains B's run %s", runB)
		}
	}

	resp = do(http.MethodGet, "/v1/runs", tB, nil)
	var listB struct {
		Runs []struct {
			ID string `json:"id"`
		} `json:"runs"`
	}
	mustJSON(t, readBody(t, resp), &listB)
	for _, r := range listB.Runs {
		if r.ID == runA {
			t.Errorf("B's run list contains A's run %s", runA)
		}
	}

	// 6. GET /v1/runners cross-tenant MUST NOT see the other tenant's runner.
	resp = do(http.MethodGet, "/v1/runners", tA, nil)
	var runnersA struct {
		Runners []struct {
			ID string `json:"id"`
		} `json:"runners"`
	}
	mustJSON(t, readBody(t, resp), &runnersA)
	for _, r := range runnersA.Runners {
		if r.ID == rB {
			t.Errorf("A sees B's runner %s in list", rB)
		}
	}

	// 7. Runner heartbeat cross-tenant MUST be 404 (and MUST NOT bump the
	//    other tenant's runner heartbeat). This is now a §7(b) runner-write
	//    endpoint: A presents its runner token (→ tenant A) against B's runner,
	//    and the handler's tenant filter rejects it. last_heartbeat is nullable
	//    until the first heartbeat lands, so use *time.Time and compare via the
	//    pointer.
	var prevHeartbeat *time.Time
	if err := pool.QueryRow(ctx,
		`SELECT last_heartbeat FROM runner WHERE id = $1`, rB).Scan(&prevHeartbeat); err != nil {
		t.Fatal(err)
	}
	resp, _ = postAuth(t, ts.URL+"/v1/runners/"+rB+"/heartbeat", runnerTokenA, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("A heartbeats B's runner = %d, want 404", resp.StatusCode)
	}
	var newHeartbeat *time.Time
	if err := pool.QueryRow(ctx,
		`SELECT last_heartbeat FROM runner WHERE id = $1`, rB).Scan(&newHeartbeat); err != nil {
		t.Fatal(err)
	}
	bumped := (prevHeartbeat == nil) != (newHeartbeat == nil) ||
		(prevHeartbeat != nil && newHeartbeat != nil && !newHeartbeat.Equal(*prevHeartbeat))
	if bumped {
		t.Errorf("cross-tenant heartbeat actually bumped B's runner: %v -> %v", prevHeartbeat, newHeartbeat)
	}

	// 8. Sanity: tenant A and tenant B are NOT the same uuid (the test wouldn't
	//    catch anything otherwise).
	if tA == tB {
		t.Fatalf("tenant ids collided: %s == %s", tA, tB)
	}
	// Silence unused: rA is only here to symmetrically prove A's runner exists.
	_ = rA
}

// newTestServerForIsolation returns a server WITHOUT seeding tenant/project —
// the isolation test seeds two tenants explicitly. Returns the pool and the
// bootstrap tenant id of the inner Server (unused fields kept for parity with
// newTestServer).
func newTestServerForIsolation(t *testing.T, dsn string) (*pgxpool.Pool, *httptest.Server, string, string) {
	t.Helper()
	if err := store.Migrate(dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool, nil, "", ""
}
