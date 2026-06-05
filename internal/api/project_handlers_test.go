package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Silon-Oy/flow/internal/store"
)

// stubBranchValidator records the calls and returns the configured outcome.
// The §8 acceptance criterion is "validation in central, not just CLI"; we
// inject this so the test exercises every validation branch without needing
// a live GitHub.
type stubBranchValidator struct {
	calls []stubBranchCall
	err   error // returned for every call
	byKey map[string]error
}

type stubBranchCall struct {
	tenantID, org, repo, branch string
}

func (s *stubBranchValidator) CheckBranch(_ context.Context, tenantID, org, repo, branch string) error {
	s.calls = append(s.calls, stubBranchCall{tenantID, org, repo, branch})
	if s.byKey != nil {
		if e, ok := s.byKey[org+"/"+repo+"@"+branch]; ok {
			return e
		}
	}
	return s.err
}

// newProjectsTestServer is the equivalent of newTestServer but injects a
// stubBranchValidator so the §8 branch-existence check is observable.
func newProjectsTestServer(t *testing.T) (*httptest.Server, *pgxpool.Pool, string, *stubBranchValidator) {
	t.Helper()
	dsn := os.Getenv("FLOW_TEST_DSN")
	if dsn == "" {
		t.Skip("FLOW_TEST_DSN not set — skipping project handler integration test")
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

	name := fmt.Sprintf("proj-api-%d", time.Now().UnixNano())
	var tenantID string
	if err := pool.QueryRow(ctx, `INSERT INTO tenant (name) VALUES ($1) RETURNING id::text`, name).Scan(&tenantID); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}

	stub := &stubBranchValidator{}
	srv := New(pool, tenantID)
	srv.BranchValidator = stub
	ts := httptest.NewServer(srv.Routes())
	t.Cleanup(ts.Close)
	return ts, pool, tenantID, stub
}

// postWithToken posts a JSON body with a session-token bearer (the
// RBAC-scoped path POST /v1/projects requires).
func postWithToken(t *testing.T, url, token string, body any) *http.Response {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	req, err := http.NewRequest(http.MethodPost, url, &buf)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	return resp
}

// TestProjects_HappyPath: a fully valid developer-level request inserts the
// row, the branch validator is called once per remote with the resolved
// (org, repo, branch), and the persisted record round-trips intact through
// store.GetProject.
func TestProjects_HappyPath(t *testing.T) {
	ts, pool, tenantID, stub := newProjectsTestServer(t)
	devToken := seedSession(t, pool, tenantID, "developer", "init-dev-"+tenantID)

	body := map[string]any{
		"name":       "happy-" + tenantID[:8],
		"owner_repo": "Silon-Oy/flow",
		"remotes": []map[string]any{
			{"remote": "origin", "owner_repo": "Silon-Oy/flow"},
			{"remote": "upstream", "owner_repo": "Acme/flow", "base_branch": "develop"},
		},
		"labels":                 []string{"auto-run"},
		"base_branch":            "main",
		"claude_timeout_seconds": 3600,
		"merge_policy":           map[string]any{"label": "ready-to-merge"},
		"secret_refs":            map[string]string{"GH_TOKEN": "github-token-key"},
	}
	resp := postWithToken(t, ts.URL+"/v1/projects", devToken, body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		buf, _ := readAll(resp)
		t.Fatalf("status = %d, want 201; body=%s", resp.StatusCode, buf)
	}
	var out struct {
		ProjectID string `json:"project_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.ProjectID == "" {
		t.Fatalf("empty project_id")
	}

	// Branch validator called once per remote with the right resolved branch
	// (the second remote overrode base_branch to "develop"; the first inherited
	// "main"). This is the päätös 14 resolution proof.
	if len(stub.calls) != 2 {
		t.Fatalf("validator calls = %d, want 2: %+v", len(stub.calls), stub.calls)
	}
	want := []stubBranchCall{
		{tenantID, "Silon-Oy", "flow", "main"},
		{tenantID, "Acme", "flow", "develop"},
	}
	for i, c := range want {
		if stub.calls[i] != c {
			t.Errorf("call[%d] = %+v, want %+v", i, stub.calls[i], c)
		}
	}

	// Round-trip via store.GetProject — the persisted record matches the wire
	// input (notably remotes[1].BaseBranch override is intact).
	rec, err := store.GetProject(context.Background(), pool, tenantID, out.ProjectID)
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if rec.Name != body["name"] {
		t.Errorf("name = %q, want %q", rec.Name, body["name"])
	}
	if rec.OwnerRepo != "Silon-Oy/flow" {
		t.Errorf("owner_repo = %q", rec.OwnerRepo)
	}
	if rec.BaseBranch != "main" {
		t.Errorf("base_branch = %q", rec.BaseBranch)
	}
	if len(rec.Remotes) != 2 || rec.Remotes[1].BaseBranch != "develop" {
		t.Errorf("remotes = %+v", rec.Remotes)
	}
	if rec.ClaudeTimeoutSeconds != 3600 {
		t.Errorf("claude_timeout_seconds = %d", rec.ClaudeTimeoutSeconds)
	}
	if rec.SecretRefs["GH_TOKEN"] != "github-token-key" {
		t.Errorf("secret_refs = %v", rec.SecretRefs)
	}
}

// TestProjects_Validation walks every §8 structural rule the central checks.
// One case per branch keeps each failure attributable.
func TestProjects_Validation(t *testing.T) {
	ts, pool, tenantID, _ := newProjectsTestServer(t)
	devToken := seedSession(t, pool, tenantID, "developer", "init-val-"+tenantID)

	cases := []struct {
		name string
		body map[string]any
		want int
	}{
		{
			name: "missing_name",
			body: map[string]any{"owner_repo": "Silon-Oy/flow"},
			want: http.StatusBadRequest,
		},
		{
			name: "bad_owner_repo",
			body: map[string]any{"name": "x", "owner_repo": "not-a-slash"},
			want: http.StatusBadRequest,
		},
		{
			name: "bad_base_branch",
			body: map[string]any{"name": "x", "owner_repo": "Silon-Oy/flow", "base_branch": "bad branch"},
			want: http.StatusBadRequest,
		},
		{
			name: "negative_timeout",
			body: map[string]any{"name": "x", "owner_repo": "Silon-Oy/flow", "claude_timeout_seconds": -1},
			want: http.StatusBadRequest,
		},
		{
			name: "non_uuid_pool",
			body: map[string]any{"name": "x", "owner_repo": "Silon-Oy/flow", "runner_pool": "not-uuid"},
			want: http.StatusBadRequest,
		},
		{
			name: "secret_ref_plaintext_token",
			body: map[string]any{
				"name":        "x",
				"owner_repo":  "Silon-Oy/flow",
				"secret_refs": map[string]string{"GH_TOKEN": "ghp_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"},
			},
			want: http.StatusBadRequest,
		},
		{
			name: "duplicate_remote",
			body: map[string]any{
				"name":       "x",
				"owner_repo": "Silon-Oy/flow",
				"remotes": []map[string]any{
					{"remote": "origin", "owner_repo": "Silon-Oy/flow"},
					{"remote": "origin", "owner_repo": "Silon-Oy/flow"},
				},
			},
			want: http.StatusBadRequest,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := postWithToken(t, ts.URL+"/v1/projects", devToken, tc.body)
			buf, _ := readAll(resp)
			resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Errorf("status = %d, want %d; body=%s", resp.StatusCode, tc.want, buf)
			}
		})
	}
}

// TestProjects_BranchNotFound: the stub returns ErrBranchNotFound; the
// handler MUST surface 422 so the wizard can re-prompt for a valid branch.
func TestProjects_BranchNotFound(t *testing.T) {
	ts, pool, tenantID, stub := newProjectsTestServer(t)
	stub.err = ErrBranchNotFound
	devToken := seedSession(t, pool, tenantID, "developer", "init-422-"+tenantID)

	resp := postWithToken(t, ts.URL+"/v1/projects", devToken, map[string]any{
		"name":       "bad-branch-" + tenantID[:8],
		"owner_repo": "Silon-Oy/flow",
		"base_branch": "no-such-branch",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		buf, _ := readAll(resp)
		t.Fatalf("status = %d, want 422; body=%s", resp.StatusCode, buf)
	}
}

// TestProjects_InstallationMissing: the stub returns ErrInstallationMissing;
// the handler MUST surface 412 so the wizard tells the operator to register
// the App for that org.
func TestProjects_InstallationMissing(t *testing.T) {
	ts, pool, tenantID, stub := newProjectsTestServer(t)
	stub.err = ErrInstallationMissing
	devToken := seedSession(t, pool, tenantID, "developer", "init-412-"+tenantID)

	resp := postWithToken(t, ts.URL+"/v1/projects", devToken, map[string]any{
		"name":       "no-install-" + tenantID[:8],
		"owner_repo": "Silon-Oy/flow",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPreconditionFailed {
		buf, _ := readAll(resp)
		t.Fatalf("status = %d, want 412; body=%s", resp.StatusCode, buf)
	}
}

// TestProjects_UpstreamFailure: any other validator error is a 502.
func TestProjects_UpstreamFailure(t *testing.T) {
	ts, pool, tenantID, stub := newProjectsTestServer(t)
	stub.err = errors.New("github: 500 internal")
	devToken := seedSession(t, pool, tenantID, "developer", "init-502-"+tenantID)

	resp := postWithToken(t, ts.URL+"/v1/projects", devToken, map[string]any{
		"name":       "upstream-" + tenantID[:8],
		"owner_repo": "Silon-Oy/flow",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		buf, _ := readAll(resp)
		t.Fatalf("status = %d, want 502; body=%s", resp.StatusCode, buf)
	}
}

// TestProjects_NameUniqueness: a duplicate (tenant_id, name) returns 409,
// the architecture's chosen mapping for UNIQUE constraints.
func TestProjects_NameUniqueness(t *testing.T) {
	ts, pool, tenantID, _ := newProjectsTestServer(t)
	devToken := seedSession(t, pool, tenantID, "developer", "init-409-"+tenantID)

	body := map[string]any{
		"name":       "dup-" + tenantID[:8],
		"owner_repo": "Silon-Oy/flow",
	}
	r1 := postWithToken(t, ts.URL+"/v1/projects", devToken, body)
	r1.Body.Close()
	if r1.StatusCode != http.StatusCreated {
		t.Fatalf("first create = %d", r1.StatusCode)
	}
	r2 := postWithToken(t, ts.URL+"/v1/projects", devToken, body)
	defer r2.Body.Close()
	if r2.StatusCode != http.StatusConflict {
		buf, _ := readAll(r2)
		t.Fatalf("duplicate = %d, want 409; body=%s", r2.StatusCode, buf)
	}
}

// TestProjects_RequiresAuth proves the route really is RBAC-scoped: no
// bearer token = 401, not a 400 from JSON parsing.
func TestProjects_RequiresAuth(t *testing.T) {
	ts, _, _, _ := newProjectsTestServer(t)
	resp := postWithToken(t, ts.URL+"/v1/projects", "", map[string]any{
		"name":       "auth-check",
		"owner_repo": "Silon-Oy/flow",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		buf, _ := readAll(resp)
		t.Fatalf("no-token status = %d, want 401; body=%s", resp.StatusCode, buf)
	}
}

// TestProjects_TenantIsolation: two tenants live in the same DB, each with
// its own admin session. Creating a project under tenant B with tenant A's
// session header must NOT cross the §7 boundary.
func TestProjects_TenantIsolation(t *testing.T) {
	dsn := os.Getenv("FLOW_TEST_DSN")
	if dsn == "" {
		t.Skip("FLOW_TEST_DSN not set")
	}
	if err := store.Migrate(dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	ctx := context.Background()

	stamp := time.Now().UnixNano()
	mkTenant := func(label string) string {
		var id string
		if err := pool.QueryRow(ctx, `INSERT INTO tenant (name) VALUES ($1) RETURNING id::text`,
			fmt.Sprintf("proj-iso-%s-%d", label, stamp)).Scan(&id); err != nil {
			t.Fatalf("seed tenant %s: %v", label, err)
		}
		return id
	}
	tA := mkTenant("A")
	tB := mkTenant("B")

	stub := &stubBranchValidator{}
	srv := New(pool, tA)
	srv.BranchValidator = stub
	ts := httptest.NewServer(srv.Routes())
	t.Cleanup(ts.Close)

	tokenA := seedSession(t, pool, tA, "admin", fmt.Sprintf("iso-A-%d", stamp))
	tokenB := seedSession(t, pool, tB, "admin", fmt.Sprintf("iso-B-%d", stamp))

	create := func(tenantHeader, token, name string) int {
		var buf bytes.Buffer
		_ = json.NewEncoder(&buf).Encode(map[string]any{
			"name":       name,
			"owner_repo": "Silon-Oy/flow",
		})
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/projects", &buf)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Flow-Tenant-ID", tenantHeader)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	// Tenant A creates "proj-x" — succeeds, stamped on tenant A.
	if code := create(tA, tokenA, fmt.Sprintf("proj-x-%d", stamp)); code != http.StatusCreated {
		t.Fatalf("A create = %d", code)
	}
	// Tenant B creates "proj-x" (same name) under tenant B's header — also
	// succeeds because the UNIQUE constraint is (tenant_id, name), not name
	// alone. If the handler accidentally stamped tenant A here, the second
	// insert would 409 instead.
	if code := create(tB, tokenB, fmt.Sprintf("proj-x-%d", stamp)); code != http.StatusCreated {
		t.Fatalf("B create with same name = %d (handler may have leaked tenant A)", code)
	}

	// Cross-token: present A's session bearer with tenant B's header. The
	// session's tenant resolves to A — RequireAuth rejects the mismatch with
	// 403 before the handler runs.
	if code := create(tB, tokenA, fmt.Sprintf("cross-%d", stamp)); code == http.StatusCreated {
		t.Errorf("cross-tenant create succeeded with A token + B header — §7 violation")
	}
}

func readAll(resp *http.Response) ([]byte, error) {
	if resp.Body == nil {
		return nil, nil
	}
	var buf bytes.Buffer
	_, err := buf.ReadFrom(resp.Body)
	return bytes.TrimSpace(buf.Bytes()), err
}
