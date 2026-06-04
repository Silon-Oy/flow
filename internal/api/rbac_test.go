package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

// TestRBAC_RunsList_DeveloperSeesOnlyOwn proves the §7 visibility filter on
// GET /v1/runs: a developer sees only runs whose app_user_id matches them,
// pre-RBAC NULL-owner rows stay invisible, and an admin in the same tenant
// sees the whole tenant.
func TestRBAC_RunsList_DeveloperSeesOnlyOwn(t *testing.T) {
	ts, pool, tenantID, projectID := newTestServer(t)
	ctx := context.Background()

	// Seed two developers (alice/bob) and an admin in the same tenant.
	aliceToken := seedSession(t, pool, tenantID, "developer", "alice-"+tenantID)
	bobToken := seedSession(t, pool, tenantID, "developer", "bob-"+tenantID)
	adminToken := seedSession(t, pool, tenantID, "admin", "admin-"+tenantID)

	var aliceID, bobID string
	if err := pool.QueryRow(ctx, `SELECT id::text FROM app_user WHERE tenant_id=$1 AND github_login=$2`,
		tenantID, "alice-"+tenantID).Scan(&aliceID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT id::text FROM app_user WHERE tenant_id=$1 AND github_login=$2`,
		tenantID, "bob-"+tenantID).Scan(&bobID); err != nil {
		t.Fatal(err)
	}

	insertRun := func(owner *string, issue int) string {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx, `
			INSERT INTO run (tenant_id, project_id, remote, issue_number, status, current_state, app_user_id)
			VALUES ($1, $2, 'origin', $3, 'initialized', 'S0_Idle', $4)
			RETURNING id::text`, tenantID, projectID, issue, owner).Scan(&id); err != nil {
			t.Fatalf("insert run: %v", err)
		}
		return id
	}
	aliceRun := insertRun(&aliceID, 901)
	bobRun := insertRun(&bobID, 902)
	orphanRun := insertRun(nil, 903) // pre-RBAC, app_user_id IS NULL

	collect := func(token string) map[string]struct{} {
		t.Helper()
		resp := getWithToken(t, ts.URL+"/v1/runs", token)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body=%s)", resp.StatusCode, readBody(t, resp))
		}
		var list struct {
			Runs []struct{ ID string `json:"id"` } `json:"runs"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		out := make(map[string]struct{}, len(list.Runs))
		for _, r := range list.Runs {
			out[r.ID] = struct{}{}
		}
		return out
	}

	// Alice sees only her run; she does NOT see Bob's run or the NULL-owner run.
	got := collect(aliceToken)
	if _, ok := got[aliceRun]; !ok {
		t.Errorf("alice does not see her own run %s; got %v", aliceRun, got)
	}
	if _, ok := got[bobRun]; ok {
		t.Errorf("alice sees bob's run %s (developer→own filter broken)", bobRun)
	}
	if _, ok := got[orphanRun]; ok {
		t.Errorf("alice sees NULL-owner run %s (pre-RBAC rows must stay admin-only)", orphanRun)
	}

	// Bob sees only his run.
	got = collect(bobToken)
	if _, ok := got[bobRun]; !ok {
		t.Errorf("bob does not see his own run %s; got %v", bobRun, got)
	}
	if _, ok := got[aliceRun]; ok {
		t.Errorf("bob sees alice's run %s", aliceRun)
	}

	// Admin sees every run in the tenant — including the NULL-owner one.
	got = collect(adminToken)
	for _, want := range []string{aliceRun, bobRun, orphanRun} {
		if _, ok := got[want]; !ok {
			t.Errorf("admin does not see %s in tenant listing; got %v", want, got)
		}
	}
}

// TestRBAC_RunsList_RejectsUnauthenticated locks the wire-level RBAC gate:
// the bootstrap-tenant fallback no longer grants access to /v1/runs — a
// request without a bearer token is 401.
func TestRBAC_RunsList_RejectsUnauthenticated(t *testing.T) {
	ts, _, _, _ := newTestServer(t)
	resp, err := http.Get(ts.URL + "/v1/runs")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

// TestRBAC_RunnersList_DeveloperForbidden locks the §7 row "Hallitsee
// jaettuja runnereita" at the wire: a developer hitting /v1/runners gets 403.
func TestRBAC_RunnersList_DeveloperForbidden(t *testing.T) {
	ts, pool, tenantID, _ := newTestServer(t)
	devToken := seedSession(t, pool, tenantID, "developer", "dev-"+tenantID)

	resp := getWithToken(t, ts.URL+"/v1/runners", devToken)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403", resp.StatusCode)
	}
}

// TestRBAC_RunnersList_AdminAllowed pairs the above: same tenant, admin
// token, 200.
func TestRBAC_RunnersList_AdminAllowed(t *testing.T) {
	ts, pool, tenantID, _ := newTestServer(t)
	adminToken := seedAdminSession(t, pool, tenantID)

	resp := getWithToken(t, ts.URL+"/v1/runners", adminToken)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}
