package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// putWithToken is the PUT counterpart of postWithToken. Kept local to the
// merge-policy tests — if a second PUT endpoint lands, lift it to api_test.go.
func putWithToken(t *testing.T, url, token string, body any) *http.Response {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	req, err := http.NewRequest(http.MethodPut, url, &buf)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT %s: %v", url, err)
	}
	return resp
}

// seedMergePolicyProject inserts a tenant-scoped project so the merge-policy
// tests have a real uuid to target. Returns the project id. Uses the same
// path the bootstrap data does — INSERT INTO project — so we don't go through
// the wizard endpoint (whose validation isn't what we're testing here).
func seedMergePolicyProject(t *testing.T, pool *pgxpool.Pool, tenantID string) string {
	t.Helper()
	var id string
	name := fmt.Sprintf("mp-proj-%d", time.Now().UnixNano())
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO project (tenant_id, name, owner_repo) VALUES ($1, $2, 'o/r') RETURNING id::text`,
		tenantID, name).Scan(&id); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	return id
}

// TestMergePolicy_AdminUpdates proves the happy path: an admin can write a
// merge_policy with the two known fields, the row reflects it, and the
// response echoes the value the dashboard form can rerender from.
func TestMergePolicy_AdminUpdates(t *testing.T) {
	ts, pool, tenantID, _ := newTestServer(t)
	projectID := seedMergePolicyProject(t, pool, tenantID)
	adminToken := seedAdminSession(t, pool, tenantID)

	body := map[string]any{
		"label":               "ready-to-merge",
		"conflict_resolution": true,
	}
	resp := putWithToken(t, ts.URL+"/v1/projects/"+projectID+"/merge-policy", adminToken, body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", resp.StatusCode, readBody(t, resp))
	}
	var got struct {
		MergePolicy map[string]any `json:"merge_policy"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.MergePolicy["label"] != "ready-to-merge" {
		t.Errorf("echoed label = %v, want ready-to-merge", got.MergePolicy["label"])
	}
	if got.MergePolicy["conflict_resolution"] != true {
		t.Errorf("echoed conflict_resolution = %v, want true", got.MergePolicy["conflict_resolution"])
	}

	// Round-trip via the DB: the stored jsonb matches what we wrote.
	var raw []byte
	if err := pool.QueryRow(context.Background(),
		`SELECT merge_policy FROM project WHERE id = $1`, projectID).Scan(&raw); err != nil {
		t.Fatalf("read row: %v", err)
	}
	var stored map[string]any
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatalf("decode stored: %v", err)
	}
	if stored["label"] != "ready-to-merge" || stored["conflict_resolution"] != true {
		t.Errorf("stored merge_policy = %v, want {label:ready-to-merge, conflict_resolution:true}", stored)
	}
}

// TestMergePolicy_DeveloperForbidden locks the §7 row "Muokkaa merge-policya"
// at the wire: a developer hitting the endpoint gets 403, never 200 or 400.
// This is the property the dashboard's panel-hiding logic ultimately leans on
// — even if the UI showed the panel, the backend would still refuse.
func TestMergePolicy_DeveloperForbidden(t *testing.T) {
	ts, pool, tenantID, _ := newTestServer(t)
	projectID := seedMergePolicyProject(t, pool, tenantID)
	devToken := seedSession(t, pool, tenantID, "developer", "mp-dev-"+tenantID)

	resp := putWithToken(t, ts.URL+"/v1/projects/"+projectID+"/merge-policy",
		devToken, map[string]any{"label": "auto-merge"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403", resp.StatusCode)
	}
}

// TestMergePolicy_Unauthenticated proves the route is RBAC-gated at the wire:
// no bearer = 401, not 400 from JSON parsing.
func TestMergePolicy_Unauthenticated(t *testing.T) {
	ts, pool, tenantID, _ := newTestServer(t)
	projectID := seedMergePolicyProject(t, pool, tenantID)

	resp := putWithToken(t, ts.URL+"/v1/projects/"+projectID+"/merge-policy", "",
		map[string]any{"label": "auto-merge"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

// TestMergePolicy_Validation walks every payload-rejection branch — keeps
// each failure attributable. We seed an admin so the request reaches the
// handler before being rejected on shape.
func TestMergePolicy_Validation(t *testing.T) {
	ts, pool, tenantID, _ := newTestServer(t)
	projectID := seedMergePolicyProject(t, pool, tenantID)
	adminToken := seedAdminSession(t, pool, tenantID)

	cases := []struct {
		name string
		path string
		body any
		want int
	}{
		{
			name: "bad_id",
			path: "/v1/projects/not-a-uuid/merge-policy",
			body: map[string]any{"label": "auto-merge"},
			want: http.StatusBadRequest,
		},
		{
			name: "label_with_space",
			path: "/v1/projects/" + projectID + "/merge-policy",
			body: map[string]any{"label": "auto merge"},
			want: http.StatusBadRequest,
		},
		{
			name: "unknown_field",
			path: "/v1/projects/" + projectID + "/merge-policy",
			body: map[string]any{"label": "auto-merge", "secret_value": "ghp_xxx"},
			want: http.StatusBadRequest,
		},
		{
			name: "unknown_project",
			path: "/v1/projects/00000000-0000-0000-0000-000000000001/merge-policy",
			body: map[string]any{"label": "auto-merge"},
			want: http.StatusNotFound,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := putWithToken(t, ts.URL+tc.path, adminToken, tc.body)
			defer resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Errorf("status = %d, want %d (body=%s)", resp.StatusCode, tc.want, readBody(t, resp))
			}
		})
	}
}
