package auth

import "net/http"

// RequireRole rejects requests whose Principal lacks cap. It MUST sit downstream
// of [[RequireAuth]] so PrincipalFromContext is populated; an unauthenticated
// request hits 401 here (RequireAuth would already have rejected it, but the
// belt-and-braces check keeps the failure mode the same if the middlewares are
// ever reordered).
func RequireRole(cap Capability) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, ok := PrincipalFromContext(r.Context())
			if !ok {
				writeAuthErr(w, http.StatusUnauthorized, "unauthenticated")
				return
			}
			if !RoleAllows(p.Role, cap) {
				writeAuthErr(w, http.StatusForbidden, "role lacks capability")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
