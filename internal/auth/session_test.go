package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Silon-Oy/flow/internal/store"
)

func TestBearerToken(t *testing.T) {
	cases := map[string]string{
		"":                       "",
		"Bearer abc":             "abc",
		"bearer abc":             "abc",
		"BEARER  abc  ":          "abc",
		"Basic abc":              "",
		"Token abc":              "",
		"Bearer":                 "",
	}
	for in, want := range cases {
		if got := bearerToken(in); got != want {
			t.Errorf("bearerToken(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRequireAuth_RejectsMissingHeader(t *testing.T) {
	called := false
	lookup := func(ctx context.Context, h []byte) (Principal, error) {
		called = true
		return Principal{}, nil
	}
	h := RequireAuth(lookup)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("inner handler must not run on missing token")
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
	if called {
		t.Errorf("lookup must not be called when token is missing")
	}
}

func TestRequireAuth_RejectsInvalidSession(t *testing.T) {
	lookup := func(ctx context.Context, h []byte) (Principal, error) {
		return Principal{}, ErrSessionInvalid
	}
	h := RequireAuth(lookup)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("inner handler must not run on invalid session")
	}))
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Authorization", "Bearer revoked")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestRequireAuth_RejectsLookupError(t *testing.T) {
	lookup := func(ctx context.Context, h []byte) (Principal, error) {
		return Principal{}, errors.New("db down")
	}
	h := RequireAuth(lookup)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("inner handler must not run on lookup error")
	}))
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Authorization", "Bearer x")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
}

func TestRequireAuth_PinsPrincipal(t *testing.T) {
	want := Principal{TenantID: "t1", UserID: "u1", Role: RoleAdmin}
	lookup := func(ctx context.Context, h []byte) (Principal, error) { return want, nil }
	var got Principal
	var ok bool
	h := RequireAuth(lookup)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, ok = PrincipalFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/x", nil).WithContext(
		WithTenantContext(context.Background(), "t1"))
	req.Header.Set("Authorization", "Bearer good")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !ok || got != want {
		t.Errorf("pinned principal = (%+v, %v), want (%+v, true)", got, ok, want)
	}
}

// TestRequireAuth_RejectsCrossTenant exercises the cross-tenant guard: the
// session token resolves to tenant t2 but the request was tagged with tenant
// t1 by WithTenant. Either someone is impersonating or the routing is
// misconfigured — 403, not 401, because the bearer is valid but doesn't
// match.
func TestRequireAuth_RejectsCrossTenant(t *testing.T) {
	lookup := func(ctx context.Context, h []byte) (Principal, error) {
		return Principal{TenantID: "t2", UserID: "u", Role: RoleAdmin}, nil
	}
	h := RequireAuth(lookup)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("inner handler must not run on cross-tenant request")
	}))
	req := httptest.NewRequest(http.MethodGet, "/x", nil).WithContext(
		WithTenantContext(context.Background(), "t1"))
	req.Header.Set("Authorization", "Bearer x")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
}

// openSessionTestPool returns a Postgres pool against FLOW_TEST_DSN, or skips
// the test when the DSN is unset (matches the rest of the suite).
func openSessionTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("FLOW_TEST_DSN")
	if dsn == "" {
		t.Skip("FLOW_TEST_DSN not set — skipping session lookup integration test")
	}
	if err := store.Migrate(dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// TestPrincipalLookupFromPool_IntegrationSmoke is a sanity test for the SQL
// the production lookup runs: against a real DB it should hash-match a freshly
// inserted user_session, return the expected principal, and reject unknown
// tokens. Skipped without FLOW_TEST_DSN — matches the rest of the suite.
func TestPrincipalLookupFromPool_IntegrationSmoke(t *testing.T) {
	pool := openSessionTestPool(t)
	ctx := context.Background()

	tenantName := fmt.Sprintf("role-lookup-%d", testNonce())
	var tenantID, userID string
	if err := pool.QueryRow(ctx, `INSERT INTO tenant (name) VALUES ($1) RETURNING id::text`, tenantName).Scan(&tenantID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO app_user (tenant_id, github_login, role)
		VALUES ($1, $2, 'admin'::user_role) RETURNING id::text`, tenantID, "admin-x").Scan(&userID); err != nil {
		t.Fatal(err)
	}
	raw := fmt.Sprintf("test-bearer-token-%d", testNonce())
	hash := HashToken(raw)
	if _, err := pool.Exec(ctx, `
		INSERT INTO user_session (user_id, token_hash, expires_at)
		VALUES ($1, $2, now() + interval '1 hour')`, userID, hash); err != nil {
		t.Fatal(err)
	}

	lookup := PrincipalLookupFromPool(pool)

	got, err := lookup(ctx, hash)
	if err != nil {
		t.Fatalf("lookup good token: %v", err)
	}
	if got.TenantID != tenantID || got.UserID != userID || got.Role != RoleAdmin {
		t.Errorf("lookup result = %+v, want tenant=%s user=%s admin", got, tenantID, userID)
	}

	// Unknown token.
	if _, err := lookup(ctx, HashToken("never-issued")); !errors.Is(err, ErrSessionInvalid) {
		t.Errorf("unknown token: err = %v, want ErrSessionInvalid", err)
	}

	// Expired session.
	expRaw := fmt.Sprintf("expired-token-%d", testNonce())
	if _, err := pool.Exec(ctx, `
		INSERT INTO user_session (user_id, token_hash, expires_at)
		VALUES ($1, $2, now() - interval '1 second')`, userID, HashToken(expRaw)); err != nil {
		t.Fatal(err)
	}
	if _, err := lookup(ctx, HashToken(expRaw)); !errors.Is(err, ErrSessionInvalid) {
		t.Errorf("expired token: err = %v, want ErrSessionInvalid", err)
	}
}
