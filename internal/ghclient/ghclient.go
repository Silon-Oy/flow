// Package ghclient is the central service's GitHub read client (R6: go-github
// for the scanner). It is the ONLY component that polls GitHub for issues —
// centralizing the rate limit (§4). The runner's git side-effect operations use
// gh-shell-out instead and live in internal/orchestrator.
package ghclient

import (
	"context"
	"encoding/json"
	"fmt"

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

// NewFromGithubClient wraps an already-built go-github client. Useful for tests
// that point at a stub server (httptest) and for callers that need to share a
// pre-configured client (custom HTTP transport, etc.).
func NewFromGithubClient(gh *github.Client) *Client {
	return &Client{gh: gh}
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

// IssueComment is a single comment, structured for prompt rendering and image
// extraction.
type IssueComment struct {
	Author string
	Body   string
}

// Issue is the full per-run issue context: title + body + every comment, in
// chronological (created-asc) order. RawJSON is a JSON document compatible with
// internal/issue.ExtractImageURLs (`{"body": ..., "comments": [{"body": ...}]}`)
// so the same extraction logic powers image discovery across body + comments.
type Issue struct {
	Number   int
	Title    string
	Body     string
	Comments []IssueComment
	RawJSON  []byte
}

// FetchIssue fetches the issue body + every comment (paginated). REWRITE of the
// bash `fetch_issue_json` helper (lib/issue.sh): one go-github call for the
// issue, one paginated call for comments, returning both a structured view (for
// prompt rendering) and a JSON document compatible with
// internal/issue.ExtractImageURLs.
func (c *Client) FetchIssue(ctx context.Context, owner, repo string, number int) (*Issue, error) {
	is, _, err := c.gh.Issues.Get(ctx, owner, repo, number)
	if err != nil {
		return nil, fmt.Errorf("issues.Get %s/%s#%d: %w", owner, repo, number, err)
	}
	comments, err := c.listAllComments(ctx, owner, repo, number)
	if err != nil {
		return nil, err
	}
	out := &Issue{
		Number:   is.GetNumber(),
		Title:    is.GetTitle(),
		Body:     is.GetBody(),
		Comments: comments,
	}
	raw, err := marshalForImageExtraction(out)
	if err != nil {
		return nil, fmt.Errorf("marshal issue json: %w", err)
	}
	out.RawJSON = raw
	return out, nil
}

func (c *Client) listAllComments(ctx context.Context, owner, repo string, number int) ([]IssueComment, error) {
	opt := &github.IssueListCommentsOptions{
		Sort:        github.String("created"),
		Direction:   github.String("asc"),
		ListOptions: github.ListOptions{PerPage: 100},
	}
	var out []IssueComment
	for {
		page, resp, err := c.gh.Issues.ListComments(ctx, owner, repo, number, opt)
		if err != nil {
			return nil, fmt.Errorf("issues.ListComments %s/%s#%d: %w", owner, repo, number, err)
		}
		for _, ic := range page {
			out = append(out, IssueComment{
				Author: ic.GetUser().GetLogin(),
				Body:   ic.GetBody(),
			})
		}
		if resp.NextPage == 0 {
			return out, nil
		}
		opt.Page = resp.NextPage
	}
}

// marshalForImageExtraction produces a JSON document with only the fields
// internal/issue.ExtractImageURLs reads (body + comments[].body). Keeping this
// minimal avoids leaking PII-rich fields (author handles, timestamps) into the
// image-extraction surface and keeps the contract with the issue package
// trivially stable.
func marshalForImageExtraction(is *Issue) ([]byte, error) {
	type jsonComment struct {
		Body string `json:"body"`
	}
	type jsonDoc struct {
		Body     string        `json:"body"`
		Comments []jsonComment `json:"comments"`
	}
	doc := jsonDoc{Body: is.Body, Comments: make([]jsonComment, len(is.Comments))}
	for i, c := range is.Comments {
		doc.Comments[i] = jsonComment{Body: c.Body}
	}
	return json.Marshal(doc)
}
