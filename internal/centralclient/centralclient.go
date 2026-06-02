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
