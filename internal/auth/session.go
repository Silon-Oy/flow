// Session-token middleware (§7(a)). RequireAuth turns a raw bearer token in
// the Authorization header into a [[Principal]] in the request context, by
// SHA-256-hashing it and joining user_session × app_user. It MUST run after
// [[WithTenant]] so the cross-tenant check has the requested tenant available.
//
// Layering in the API mux:
//
//	WithTenant(extractor) → RequireAuth(pool) → RequireRole(cap) → handler
//
// Every step is fail-closed: missing/expired tokens 401, cross-tenant 403,
// DB error 503 — never silently grant access.
package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// PrincipalLookup resolves a hashed session token into a Principal. Stubbed
// in tests, satisfied by [[PrincipalLookupFromPool]] in production.
type PrincipalLookup func(ctx context.Context, tokenHash []byte) (Principal, error)

// ErrSessionInvalid signals that no live user_session row matched the token
// hash (expired, revoked, or never issued). The middleware maps this to 401.
var ErrSessionInvalid = errors.New("auth: session invalid")

// pgxQuerier is the minimal interface RequireAuth needs from a pgxpool.Pool —
// kept narrow so tests can stub it.
type pgxQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// PrincipalLookupFromPool returns a PrincipalLookup that joins user_session
// with app_user and refuses expired sessions. last_used_at is bumped in the
// same statement so admins can spot stale sessions in user_session without a
// second round-trip; if the bump fails it fails the whole call (rare, and
// safer than silently granting access while telemetry rots).
func PrincipalLookupFromPool(pool pgxQuerier) PrincipalLookup {
	return func(ctx context.Context, tokenHash []byte) (Principal, error) {
		var p Principal
		var roleStr string
		err := pool.QueryRow(ctx, `
			WITH bumped AS (
			    UPDATE user_session
			       SET last_used_at = now()
			     WHERE token_hash = $1
			       AND expires_at > now()
			    RETURNING user_id
			)
			SELECT u.tenant_id::text, u.id::text, u.role::text
			  FROM bumped b
			  JOIN app_user u ON u.id = b.user_id
		`, tokenHash).Scan(&p.TenantID, &p.UserID, &roleStr)
		if errors.Is(err, pgx.ErrNoRows) {
			return Principal{}, ErrSessionInvalid
		}
		if err != nil {
			return Principal{}, err
		}
		p.Role = Role(roleStr)
		return p, nil
	}
}

// RequireAuth wraps next so every request either carries a resolved Principal
// in its context or is rejected with 401 (no token / bad token) or 403
// (token resolves to a tenant other than the one the request targeted).
//
// The order matters: WithTenant must have run first so the requested tenant
// is available for the cross-tenant check. Handlers downstream read the
// principal via [[PrincipalFromContext]].
func RequireAuth(lookup PrincipalLookup) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw := bearerToken(r.Header.Get("Authorization"))
			if raw == "" {
				writeAuthErr(w, http.StatusUnauthorized, "missing bearer token")
				return
			}
			ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
			defer cancel()
			p, err := lookup(ctx, HashToken(raw))
			if errors.Is(err, ErrSessionInvalid) {
				writeAuthErr(w, http.StatusUnauthorized, "session invalid")
				return
			}
			if err != nil {
				writeAuthErr(w, http.StatusServiceUnavailable, "auth lookup failed")
				return
			}
			// Cross-tenant guard: the resolved principal's tenant MUST match the
			// tenant the request claimed via [[WithTenant]]. Mismatch is a probe,
			// not a typo — 403, not 401.
			if reqTenant, ok := TenantFromContext(r.Context()); ok && reqTenant != "" && reqTenant != p.TenantID {
				writeAuthErr(w, http.StatusForbidden, "cross-tenant request")
				return
			}
			next.ServeHTTP(w, r.WithContext(WithPrincipalContext(r.Context(), p)))
		})
	}
}

// bearerToken returns the token portion of an `Authorization: Bearer <tok>`
// header, or "" if the header is absent / malformed. Case-insensitive on the
// scheme; whitespace-trimmed on the token.
func bearerToken(h string) string {
	if h == "" {
		return ""
	}
	const scheme = "bearer "
	if len(h) < len(scheme) {
		return ""
	}
	if !strings.EqualFold(h[:len(scheme)], scheme) {
		return ""
	}
	return strings.TrimSpace(h[len(scheme):])
}

func writeAuthErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write([]byte(`{"error":"` + msg + `"}`))
}
