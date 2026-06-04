package api

import (
	"crypto/subtle"
	"errors"
	"log"
	"net/http"
	"strings"
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

// authorizeBroker enforces the §7.3 endpoint's pre-shared bearer. Issue #6
// landed per-runner tokens for the runner-write endpoints, but this broker
// endpoint still rides the shared FLOW_BROKER_TOKEN; folding it onto runner
// tokens is a follow-up. Behaviour:
//
//   - BrokerToken empty  → 503 (broker not configured; fail-closed).
//   - Missing / malformed Authorization header → 401.
//   - Token mismatch → 401 (constant-time compare so a network attacker
//     cannot side-channel byte-by-byte).
//
// Returns true when the caller is authorized; the handler then proceeds.
func (s *Server) authorizeBroker(w http.ResponseWriter, r *http.Request) bool {
	if s.BrokerToken == "" {
		writeErr(w, http.StatusServiceUnavailable,
			"github-app broker disabled: FLOW_BROKER_TOKEN not set on the central")
		return false
	}
	hdr := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if !strings.HasPrefix(hdr, prefix) {
		writeErr(w, http.StatusUnauthorized, "missing bearer token")
		return false
	}
	got := hdr[len(prefix):]
	if subtle.ConstantTimeCompare([]byte(got), []byte(s.BrokerToken)) != 1 {
		writeErr(w, http.StatusUnauthorized, "invalid bearer token")
		return false
	}
	return true
}

// handleGitHubAppToken returns an installation access token for the requested
// (tenant, org). Query params (rather than headers) match the §6 declared
// surface `GET /v1/github-app/token?tenant&org`.
//
// `tenant` defaults to the bootstrap tenant when omitted — Vaihe 1 ships a
// single tenant in data. A follow-up will resolve the tenant from the runner
// token (issue #6 added that token to the runner-write endpoints) and refuse
// tenant overrides that disagree with the authenticated identity.
func (s *Server) handleGitHubAppToken(w http.ResponseWriter, r *http.Request) {
	if s.GHApp == nil {
		writeErr(w, http.StatusServiceUnavailable, "github-app broker not configured")
		return
	}
	if !s.authorizeBroker(w, r) {
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
		// Keep upstream details server-side: the GitHub error body may carry
		// rate-limit headers or org names we should not echo to the runner.
		log.Printf("github-app mint failed tenant=%s org=%s: %v", tenantID, org, err)
		writeErr(w, http.StatusBadGateway, "failed to mint github-app token")
		return
	}
	writeJSON(w, http.StatusOK, ghAppTokenResp{Token: tok.Token, ExpiresAt: tok.ExpiresAt})
}
