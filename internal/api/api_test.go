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

func mustJSON(t *testing.T, data []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(data, v); err != nil {
		t.Fatalf("unmarshal %s: %v", data, err)
	}
}
