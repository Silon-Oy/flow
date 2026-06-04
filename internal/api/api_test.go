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

	"github.com/Silon-Oy/flow/internal/store"
)

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

	// 2. Empty queue -> 204 (not an error).
	resp, _ = post(t, ts.URL+"/v1/leases/acquire", map[string]any{"runner_id": reg.RunnerID, "kinds": []string{"develop"}})
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
	resp, data = post(t, ts.URL+"/v1/leases/acquire", map[string]any{"runner_id": reg.RunnerID, "kinds": []string{"develop"}})
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
	resp, _ = post(t, ts.URL+"/v1/leases/"+acq.Lease.ID+"/heartbeat", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("lease heartbeat: %d", resp.StatusCode)
	}

	// 5. Create a run.
	resp, data = post(t, ts.URL+"/v1/runs", map[string]any{"project_id": projectID, "remote": "origin", "issue_number": 55})
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
	pr, err := http.DefaultClient.Do(patchReq)
	if err != nil {
		t.Fatal(err)
	}
	if pr.StatusCode != http.StatusOK {
		t.Fatalf("patch run: %d %s", pr.StatusCode, readBody(t, pr))
	}
	pr.Body.Close()

	// 7. Push events.
	resp, data = post(t, ts.URL+"/v1/runs/"+rc.RunID+"/events", map[string]any{
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
	resp, _ = post(t, ts.URL+"/v1/leases/"+acq.Lease.ID+"/release", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("release: %d", resp.StatusCode)
	}

	// 10. List runs (filtered).
	lr, err := http.Get(ts.URL + "/v1/runs")
	if err != nil {
		t.Fatal(err)
	}
	var list struct {
		Runs []map[string]any `json:"runs"`
	}
	mustJSON(t, readBody(t, lr), &list)
	if len(list.Runs) == 0 {
		t.Errorf("expected at least one run in list")
	}
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

	// 3. PATCH cross-tenant MUST be 404 and MUST NOT mutate the row.
	resp = do(http.MethodPatch, "/v1/runs/"+runB, tA,
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
	resp = do(http.MethodPost, "/v1/runs/"+runB+"/events", tA, map[string]any{
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
	//    other tenant's runner heartbeat). last_heartbeat is nullable until the
	//    first heartbeat lands, so use *time.Time and compare via the pointer.
	var prevHeartbeat *time.Time
	if err := pool.QueryRow(ctx,
		`SELECT last_heartbeat FROM runner WHERE id = $1`, rB).Scan(&prevHeartbeat); err != nil {
		t.Fatal(err)
	}
	resp = do(http.MethodPost, "/v1/runners/"+rB+"/heartbeat", tA, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("A heartbeats B's runner = %d, want 404", resp.StatusCode)
	}
	resp.Body.Close()
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
