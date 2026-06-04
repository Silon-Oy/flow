package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Silon-Oy/flow/internal/githubapp"
	"github.com/Silon-Oy/flow/internal/secrets"
)

// TestBrokerAuthGate exercises the §7.3 bearer gate without touching the DB
// or the broker. It runs without FLOW_TEST_DSN, so CI catches an accidentally
// unauthenticated endpoint regardless of DB availability.
func TestBrokerAuthGate(t *testing.T) {
	cases := []struct {
		name        string
		brokerToken string
		auth        string
		wantCode    int
	}{
		{"empty BrokerToken → 503", "", "Bearer anything", http.StatusServiceUnavailable},
		{"empty BrokerToken no header → 503", "", "", http.StatusServiceUnavailable},
		{"missing bearer → 401", "secret", "", http.StatusUnauthorized},
		{"malformed scheme → 401", "secret", "Basic Zm9vOmJhcg==", http.StatusUnauthorized},
		{"wrong token → 401", "secret", "Bearer nope", http.StatusUnauthorized},
		// The "right token" case proceeds past auth and hits the 400 (missing
		// org). 400 confirms auth passed without needing the DB / broker.
		{"right token, no org → 400", "secret", "Bearer secret", http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{
				BrokerToken: tc.brokerToken,
				TenantID:    "00000000-0000-0000-0000-000000000000",
				hub:         newLogHub(),
			}
			// The handler checks GHApp != nil first; auth-gate tests need it
			// set so we exercise the auth branch (not the GHApp-nil 503).
			// Pool=nil is safe because the handler returns 400 on missing
			// org *before* any broker method is invoked.
			s.GHApp = githubapp.NewBroker(nil, secrets.EnvResolver{})
			req := httptest.NewRequest(http.MethodGet, "/v1/github-app/token", nil)
			if tc.auth != "" {
				req.Header.Set("Authorization", tc.auth)
			}
			rec := httptest.NewRecorder()
			s.handleGitHubAppToken(rec, req)
			if rec.Code != tc.wantCode {
				t.Errorf("status %d, want %d (body: %s)", rec.Code, tc.wantCode, rec.Body.String())
			}
		})
	}
}
