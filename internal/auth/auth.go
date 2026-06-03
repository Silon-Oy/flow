// Package auth implements §7(a) — human → central authentication via the
// GitHub OAuth Device Flow (RFC 8628). The central is the OAuth client: a raw
// GitHub access token never leaves flowd. flowctl exchanges a one-shot device
// code for an opaque flow session token whose SHA-256 hash lives in
// user_session.
//
// The flow:
//
//  1. flowctl → POST /v1/auth/device/start → Service.StartDeviceLogin
//     central → POST github.com/login/device/code (client_id + scope=read:user)
//     central → returns {user_code, verification_uri, device_code, interval}
//
//  2. user opens verification_uri in a browser and types user_code
//
//  3. flowctl polls POST /v1/auth/device/poll {device_code} on `interval`
//     central → POST github.com/login/oauth/access_token (device_code grant)
//        - 401-equivalent payload {error:"authorization_pending"|"slow_down"}
//          → pending=true; flowctl waits and retries
//        - {access_token:"gho_..."} → central calls api.github.com/user with it,
//          upserts app_user, mints a flow session token, persists its hash,
//          discards the GitHub token, and returns the flow session token.
//
// The GitHub access token never persists. The flow session token is opaque
// (random bytes, hex) — there is no JWT machinery on a self-hosted Tailscale
// deploy, and revocation is a single DELETE.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// DefaultSessionTTL is how long a freshly minted flow session token is valid.
// 30 days mirrors a typical "stay signed in" window without inviting indefinite
// credential drift; admins can shorten by deleting user_session rows.
const DefaultSessionTTL = 30 * 24 * time.Hour

// GitHubBaseURL / GitHubAPIBaseURL are the live endpoints; tests inject httptest
// servers via Service.GitHubBaseURL / Service.GitHubAPIBaseURL.
const (
	GitHubBaseURL    = "https://github.com"
	GitHubAPIBaseURL = "https://api.github.com"
	deviceScope      = "read:user"
)

// ErrAuthorizationPending is the GitHub-defined "user hasn't entered the code
// yet" signal. Surfaced to the handler so /v1/auth/device/poll can return a
// `pending` flag rather than a hard error.
var ErrAuthorizationPending = errors.New("authorization pending")

// ErrSlowDown asks the client to back off polling.
var ErrSlowDown = errors.New("slow down")

// ErrAccessDenied means the user clicked "cancel" on the GitHub page.
var ErrAccessDenied = errors.New("access denied")

// ErrExpiredToken means the device_code is too old; the client must restart.
var ErrExpiredToken = errors.New("expired device code")

// Service is the auth surface flowd exposes to the API layer. It owns the
// Postgres pool, the GitHub OAuth client_id, and the GitHub HTTP endpoints
// (overridable in tests). All public methods are safe for concurrent use.
type Service struct {
	Pool             *pgxpool.Pool
	TenantID         string
	ClientID         string
	GitHubBaseURL    string
	GitHubAPIBaseURL string
	HTTP             *http.Client
	SessionTTL       time.Duration
	DefaultRole      string
	Now              func() time.Time
}

// New returns a Service with default endpoints and a 30 s HTTP timeout — GitHub
// device-code endpoints answer fast; if they don't we want to fail closed.
func New(pool *pgxpool.Pool, tenantID, clientID string) *Service {
	return &Service{
		Pool:             pool,
		TenantID:         tenantID,
		ClientID:         clientID,
		GitHubBaseURL:    GitHubBaseURL,
		GitHubAPIBaseURL: GitHubAPIBaseURL,
		HTTP:             &http.Client{Timeout: 30 * time.Second},
		SessionTTL:       DefaultSessionTTL,
		DefaultRole:      "developer",
		Now:              time.Now,
	}
}

// DeviceStart is the user-visible payload of POST /v1/auth/device/start. The
// device_code is returned to the client so the poll endpoint stays stateless on
// the central side — GitHub already tracks the device_code → user_code mapping;
// duplicating that in our DB would be redundant and an extra failure mode.
type DeviceStart struct {
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationURI string `json:"verification_uri"`
	ExpiresIn       int    `json:"expires_in"`
	Interval        int    `json:"interval"`
}

// StartDeviceLogin asks GitHub for a fresh device + user code pair.
func (s *Service) StartDeviceLogin(ctx context.Context) (*DeviceStart, error) {
	if s.ClientID == "" {
		return nil, errors.New("auth: FLOW_GITHUB_OAUTH_CLIENT_ID is not configured")
	}
	form := url.Values{
		"client_id": {s.ClientID},
		"scope":     {deviceScope},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		s.GitHubBaseURL+"/login/device/code", strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := s.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("device/code: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("device/code: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out struct {
		DeviceCode      string `json:"device_code"`
		UserCode        string `json:"user_code"`
		VerificationURI string `json:"verification_uri"`
		ExpiresIn       int    `json:"expires_in"`
		Interval        int    `json:"interval"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("device/code decode: %w", err)
	}
	if out.DeviceCode == "" || out.UserCode == "" {
		return nil, errors.New("device/code: empty device or user code")
	}
	if out.Interval <= 0 {
		out.Interval = 5 // GitHub's documented default poll interval.
	}
	return &DeviceStart{
		DeviceCode:      out.DeviceCode,
		UserCode:        out.UserCode,
		VerificationURI: out.VerificationURI,
		ExpiresIn:       out.ExpiresIn,
		Interval:        out.Interval,
	}, nil
}

// PollResult is the user-visible payload of POST /v1/auth/device/poll. Exactly
// one of (Pending, SessionToken) is meaningful per call.
type PollResult struct {
	// Pending=true means GitHub said "authorization_pending"; the client keeps
	// polling. SessionToken is empty in this case.
	Pending      bool
	SessionToken string
	GitHubLogin  string
	ExpiresAt    time.Time
}

// PollDeviceLogin exchanges device_code for a GitHub access token (if the user
// has authorised). On success it upserts the app_user, mints a flow session
// token, persists its hash, and returns the raw token to the caller.
func (s *Service) PollDeviceLogin(ctx context.Context, deviceCode string) (*PollResult, error) {
	if s.ClientID == "" {
		return nil, errors.New("auth: FLOW_GITHUB_OAUTH_CLIENT_ID is not configured")
	}
	if deviceCode == "" {
		return nil, errors.New("auth: device_code required")
	}
	form := url.Values{
		"client_id":   {s.ClientID},
		"device_code": {deviceCode},
		"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		s.GitHubBaseURL+"/login/oauth/access_token", strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := s.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("access_token: %w", err)
	}
	defer resp.Body.Close()
	// GitHub returns 200 OK even for "authorization_pending"; the payload's
	// `error` field is the actual signal.
	var tok struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		Scope       string `json:"scope"`
		Error       string `json:"error"`
		ErrorDesc   string `json:"error_description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tok); err != nil {
		return nil, fmt.Errorf("access_token decode: %w", err)
	}
	switch tok.Error {
	case "":
		// success — fall through
	case "authorization_pending":
		return &PollResult{Pending: true}, nil
	case "slow_down":
		return nil, ErrSlowDown
	case "access_denied":
		return nil, ErrAccessDenied
	case "expired_token":
		return nil, ErrExpiredToken
	default:
		return nil, fmt.Errorf("access_token: %s: %s", tok.Error, tok.ErrorDesc)
	}
	if tok.AccessToken == "" {
		return nil, errors.New("access_token: empty token in success response")
	}

	login, err := s.fetchGitHubLogin(ctx, tok.AccessToken)
	if err != nil {
		return nil, err
	}

	userID, err := s.upsertUser(ctx, login)
	if err != nil {
		return nil, fmt.Errorf("upsert user: %w", err)
	}

	sessionToken, hash, err := mintSessionToken()
	if err != nil {
		return nil, fmt.Errorf("mint session: %w", err)
	}
	expiresAt := s.now().Add(s.sessionTTL())
	if _, err := s.Pool.Exec(ctx, `
		INSERT INTO user_session (user_id, token_hash, expires_at)
		VALUES ($1, $2, $3)`, userID, hash, expiresAt); err != nil {
		return nil, fmt.Errorf("insert session: %w", err)
	}
	return &PollResult{
		SessionToken: sessionToken,
		GitHubLogin:  login,
		ExpiresAt:    expiresAt,
	}, nil
}

func (s *Service) fetchGitHubLogin(ctx context.Context, accessToken string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.GitHubAPIBaseURL+"/user", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := s.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("api/user: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("api/user: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var u struct {
		Login string `json:"login"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&u); err != nil {
		return "", fmt.Errorf("api/user decode: %w", err)
	}
	if u.Login == "" {
		return "", errors.New("api/user: empty login")
	}
	return u.Login, nil
}

// upsertUser inserts the bootstrap-tenant app_user row for this GitHub login
// (developer role by default — RBAC escalation is admin-side, issue #7) and
// returns the user id. A returning user keeps the role they already have.
func (s *Service) upsertUser(ctx context.Context, login string) (string, error) {
	var id string
	err := s.Pool.QueryRow(ctx, `
		INSERT INTO app_user (tenant_id, github_login, role)
		VALUES ($1, $2, $3::user_role)
		ON CONFLICT (tenant_id, github_login)
		DO UPDATE SET github_login = EXCLUDED.github_login
		RETURNING id::text`, s.TenantID, login, s.defaultRole()).Scan(&id)
	return id, err
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Service) sessionTTL() time.Duration {
	if s.SessionTTL > 0 {
		return s.SessionTTL
	}
	return DefaultSessionTTL
}

func (s *Service) defaultRole() string {
	if s.DefaultRole != "" {
		return s.DefaultRole
	}
	return "developer"
}

// mintSessionToken returns (rawToken, sha256(rawToken)). The raw token is only
// returned to the caller once — afterwards we only ever have the hash. 32 bytes
// of CSPRNG entropy mirrors the runner-token shape (handlers.randomToken).
func mintSessionToken() (string, []byte, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", nil, err
	}
	raw := hex.EncodeToString(b)
	sum := sha256.Sum256([]byte(raw))
	return raw, sum[:], nil
}

// HashToken is the canonical SHA-256 of a raw session token. Exposed so future
// middleware (issue #4) can look up sessions by hashing the bearer token from
// the Authorization header.
func HashToken(raw string) []byte {
	sum := sha256.Sum256([]byte(raw))
	return sum[:]
}
