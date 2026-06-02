package ghclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/google/go-github/v66/github"

	"github.com/Silon-Oy/flow/internal/issue"
)

// stubGitHub builds an httptest.Server that answers Issues.Get and
// (paginated) Issues.ListComments for one owner/repo/number, plus a go-github
// client pointed at it.
func stubGitHub(t *testing.T, owner, repo string, number int, body string, pages [][]map[string]any) (*Client, func()) {
	t.Helper()
	mux := http.NewServeMux()
	issuePath := fmt.Sprintf("/repos/%s/%s/issues/%d", owner, repo, number)
	commentsPath := fmt.Sprintf("/repos/%s/%s/issues/%d/comments", owner, repo, number)

	mux.HandleFunc(issuePath, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"number": number,
			"title":  "Test issue",
			"body":   body,
		})
	})

	srv := httptest.NewServer(mux)

	// Capture srv inside the handler closure so the Link header points back at
	// the same server (so go-github's pagination follows our stub).
	mux.HandleFunc(commentsPath, func(w http.ResponseWriter, r *http.Request) {
		page := 1
		if p := r.URL.Query().Get("page"); p != "" {
			fmt.Sscanf(p, "%d", &page)
		}
		if page < 1 {
			page = 1
		}
		idx := page - 1
		if idx >= len(pages) {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte("[]"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if page < len(pages) {
			// Emit a RFC 5988 Link header so go-github's NextPage parses to page+1.
			next := fmt.Sprintf("%s%s?page=%d", srv.URL, commentsPath, page+1)
			w.Header().Set("Link", fmt.Sprintf(`<%s>; rel="next"`, next))
		}
		_ = json.NewEncoder(w).Encode(pages[idx])
	})

	// Point go-github at the stub server.
	gh := github.NewClient(srv.Client())
	base, err := url.Parse(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	gh.BaseURL = base

	return NewFromGithubClient(gh), srv.Close
}

func TestFetchIssueBodyAndComments(t *testing.T) {
	const owner, repo = "o", "r"
	const number = 42
	const body = "Issue body with ![pic](https://github.com/user-attachments/assets/aaa-bbb)."
	pages := [][]map[string]any{
		// page 1
		{
			{"user": map[string]any{"login": "alice"}, "body": "First comment ![c1](https://github.com/user-attachments/assets/ccc-ddd)"},
			{"user": map[string]any{"login": "run-issues[bot]"}, "body": "<!-- run-issues:awaiting-answer --> ignore me ![bot](https://github.com/user-attachments/assets/9999)"},
		},
		// page 2
		{
			{"user": map[string]any{"login": "bob"}, "body": "Second-page comment, plain text."},
		},
	}

	c, closeSrv := stubGitHub(t, owner, repo, number, body, pages)
	defer closeSrv()

	got, err := c.FetchIssue(context.Background(), owner, repo, number)
	if err != nil {
		t.Fatalf("FetchIssue: %v", err)
	}

	if got.Number != number {
		t.Errorf("number = %d, want %d", got.Number, number)
	}
	if got.Title != "Test issue" {
		t.Errorf("title = %q, want %q", got.Title, "Test issue")
	}
	if got.Body != body {
		t.Errorf("body mismatch:\n got=%q\nwant=%q", got.Body, body)
	}
	if len(got.Comments) != 3 {
		t.Fatalf("comments len = %d, want 3 (alice + run-issues bot + bob)", len(got.Comments))
	}
	if got.Comments[0].Author != "alice" || !strings.Contains(got.Comments[0].Body, "First comment") {
		t.Errorf("comments[0] = %+v", got.Comments[0])
	}
	if got.Comments[2].Author != "bob" {
		t.Errorf("comments[2] author = %q, want bob (paginated tail missing?)", got.Comments[2].Author)
	}

	// RawJSON must round-trip through internal/issue.ExtractImageURLs and
	// include only issue-image URLs from body + non-bot comments (the
	// run-issues:* marker comment is suppressed at extraction).
	urls, err := issue.ExtractImageURLs(got.RawJSON)
	if err != nil {
		t.Fatalf("ExtractImageURLs: %v", err)
	}
	want := map[string]bool{
		"https://github.com/user-attachments/assets/aaa-bbb": true,
		"https://github.com/user-attachments/assets/ccc-ddd": true,
	}
	for _, u := range urls {
		if !want[u] {
			t.Errorf("unexpected image url: %s", u)
		}
		delete(want, u)
	}
	for u := range want {
		t.Errorf("missing image url: %s", u)
	}
}

func TestFetchIssueEmptyComments(t *testing.T) {
	const owner, repo = "o", "r"
	const number = 7
	c, closeSrv := stubGitHub(t, owner, repo, number, "no images here", nil)
	defer closeSrv()

	got, err := c.FetchIssue(context.Background(), owner, repo, number)
	if err != nil {
		t.Fatalf("FetchIssue: %v", err)
	}
	if got.Body != "no images here" {
		t.Errorf("body = %q", got.Body)
	}
	if len(got.Comments) != 0 {
		t.Errorf("comments len = %d, want 0", len(got.Comments))
	}
	if len(got.RawJSON) == 0 {
		t.Errorf("RawJSON must be non-empty (it powers image extraction)")
	}
}
