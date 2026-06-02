// Package issue ports the pure GitHub-issue helpers from lib/issue.sh and
// lib/issue-images.sh: output truncation for issue comments and image-URL
// extraction. The gh-shell-out wrappers (comment/view/claim) are REWRITE
// targets and live in internal/ghclient, not here.
package issue

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

// truncationNotice is the Finnish notice prepended when output is truncated.
// The trailing newline is part of the byte budget (matches lib/issue.sh).
const truncationNotice = "_(Tuloste typistetty — täysi loki run-kansiossa.)_\n"

// TruncateForGitHub passes input through unchanged if it fits within maxBytes.
// Otherwise it keeps the TAIL (the decision line lives at the end:
// CYCLE_REVIEW_DECISION: / IMPLEMENTER_RESULT:) and drops the head, prepending
// a visible truncation notice. The notice is included in the byte budget so the
// result stays at or under maxBytes.
func TruncateForGitHub(input string, maxBytes int) string {
	if len(input) <= maxBytes {
		return input
	}
	noticeBytes := len(truncationNotice)
	tailBytes := maxBytes - noticeBytes
	if tailBytes < 0 {
		tailBytes = 0
	}
	b := []byte(input)
	tail := b
	if len(b) > tailBytes {
		tail = b[len(b)-tailBytes:]
	}
	return truncationNotice + string(tail)
}

// issueDoc mirrors the `gh issue view --json title,body,...,comments` shape we
// read for image extraction. Only the fields used are decoded.
type issueDoc struct {
	Body     string         `json:"body"`
	Comments []issueComment `json:"comments"`
}

type issueComment struct {
	Body string `json:"body"`
}

var (
	// Markdown image: ![alt](URL ...) — capture up to whitespace or ')'.
	reMarkdownImg = regexp.MustCompile(`!\[[^\]]*\]\(([^)\s]+)`)
	// HTML <img ...> tag.
	reImgTag = regexp.MustCompile(`(?i)<img[^>]*>`)
	// src attribute inside an <img> tag: quoted (single/double) or bare.
	reImgSrc = regexp.MustCompile(`(?i)src=("[^"]*"|'[^']*'|[^\s>"']+)`)
	// Bare GitHub attachment URLs (two known hosts).
	reGHAttach = regexp.MustCompile(`https://github\.com/user-attachments/assets/[A-Za-z0-9._/-]+`)
	reGHUserImg = regexp.MustCompile(`https://user-images\.githubusercontent\.com/[A-Za-z0-9._/?=&%-]+`)
)

// ExtractImageURLs returns deduped image URLs found in the issue body and in
// every comment that is NOT a run-issues bot comment (bodies containing the
// "run-issues:" token are skipped, mirroring detect_answer). First-occurrence
// order is preserved.
func ExtractImageURLs(issueJSON []byte) ([]string, error) {
	var doc issueDoc
	if err := json.Unmarshal(issueJSON, &doc); err != nil {
		return nil, err
	}
	var blobs []string
	blobs = append(blobs, doc.Body)
	for _, c := range doc.Comments {
		if strings.Contains(c.Body, "run-issues:") {
			continue
		}
		blobs = append(blobs, c.Body)
	}
	return extractImageURLsFromText(strings.Join(blobs, "\n")), nil
}

// extractImageURLsFromText recognises markdown images, HTML <img src>, and bare
// GitHub attachment URLs, deduped with first-occurrence order preserved.
func extractImageURLsFromText(text string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(u string) {
		u = strings.TrimSpace(u)
		if u == "" || seen[u] {
			return
		}
		seen[u] = true
		out = append(out, u)
	}

	// Patterns are applied in the same order as the bash pipeline so dedup
	// ordering matches: markdown, then html img, then bare GH hosts.
	for _, m := range reMarkdownImg.FindAllStringSubmatch(text, -1) {
		add(m[1])
	}
	for _, tag := range reImgTag.FindAllString(text, -1) {
		if sm := reImgSrc.FindStringSubmatch(tag); sm != nil {
			v := sm[1]
			v = strings.Trim(v, `"'`)
			add(v)
		}
	}
	for _, m := range reGHAttach.FindAllString(text, -1) {
		add(m)
	}
	for _, m := range reGHUserImg.FindAllString(text, -1) {
		add(m)
	}
	return out
}

// DownloadResult records the outcome for one issue image.
type DownloadResult struct {
	URL  string // original URL
	Path string // path on disk (empty if Err != nil)
	Err  error
}

// DownloadImages downloads each URL into destDir (created if missing) and
// returns a per-URL result. Per-URL errors do NOT abort the batch — agents can
// still work with the issue body even when a single attachment is unreachable.
// Filenames are deterministic ("00.png", "01.jpg" …) so re-runs overwrite the
// same files; the extension is derived from the URL path or Content-Type and
// falls back to ".bin".
func DownloadImages(ctx context.Context, destDir string, urls []string) ([]DownloadResult, error) {
	if len(urls) == 0 {
		return nil, nil
	}
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir %s: %w", destDir, err)
	}
	results := make([]DownloadResult, len(urls))
	for i, u := range urls {
		results[i] = downloadOne(ctx, destDir, i, u)
	}
	return results, nil
}

func downloadOne(ctx context.Context, destDir string, idx int, u string) DownloadResult {
	res := DownloadResult{URL: u}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		res.Err = err
		return res
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		res.Err = err
		return res
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		res.Err = fmt.Errorf("http %d", resp.StatusCode)
		return res
	}
	ext := extensionFor(u, resp.Header.Get("Content-Type"))
	fname := fmt.Sprintf("%02d%s", idx, ext)
	full := filepath.Join(destDir, fname)
	f, err := os.Create(full)
	if err != nil {
		res.Err = err
		return res
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		_ = f.Close()
		_ = os.Remove(full)
		res.Err = err
		return res
	}
	if err := f.Close(); err != nil {
		res.Err = err
		return res
	}
	res.Path = full
	return res
}

// extensionFor picks a sensible file extension. The URL's path basename is
// checked first (covers user-images.githubusercontent.com/*.png and
// markdown ![alt](https://x/y.png)); for bare GitHub user-attachments URLs
// (which carry no extension) we fall back to the Content-Type, then ".bin".
func extensionFor(rawURL, contentType string) string {
	if i := strings.IndexByte(rawURL, '?'); i >= 0 {
		rawURL = rawURL[:i]
	}
	base := path.Base(rawURL)
	if ext := strings.ToLower(filepath.Ext(base)); ext != "" && len(ext) <= 5 {
		return ext
	}
	switch strings.ToLower(strings.SplitN(contentType, ";", 2)[0]) {
	case "image/png":
		return ".png"
	case "image/jpeg", "image/jpg":
		return ".jpg"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	case "image/svg+xml":
		return ".svg"
	}
	return ".bin"
}
