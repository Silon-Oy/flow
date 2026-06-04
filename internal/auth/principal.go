package auth

import "context"

// Role is the authenticated user's role within a tenant (§7). The set is
// closed; new roles require a schema change (user_role enum) and a
// capability-registry update.
type Role string

const (
	RoleAdmin     Role = "admin"
	RoleDeveloper Role = "developer"
)

// Principal is the resolved identity behind a request: the tenant, the user
// inside that tenant, and the user's role. RequireAuth pins it into the
// request context; RequireRole and tenant-scoped handlers read it.
//
// A request without a Principal is unauthenticated — handlers that need a
// user-bound identity (e.g. "developer sees own runs") MUST gate on
// PrincipalFromContext and refuse the call when ok is false.
type Principal struct {
	TenantID string
	UserID   string
	Role     Role
}

type principalCtxKeyT struct{}

var principalCtxKey = principalCtxKeyT{}

// PrincipalFromContext returns the Principal pinned by RequireAuth, or
// (zero, false) if the request did not flow through it.
func PrincipalFromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalCtxKey).(Principal)
	return p, ok
}

// WithPrincipalContext pins p into ctx. Exposed for tests and for non-HTTP
// code paths that need a principal-scoped context without the middleware.
func WithPrincipalContext(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalCtxKey, p)
}
