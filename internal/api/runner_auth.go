package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Silon-Oy/flow/internal/auth"
)

// errRunnerTokenRequired / errRunnerTokenInvalid are surfaced to WithTenant,
// which maps any non-nil error (and an empty tenant) to 401. We keep them
// distinct for readability/debugging; both fail closed — no valid runner
// identity, no runner-write access.
var (
	errRunnerTokenRequired = errors.New("runner token required")
	errRunnerTokenInvalid  = errors.New("invalid runner token")
)

// runnerTokenExtractor is the §7(b) TenantExtractor for runner-write endpoints.
// It authenticates a request by its `Authorization: Bearer <runner-token>`
// header: the token is hashed and the owning runner row is looked up, and the
// runner's tenant is returned to be pinned into the request context (so the
// existing tenant-filtered handlers keep working unchanged).
//
// Scope enforcement is structural: this extractor is wired ONLY to the
// runner-write routes in Routes(). A runner token therefore confers no access
// to CLI/dashboard (read) endpoints — those run through the header extractor,
// and their proper user-session enforcement is issue #7.
//
// The token is never compared byte-by-byte in Go: we match SHA-256 digests
// inside the unique-index lookup (auth.HashToken), the same model
// user_session uses, so a timing side channel could leak at most a hash, never
// the token itself.
func (s *Server) runnerTokenExtractor(r *http.Request) (string, error) {
	const prefix = "Bearer "
	hdr := r.Header.Get("Authorization")
	if !strings.HasPrefix(hdr, prefix) {
		return "", errRunnerTokenRequired
	}
	raw := strings.TrimSpace(hdr[len(prefix):])
	if raw == "" {
		return "", errRunnerTokenRequired
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	var tenantID string
	err := s.Pool.QueryRow(ctx,
		`SELECT tenant_id::text FROM runner WHERE token_hash = $1`,
		auth.HashToken(raw)).Scan(&tenantID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", errRunnerTokenInvalid
	}
	if err != nil {
		// DB error during auth => fail-closed: reject (WithTenant -> 401). We do
		// not hand out runner-write access when we cannot verify the identity.
		return "", err
	}
	return tenantID, nil
}
