package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/Silon-Oy/flow/internal/ghclient"
)

// WorkKey is the canonical claimable-work uniqueness key, matching the lease
// arbitration key (tenant, project, remote, issue, kind). Centralizing the
// formula here keeps the scanner (producer) and the lease (consumer) in sync.
func WorkKey(tenantID, projectID, remote string, issue int, kind string) string {
	return fmt.Sprintf("%s:%s:%s:%d:%s", tenantID, projectID, remote, issue, kind)
}

// Scanner polls GitHub for auto-run issues and upserts CLAIMABLE_WORK. It is the
// only GitHub-polling component (§4). It does NOT claim work — the lease does.
type Scanner struct {
	srv      *Server
	ghToken  string
	interval time.Duration
}

// NewScanner builds a Scanner. ghToken may be empty (rate-limited anon access)
// in Vaihe 1; the per-tenant App broker supplies scoped tokens in Vaihe 2.
func NewScanner(srv *Server, ghToken string, interval time.Duration) *Scanner {
	if interval <= 0 {
		interval = 60 * time.Second
	}
	return &Scanner{srv: srv, ghToken: ghToken, interval: interval}
}

// Run scans on a ticker until ctx is cancelled.
func (sc *Scanner) Run(ctx context.Context) {
	ticker := time.NewTicker(sc.interval)
	defer ticker.Stop()
	// Scan once immediately so a fresh boot does not wait a full interval.
	sc.scanOnce(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sc.scanOnce(ctx)
		}
	}
}

// project is the subset of the project row the scanner needs. tenantID is
// loaded per-row, not derived from a global "current tenant", because the
// scanner is system-level: it iterates every tenant on every tick (§7).
type scanProject struct {
	tenantID  string
	id        string
	ownerRepo string
	remotes   []remoteEntry
	labels    []string
}

// remoteEntry is the canonical PROJECT.remotes[] shape (§8, päätös 14): the
// remote name + its resolved owner/repo + an optional per-remote base_branch
// override. The scanner reads owner/repo directly from project config (the
// runner resolves git remotes locally; the scanner has no clone). base_branch
// is consumed elsewhere; the scanner only needs name + owner/repo.
type remoteEntry struct {
	Remote     string `json:"remote"`
	OwnerRepo  string `json:"owner_repo"`
	BaseBranch string `json:"base_branch,omitempty"`
}

// scanOnce iterates EVERY tenant's projects and enqueues their auto-run
// issues as claimable work. The scanner is a system-level component (no HTTP
// request, no middleware): it MUST iterate tenants explicitly so there is no
// implicit "tenant-empty = all tenants" shortcut bypassing the §7 boundary.
// Errors are logged, not fatal — a transient GitHub failure must not crash
// the central service.
func (sc *Scanner) scanOnce(ctx context.Context) {
	projects, err := sc.loadProjects(ctx)
	if err != nil {
		log.Printf("scanner: load projects: %v", err)
		return
	}
	gh := ghclient.New(sc.ghToken)
	for _, p := range projects {
		// A project with no explicit remotes scans its owner_repo as "origin".
		remotes := p.remotes
		if len(remotes) == 0 {
			remotes = []remoteEntry{{Remote: "origin", OwnerRepo: p.ownerRepo}}
		}
		labels := p.labels
		if len(labels) == 0 {
			labels = []string{"auto-run"}
		}
		for _, rem := range remotes {
			if rem.OwnerRepo == "" {
				continue
			}
			owner, repo, ok := splitOwnerRepo(rem.OwnerRepo)
			if !ok {
				continue
			}
			for _, label := range labels {
				issues, err := gh.ListOpenLabeledIssues(ctx, owner, repo, label)
				if err != nil {
					log.Printf("scanner: list %s/%s label=%s: %v", owner, repo, label, err)
					continue
				}
				for _, is := range issues {
					sc.enqueue(ctx, p.tenantID, p.id, rem.Remote, is.Number)
				}
			}
		}
	}
}

// loadProjects returns every project across every tenant. The result carries
// tenant_id per row so enqueue can stamp the correct tenant onto each work
// row — there is no shared "current tenant" state in the scanner.
func (sc *Scanner) loadProjects(ctx context.Context) ([]scanProject, error) {
	rows, err := sc.srv.Pool.Query(ctx,
		`SELECT tenant_id::text, id::text, owner_repo, remotes, labels FROM project`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []scanProject
	for rows.Next() {
		var p scanProject
		var remotesRaw, labelsRaw []byte
		if err := rows.Scan(&p.tenantID, &p.id, &p.ownerRepo, &remotesRaw, &labelsRaw); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(remotesRaw, &p.remotes)
		_ = json.Unmarshal(labelsRaw, &p.labels)
		out = append(out, p)
	}
	return out, rows.Err()
}

// enqueue upserts a develop-kind work row stamped with the project's own
// tenant. ON CONFLICT (work_key) DO NOTHING makes re-scanning idempotent: an
// issue already queued (and possibly leased) is not re-inserted.
func (sc *Scanner) enqueue(ctx context.Context, tenantID, projectID, remote string, issue int) {
	wk := WorkKey(tenantID, projectID, remote, issue, "develop")
	_, err := sc.srv.Pool.Exec(ctx, `
		INSERT INTO claimable_work (tenant_id, project_id, work_key, remote, issue_number, kind)
		VALUES ($1, $2, $3, $4, $5, 'develop')
		ON CONFLICT (work_key) DO NOTHING`,
		tenantID, projectID, wk, remote, issue)
	if err != nil {
		log.Printf("scanner: enqueue %s: %v", wk, err)
	}
}

func splitOwnerRepo(s string) (owner, repo string, ok bool) {
	for i := 0; i < len(s); i++ {
		if s[i] == '/' {
			owner, repo = s[:i], s[i+1:]
			return owner, repo, owner != "" && repo != "" && !containsSlash(repo)
		}
	}
	return "", "", false
}

func containsSlash(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == '/' {
			return true
		}
	}
	return false
}
