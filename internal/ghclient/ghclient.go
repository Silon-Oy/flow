// Package ghclient is the central service's GitHub read client (R6: go-github
// for the scanner). It is the ONLY component that polls GitHub for issues —
// centralizing the rate limit (§4). The runner's git side-effect operations use
// gh-shell-out instead and live in internal/orchestrator.
package ghclient

import (
	"context"

	"github.com/google/go-github/v66/github"
)

// IssueRef is the minimal issue identity the scanner enqueues.
type IssueRef struct {
	Number int
	Title  string
}

// Client wraps a go-github client scoped to read issues.
type Client struct {
	gh *github.Client
}

// New returns a Client. token may be empty for unauthenticated (rate-limited)
// access; in Vaihe 2 the per-tenant App token broker supplies a scoped token.
func New(token string) *Client {
	c := github.NewClient(nil)
	if token != "" {
		c = c.WithAuthToken(token)
	}
	return &Client{gh: c}
}

// ListOpenLabeledIssues returns open, UNASSIGNED issues on owner/repo carrying
// the given label, oldest first. Unassigned mirrors the bash
// pick_oldest_unassigned filter; the lease — not assignment — is now the
// arbiter, but skipping assigned issues avoids re-enqueuing human-claimed work.
func (c *Client) ListOpenLabeledIssues(ctx context.Context, owner, repo, label string) ([]IssueRef, error) {
	opt := &github.IssueListByRepoOptions{
		State:     "open",
		Labels:    []string{label},
		Assignee:  "none",
		Sort:      "created",
		Direction: "asc",
		ListOptions: github.ListOptions{PerPage: 50},
	}
	var out []IssueRef
	for {
		issues, resp, err := c.gh.Issues.ListByRepo(ctx, owner, repo, opt)
		if err != nil {
			return nil, err
		}
		for _, is := range issues {
			// ListByRepo also returns PRs; skip them.
			if is.IsPullRequest() {
				continue
			}
			out = append(out, IssueRef{Number: is.GetNumber(), Title: is.GetTitle()})
		}
		if resp.NextPage == 0 {
			break
		}
		opt.Page = resp.NextPage
	}
	return out, nil
}
