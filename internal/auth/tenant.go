// Package auth enforces the §7 tenant-isolation invariant in middleware, not
// application code. Every tenant-scoped HTTP request passes through WithTenant,
// which resolves the request's tenant via a pluggable TenantExtractor and pins
// it into the request context. Handlers then read the tenant via
// TenantFromContext and pass it into every DB call — there is no implicit
// "all tenants" fallback.
//
// Vaihe 1 stub: HeaderExtractor reads X-Flow-Tenant-ID with a bootstrap
// fallback so existing single-tenant clients keep working. Vaihe 2 replaces
// the extractor with OAuth / runner-token validation behind the same
// signature — handlers do not change.
package auth

import (
	"context"
	"net/http"
)

// ctxKey is an unexported struct type to prevent context-key collisions
// across packages (the only safe way to type a context key in Go).
type ctxKey struct{}

var tenantCtxKey = ctxKey{}

// TenantFromContext returns the tenant id pinned by WithTenant. The bool is
// false when the request was not routed through the middleware — call sites
// that depend on the middleware MUST surface that as a programmer error.
func TenantFromContext(ctx context.Context) (string, bool) {
	v, ok := ctx.Value(tenantCtxKey).(string)
	return v, ok
}

// WithTenantContext returns a copy of ctx with the given tenant id pinned.
// Useful in tests and in non-HTTP code paths (e.g. the scanner) that need to
// produce a tenant-scoped context without going through the middleware.
func WithTenantContext(ctx context.Context, tenantID string) context.Context {
	return context.WithValue(ctx, tenantCtxKey, tenantID)
}

// TenantExtractor resolves a request's tenant id. An empty string with nil
// error is rejected as unauthenticated; a non-nil error is rejected as
// unauthorized. The split lets stronger extractors (Vaihe 2) distinguish
// "no credential" from "bad credential".
type TenantExtractor func(*http.Request) (string, error)

// WithTenant wraps next so every request either has a resolved tenant in its
// context or is rejected with 401 before reaching the handler. This is the
// load-bearing tenant-isolation seam (§7).
func WithTenant(extract TenantExtractor) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			tid, err := extract(r)
			if err != nil || tid == "" {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":"tenant required"}`))
				return
			}
			next.ServeHTTP(w, r.WithContext(WithTenantContext(r.Context(), tid)))
		})
	}
}

// HeaderExtractor is the Vaihe 1 stub: read X-Flow-Tenant-ID and, when the
// header is absent, fall back to bootstrapTenantID. The fallback lets the
// existing single-tenant clients (and tests) keep working while the wire
// contract gains explicit tenancy. Vaihe 2 swaps this for OAuth / runner-token
// validation without touching handlers.
func HeaderExtractor(bootstrapTenantID string) TenantExtractor {
	return func(r *http.Request) (string, error) {
		if h := r.Header.Get("X-Flow-Tenant-ID"); h != "" {
			return h, nil
		}
		return bootstrapTenantID, nil
	}
}
