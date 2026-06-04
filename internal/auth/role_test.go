package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPrincipalContext_RoundTrip(t *testing.T) {
	p := Principal{TenantID: "t1", UserID: "u1", Role: RoleAdmin}
	ctx := WithPrincipalContext(context.Background(), p)
	got, ok := PrincipalFromContext(ctx)
	if !ok {
		t.Fatalf("expected principal in ctx")
	}
	if got != p {
		t.Errorf("got %+v, want %+v", got, p)
	}
}

func TestPrincipalContext_EmptyByDefault(t *testing.T) {
	if _, ok := PrincipalFromContext(context.Background()); ok {
		t.Errorf("bare context should not carry a principal")
	}
}

// TestRequireRole_AllowsAdmin asserts the happy path through the middleware:
// a principal whose role allows the capability reaches the inner handler.
func TestRequireRole_AllowsAdmin(t *testing.T) {
	reached := false
	h := RequireRole(CapRunnersManageShared)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/x", nil).WithContext(
		WithPrincipalContext(context.Background(), Principal{Role: RoleAdmin}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if !reached {
		t.Errorf("admin should reach handler for CapRunnersManageShared")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

// TestRequireRole_DeniesDeveloper proves the deny path: a developer hitting an
// admin-only capability gets 403, and the inner handler never runs.
func TestRequireRole_DeniesDeveloper(t *testing.T) {
	reached := false
	h := RequireRole(CapRunnersManageShared)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
	}))
	req := httptest.NewRequest(http.MethodGet, "/x", nil).WithContext(
		WithPrincipalContext(context.Background(), Principal{Role: RoleDeveloper}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if reached {
		t.Errorf("developer must not reach admin-only handler")
	}
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
}

// TestRequireRole_NoPrincipal proves that a request that bypassed RequireAuth
// (no principal in context) is rejected as 401 — the middleware is
// belt-and-braces and never silently grants access.
func TestRequireRole_NoPrincipal(t *testing.T) {
	reached := false
	h := RequireRole(CapRunsViewOwn)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
	}))
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if reached {
		t.Errorf("missing principal must not reach handler")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

// TestRequireRole_EveryRowEnforcedAtHTTP layers RequireRole over a synthetic
// handler and asserts that every §7 row reaches the right status code for
// each role. This complements TestRoleLimitsTable by proving the wire-level
// gate, not just the RoleAllows function.
func TestRequireRole_EveryRowEnforcedAtHTTP(t *testing.T) {
	type row struct {
		cap          Capability
		devStatus    int
		adminStatus  int
	}
	rows := []row{
		{CapProjectRegister, http.StatusOK, http.StatusOK},
		{CapRunsViewOwn, http.StatusOK, http.StatusOK},
		{CapRunsViewTenant, http.StatusForbidden, http.StatusOK},
		{CapRunnerRegisterSelf, http.StatusOK, http.StatusOK},
		{CapRunnersManageShared, http.StatusForbidden, http.StatusOK},
		{CapSecretsManage, http.StatusForbidden, http.StatusOK},
		{CapMergePolicyManage, http.StatusForbidden, http.StatusOK},
		{CapGitHubAppManage, http.StatusForbidden, http.StatusOK},
	}
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })

	for _, r := range rows {
		h := RequireRole(r.cap)(inner)
		t.Run(string(r.cap)+":developer", func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/x", nil).WithContext(
				WithPrincipalContext(context.Background(), Principal{Role: RoleDeveloper}))
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != r.devStatus {
				t.Errorf("developer %s: status = %d, want %d", r.cap, rec.Code, r.devStatus)
			}
		})
		t.Run(string(r.cap)+":admin", func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/x", nil).WithContext(
				WithPrincipalContext(context.Background(), Principal{Role: RoleAdmin}))
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != r.adminStatus {
				t.Errorf("admin %s: status = %d, want %d", r.cap, rec.Code, r.adminStatus)
			}
		})
	}
}
