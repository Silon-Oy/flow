// Package centralclient is the HTTP client the runner and CLI use to talk to
// flowd. It mirrors the §6 REST surface and is the single place that knows the
// wire shapes, so the runner/CLI never hand-roll requests.
package centralclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/Silon-Oy/flow/internal/lease"
	"github.com/Silon-Oy/flow/internal/runstate"
)

// Client talks to a flowd base URL with an optional runner token.
type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
}

// New returns a Client with a sane default HTTP client.
func New(baseURL, token string) *Client {
	return &Client{
		BaseURL: baseURL,
		Token:   token,
		HTTP:    &http.Client{Timeout: 30 * time.Second},
	}
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) (int, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, rdr)
	if err != nil {
		return 0, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		msg, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, fmt.Errorf("%s %s: %d: %s", method, path, resp.StatusCode, bytes.TrimSpace(msg))
	}
	if out != nil && resp.StatusCode != http.StatusNoContent {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil && err != io.EOF {
			return resp.StatusCode, err
		}
	}
	return resp.StatusCode, nil
}

// RegisterRunner registers this host and returns the runner id + token.
func (c *Client) RegisterRunner(ctx context.Context, hostname string, capacity int) (string, string, error) {
	var out struct {
		RunnerID    string `json:"runner_id"`
		RunnerToken string `json:"runner_token"`
	}
	_, err := c.do(ctx, http.MethodPost, "/v1/runners/register",
		map[string]any{"hostname": hostname, "capacity": capacity}, &out)
	return out.RunnerID, out.RunnerToken, err
}

// RunnerHeartbeat refreshes the runner's liveness.
func (c *Client) RunnerHeartbeat(ctx context.Context, runnerID string) error {
	_, err := c.do(ctx, http.MethodPost, "/v1/runners/"+runnerID+"/heartbeat", nil, nil)
	return err
}

// AcquireResult carries the acquire outcome. Acquired is false when the queue
// is empty (204) — the runner backs off; it is NOT an error.
type AcquireResult struct {
	Acquired bool
	Lease    *lease.Lease
	Work     *lease.Work
}

// Acquire attempts to claim work. A 204 (empty queue) returns Acquired=false,
// nil error. A transport/DB error propagates so the runner fails closed.
func (c *Client) Acquire(ctx context.Context, runnerID string, kinds []string) (AcquireResult, error) {
	var out struct {
		Lease *lease.Lease `json:"lease"`
		Work  *lease.Work  `json:"work"`
	}
	code, err := c.do(ctx, http.MethodPost, "/v1/leases/acquire",
		map[string]any{"runner_id": runnerID, "kinds": kinds}, &out)
	if err != nil {
		return AcquireResult{}, err
	}
	if code == http.StatusNoContent {
		return AcquireResult{Acquired: false}, nil
	}
	return AcquireResult{Acquired: true, Lease: out.Lease, Work: out.Work}, nil
}

// LeaseHeartbeat extends a lease. Returns an error (HTTP 409) if the lease is no
// longer active — the runner must abort the run.
func (c *Client) LeaseHeartbeat(ctx context.Context, leaseID string) error {
	_, err := c.do(ctx, http.MethodPost, "/v1/leases/"+leaseID+"/heartbeat", nil, nil)
	return err
}

// LeaseRelease releases a lease.
func (c *Client) LeaseRelease(ctx context.Context, leaseID string) error {
	_, err := c.do(ctx, http.MethodPost, "/v1/leases/"+leaseID+"/release", nil, nil)
	return err
}

// CreateRun opens a run record and returns its id.
func (c *Client) CreateRun(ctx context.Context, projectID, remote string, issueNumber int) (string, error) {
	var out struct {
		RunID string `json:"run_id"`
	}
	_, err := c.do(ctx, http.MethodPost, "/v1/runs",
		map[string]any{"project_id": projectID, "remote": remote, "issue_number": issueNumber}, &out)
	return out.RunID, err
}

// PatchRun applies a telemetry patch.
func (c *Client) PatchRun(ctx context.Context, runID string, patch map[string]any) error {
	_, err := c.do(ctx, http.MethodPatch, "/v1/runs/"+runID, patch, nil)
	return err
}

// GetRun loads the full run record by id. The in-container orchestrator uses
// this to resolve its per-run config (remote, issue number, branch) without
// re-deriving it from the host context — the container only carries the run-id
// (and the central URL + runner token) across the §11.1 trust boundary.
func (c *Client) GetRun(ctx context.Context, runID string) (*runstate.Run, error) {
	var r runstate.Run
	if _, err := c.do(ctx, http.MethodGet, "/v1/runs/"+runID, nil, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// AppendEvents pushes a batch of run events.
func (c *Client) AppendEvents(ctx context.Context, runID string, events []runstate.Event) error {
	_, err := c.do(ctx, http.MethodPost, "/v1/runs/"+runID+"/events",
		map[string]any{"events": events}, nil)
	return err
}

// EgressEntry is the wire shape POST /v1/egress accepts. It mirrors
// egresship.Entry but lives here so the runner can call ShipEgress without
// pulling the api/egresship packages into a tight import loop.
type EgressEntry struct {
	Host    string    `json:"host"`
	Allowed bool      `json:"allowed"`
	TS      time.Time `json:"ts"`
}

// ShipEgress posts a batch of squid access-log entries to flowd. The central
// stamps tenant_id; lease/run linkage is left for a later phase (squid does
// not know which lease originated a request — §11.6 minimum-acceptable).
func (c *Client) ShipEgress(ctx context.Context, entries []EgressEntry) error {
	if len(entries) == 0 {
		return nil
	}
	_, err := c.do(ctx, http.MethodPost, "/v1/egress",
		map[string]any{"entries": entries}, nil)
	return err
}

// RunView is the subset of a run the CLI status command shows.
type RunView struct {
	ID           string  `json:"id"`
	ProjectID    string  `json:"project_id"`
	Remote       string  `json:"remote"`
	IssueNumber  int     `json:"issue_number"`
	Status       string  `json:"status"`
	CurrentState *string `json:"current_state"`
	Branch       *string `json:"branch"`
	PRURL        *string `json:"pr_url"`
}

// ListRuns returns runs, optionally filtered by status.
func (c *Client) ListRuns(ctx context.Context, status string) ([]RunView, error) {
	var out struct {
		Runs []RunView `json:"runs"`
	}
	path := "/v1/runs"
	if status != "" {
		path += "?status=" + status
	}
	_, err := c.do(ctx, http.MethodGet, path, nil, &out)
	return out.Runs, err
}

// RunnerView is the subset of a runner the CLI status command shows.
type RunnerView struct {
	ID            string     `json:"id"`
	Hostname      string     `json:"hostname"`
	Capacity      int        `json:"capacity"`
	ActiveLeases  int        `json:"active_leases"`
	LastHeartbeat *time.Time `json:"last_heartbeat"`
	Status        string     `json:"status"`
}

// ListRunners returns the registered runners.
func (c *Client) ListRunners(ctx context.Context) ([]RunnerView, error) {
	var out struct {
		Runners []RunnerView `json:"runners"`
	}
	_, err := c.do(ctx, http.MethodGet, "/v1/runners", nil, &out)
	return out.Runners, err
}

// --- §7(a) human auth — GitHub OAuth device flow ----------------------------

// DeviceStart mirrors the central's POST /v1/auth/device/start response. The
// CLI shows VerificationURI + UserCode to the user and polls every Interval.
type DeviceStart struct {
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationURI string `json:"verification_uri"`
	ExpiresIn       int    `json:"expires_in"`
	Interval        int    `json:"interval"`
}

// StartDeviceLogin asks the central to begin a device-flow login. The returned
// DeviceCode must be passed back to PollDeviceLogin; GitHub already binds it to
// the user_code shown in the browser.
func (c *Client) StartDeviceLogin(ctx context.Context) (*DeviceStart, error) {
	var out DeviceStart
	if _, err := c.do(ctx, http.MethodPost, "/v1/auth/device/start", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DevicePoll mirrors the central's POST /v1/auth/device/poll response. Exactly
// one of (Pending, SessionToken) is meaningful per call.
type DevicePoll struct {
	Pending      bool      `json:"pending"`
	SessionToken string    `json:"session_token,omitempty"`
	GitHubLogin  string    `json:"github_login,omitempty"`
	ExpiresAt    time.Time `json:"expires_at,omitempty"`
}

// PollDeviceLogin polls the central once. Pending=true means "keep polling".
// SessionToken!="" means success — the CLI writes it to the credentials file.
func (c *Client) PollDeviceLogin(ctx context.Context, deviceCode string) (*DevicePoll, error) {
	var out DevicePoll
	if _, err := c.do(ctx, http.MethodPost, "/v1/auth/device/poll",
		map[string]string{"device_code": deviceCode}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// --- §8 PROJECT wizard -----------------------------------------------------

// ProjectRemote mirrors the wire shape POST /v1/projects expects on each
// remotes[] entry. base_branch is optional per päätös 14 (per-remote override
// of PROJECT.base_branch).
type ProjectRemote struct {
	Remote     string `json:"remote"`
	OwnerRepo  string `json:"owner_repo"`
	BaseBranch string `json:"base_branch,omitempty"`
}

// CreateProjectRequest is what the CLI sends to POST /v1/projects. Optional
// fields are omitempty so the wizard can leave them at server-side defaults
// (labels=["auto-run"], base_branch="main").
type CreateProjectRequest struct {
	Name                 string            `json:"name"`
	OwnerRepo            string            `json:"owner_repo"`
	Remotes              []ProjectRemote   `json:"remotes,omitempty"`
	Labels               []string          `json:"labels,omitempty"`
	BaseBranch           string            `json:"base_branch,omitempty"`
	RunnerPool           string            `json:"runner_pool,omitempty"`
	ClaudeTimeoutSeconds int               `json:"claude_timeout_seconds,omitempty"`
	MergePolicy          map[string]any    `json:"merge_policy,omitempty"`
	SecretRefs           map[string]string `json:"secret_refs,omitempty"`
}

// CreateProject calls POST /v1/projects and returns the new project id. The
// central performs the §8 validation (regex, App-install, branch existence);
// the CLI uses any HTTP-level error message verbatim — it's the diagnostic.
func (c *Client) CreateProject(ctx context.Context, req CreateProjectRequest) (string, error) {
	var out struct {
		ProjectID string `json:"project_id"`
	}
	if _, err := c.do(ctx, http.MethodPost, "/v1/projects", req, &out); err != nil {
		return "", err
	}
	return out.ProjectID, nil
}

// --- §7.3 GitHub App token broker ------------------------------------------

// GitHubAppToken is what the central returns from /v1/github-app/token. The
// runner uses Token as the bearer for git/`gh`/REST against the org, and
// re-fetches once ExpiresAt is near (GitHub installation tokens last ~1h).
type GitHubAppToken struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

// MintGitHubAppToken asks the central for an installation token for `org`.
// `tenant` may be empty in Vaihe 1 — the central then resolves to the
// bootstrap tenant (Vaihe 2 will require the runner-token to disambiguate).
func (c *Client) MintGitHubAppToken(ctx context.Context, tenant, org string) (*GitHubAppToken, error) {
	if org == "" {
		return nil, fmt.Errorf("centralclient: org required")
	}
	q := url.Values{}
	if tenant != "" {
		q.Set("tenant", tenant)
	}
	q.Set("org", org)
	var out GitHubAppToken
	if _, err := c.do(ctx, http.MethodGet, "/v1/github-app/token?"+q.Encode(), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
