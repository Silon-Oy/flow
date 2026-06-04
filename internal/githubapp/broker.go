package githubapp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Silon-Oy/flow/internal/secrets"
)

// DefaultGitHubAPIBase is GitHub's public REST API root. Self-hosted GHES
// pushes through a different host; expose Broker.APIBase so a future tenant
// config can override it.
const DefaultGitHubAPIBase = "https://api.github.com"

// CacheRefreshBuffer is how early we treat a token as expired so a re-issue
// happens before any downstream caller hits a now-invalid token. GitHub
// installation tokens last ~1h; refreshing 5 min early eats one extra mint
// per hour per (tenant, org) but keeps the worst-case latency bounded.
const CacheRefreshBuffer = 5 * time.Minute

// ErrInstallNotFound means the (tenant_id, org) pair has no row in
// github_app_install. Callers map this to HTTP 404.
var ErrInstallNotFound = errors.New("githubapp: installation not registered")

// Token is what runner-facing code receives — opaque to the caller.
type Token struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

// Valid reports whether `t` is still safely usable at `now`. A nil receiver or
// a token within CacheRefreshBuffer of expiry is invalid.
func (t *Token) Valid(now time.Time) bool {
	if t == nil || t.Token == "" {
		return false
	}
	return now.Before(t.ExpiresAt.Add(-CacheRefreshBuffer))
}

// installation is the slice of github_app_install we pull per lookup. We keep
// app_id + private_key_ref alongside installation_id so a key rotation on the
// row immediately propagates to the next mint without process restart.
type installation struct {
	AppID          int64
	InstallationID int64
	PrivateKeyRef  string
}

// Broker resolves (tenant_id, org) -> installation token. Safe for concurrent
// use; cache lookups are mutex-guarded.
type Broker struct {
	pool    *pgxpool.Pool
	res     secrets.Resolver
	http    *http.Client
	apiBase string

	mu    sync.Mutex
	cache map[string]*Token
}

// NewBroker wires up a broker against pool with the env-backed secrets
// resolver. http.Client / apiBase default to sane production values; tests
// override them via WithHTTPClient / WithAPIBase.
func NewBroker(pool *pgxpool.Pool, res secrets.Resolver) *Broker {
	return &Broker{
		pool:    pool,
		res:     res,
		http:    &http.Client{Timeout: 15 * time.Second},
		apiBase: DefaultGitHubAPIBase,
		cache:   map[string]*Token{},
	}
}

// WithHTTPClient swaps the HTTP client (used by tests to inject a transport).
func (b *Broker) WithHTTPClient(c *http.Client) *Broker { b.http = c; return b }

// WithAPIBase points the broker at a non-default GitHub API root (for GHES or
// for tests against an httptest.Server).
func (b *Broker) WithAPIBase(base string) *Broker { b.apiBase = base; return b }

func cacheKey(tenantID string, installationID int64) string {
	return fmt.Sprintf("%s|%d", tenantID, installationID)
}

// Token returns a valid installation access token for (tenantID, org). It
// uses the in-process cache; on miss or near-expiry it re-mints via GitHub.
//
// On any error the caller (HTTP handler) maps the failure to a 4xx/5xx — the
// broker itself never logs the secret material, only the (tenant, org) tuple.
func (b *Broker) Token(ctx context.Context, tenantID, org string) (*Token, error) {
	if tenantID == "" {
		return nil, errors.New("githubapp: tenant id required")
	}
	if org == "" {
		return nil, errors.New("githubapp: org required")
	}

	inst, err := b.lookupInstall(ctx, tenantID, org)
	if err != nil {
		return nil, err
	}

	key := cacheKey(tenantID, inst.InstallationID)
	now := time.Now()

	b.mu.Lock()
	cached := b.cache[key]
	b.mu.Unlock()
	if cached.Valid(now) {
		return cached, nil
	}

	tok, err := b.mintInstallationToken(ctx, inst, now)
	if err != nil {
		return nil, err
	}

	b.mu.Lock()
	b.cache[key] = tok
	b.mu.Unlock()
	return tok, nil
}

// lookupInstall resolves the registry row. `UNIQUE (tenant_id, org)` makes
// this a single-row lookup.
func (b *Broker) lookupInstall(ctx context.Context, tenantID, org string) (*installation, error) {
	if b.pool == nil {
		return nil, errors.New("githubapp: nil pool")
	}
	var inst installation
	err := b.pool.QueryRow(ctx, `
		SELECT app_id, installation_id, private_key_ref
		  FROM github_app_install
		 WHERE tenant_id = $1 AND org = $2`,
		tenantID, org,
	).Scan(&inst.AppID, &inst.InstallationID, &inst.PrivateKeyRef)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrInstallNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("githubapp: lookup: %w", err)
	}
	return &inst, nil
}

// mintInstallationToken hits POST /app/installations/{id}/access_tokens with
// an App-JWT bearer. The HTTP contract matches GitHub's documented endpoint.
func (b *Broker) mintInstallationToken(ctx context.Context, inst *installation, now time.Time) (*Token, error) {
	pemBytes, err := b.res.Resolve(inst.PrivateKeyRef)
	if err != nil {
		return nil, fmt.Errorf("githubapp: resolve private key: %w", err)
	}
	key, err := ParseRSAPrivateKey(pemBytes)
	if err != nil {
		return nil, fmt.Errorf("githubapp: parse private key: %w", err)
	}
	jwt, err := NewAppJWT(inst.AppID, key, now)
	if err != nil {
		return nil, fmt.Errorf("githubapp: build JWT: %w", err)
	}

	url := fmt.Sprintf("%s/app/installations/%d/access_tokens", b.apiBase, inst.InstallationID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader([]byte("{}")))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Content-Type", "application/json")

	resp, err := b.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("githubapp: mint: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		// GitHub returns JSON like {"message":"...", "status":"401"}; surface
		// the body verbatim — it's the operator's primary diagnostic.
		return nil, fmt.Errorf("githubapp: mint: HTTP %d: %s", resp.StatusCode, bytes.TrimSpace(body))
	}
	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("githubapp: decode mint response: %w", err)
	}
	if out.Token == "" || out.ExpiresAt.IsZero() {
		return nil, errors.New("githubapp: mint: empty token or expires_at")
	}
	return &Token{Token: out.Token, ExpiresAt: out.ExpiresAt}, nil
}
