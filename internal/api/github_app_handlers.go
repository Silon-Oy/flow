package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/Silon-Oy/flow/internal/githubapp"
)

// ghAppTokenResp is the runner-facing wire shape for §7.3. expires_at is the
// raw value GitHub returned; the runner is responsible for refreshing before
// it elapses (the broker keeps a 5-min server-side cache buffer either way).
type ghAppTokenResp struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

// handleGitHubAppToken returns an installation access token for the requested
// (tenant, org). Query params (rather than headers) match the §6 declared
// surface `GET /v1/github-app/token?tenant&org`.
//
// `tenant` defaults to the bootstrap tenant when omitted — Vaihe 1 ships a
// single tenant in data, so the runner does not yet know its own tenant id.
// Vaihe 2 (issue #6) wires runner-token middleware that resolves tenant from
// the bearer; this handler will then refuse tenant overrides that disagree.
func (s *Server) handleGitHubAppToken(w http.ResponseWriter, r *http.Request) {
	if s.GHApp == nil {
		writeErr(w, http.StatusServiceUnavailable, "github-app broker not configured")
		return
	}
	tenantID := r.URL.Query().Get("tenant")
	if tenantID == "" {
		tenantID = s.TenantID
	}
	org := r.URL.Query().Get("org")
	if org == "" {
		writeErr(w, http.StatusBadRequest, "org required")
		return
	}

	ctx, cancel := withTimeout(r, 15*time.Second)
	defer cancel()

	tok, err := s.GHApp.Token(ctx, tenantID, org)
	switch {
	case err == nil:
		// fall through
	case errors.Is(err, githubapp.ErrInstallNotFound):
		writeErr(w, http.StatusNotFound, "no GitHub App installation registered for that tenant+org")
		return
	default:
		writeErr(w, http.StatusBadGateway, "mint github-app token: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, ghAppTokenResp{Token: tok.Token, ExpiresAt: tok.ExpiresAt})
}
