package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/Silon-Oy/flow/internal/githubapp"
	"github.com/Silon-Oy/flow/internal/store"
)

// projectCreateReq is the wire shape POST /v1/projects accepts (§8 wizard
// datamalli). Optional fields are pointers / omitempty so a CI YAML can omit
// them without forcing the wizard to invent defaults at the wire boundary —
// defaults are applied in the handler.
type projectCreateReq struct {
	Name                 string            `json:"name"`
	OwnerRepo            string            `json:"owner_repo"`
	Remotes              []projectRemoteIn `json:"remotes"`
	Labels               []string          `json:"labels"`
	BaseBranch           string            `json:"base_branch"`
	RunnerPool           string            `json:"runner_pool,omitempty"`
	ClaudeTimeoutSeconds int               `json:"claude_timeout_seconds,omitempty"`
	MergePolicy          map[string]any    `json:"merge_policy,omitempty"`
	SecretRefs           map[string]string `json:"secret_refs,omitempty"`
}

// projectRemoteIn mirrors store.ProjectRemote on the wire (päätös 14). The
// validator resolves the canonical owner/repo + base_branch shape; the store
// shape is the single source of truth.
type projectRemoteIn struct {
	Remote     string `json:"remote"`
	OwnerRepo  string `json:"owner_repo"`
	BaseBranch string `json:"base_branch,omitempty"`
}

// projectCreateResp returns just the new id; the wizard fetches the full
// record back via GET on a follow-up (not part of #9).
type projectCreateResp struct {
	ProjectID string `json:"project_id"`
}

// ownerRepoRegex matches "owner/repo" — owner and repo each a non-empty
// segment of GitHub-permitted characters (letters/digits/_/./-), no slash
// inside either segment. GitHub itself permits "_" and "." in repo names;
// "-" is the most common.
var ownerRepoRegex = regexp.MustCompile(`^[A-Za-z0-9._-]+/[A-Za-z0-9._-]+$`)

// branchNameRegex matches a conservative git branch name: no whitespace, no
// control chars, no ASCII characters git rejects. We do not try to enforce
// every git ref-name rule (refs/heads/.. -double-dot, trailing /, etc.) — the
// GitHub branch-existence check (the next step) catches malformed refs by
// returning 404.
var branchNameRegex = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)

// uuidRegex matches the canonical 8-4-4-4-12 hex UUID form; runner_pool is a
// uuid column so we sanity-check the string before we let pgx try to cast.
var uuidRegex = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// BranchValidator verifies the named ref exists on a GitHub repo. The default
// implementation lives in branchValidatorBroker (uses the App-token broker);
// tests inject a stub so they don't need an httptest GitHub mock.
//
// The signature carries tenantID + org so a multi-org project validates each
// remote against its own App installation (§7.3 broker generalisation).
type BranchValidator interface {
	// CheckBranch returns nil if the branch exists, ErrBranchNotFound if the
	// GitHub branches endpoint returns 404, ErrInstallationMissing if the
	// tenant has no App install for that org, and a wrapped error for any
	// other transport/GitHub failure (handler maps it to 502).
	CheckBranch(ctx context.Context, tenantID, org, repo, branch string) error
}

// ErrBranchNotFound / ErrInstallationMissing are the structured failure modes
// CheckBranch surfaces. The handler translates each to a 4xx with a stable
// error string so the wizard can diagnose without parsing prose.
var (
	ErrBranchNotFound      = errors.New("branch not found on remote")
	ErrInstallationMissing = errors.New("no github app installation for org")
)

// branchValidatorBroker is the default BranchValidator: ask the App-token
// broker for an installation token, then HEAD /repos/{owner}/{repo}/branches/
// {branch}. A 200 means the branch exists; 404 → ErrBranchNotFound. We use
// HEAD (no body) so the cost is one round-trip per (org, repo, branch).
type branchValidatorBroker struct {
	broker *githubapp.Broker
	http   *http.Client
	apiURL string
}

// NewBranchValidator wires the default validator. apiBase defaults to GitHub's
// public REST root; tests can override via WithAPIBase on the broker.
func NewBranchValidator(b *githubapp.Broker) BranchValidator {
	if b == nil {
		return noBranchValidator{}
	}
	return &branchValidatorBroker{
		broker: b,
		http:   &http.Client{Timeout: 15 * time.Second},
		apiURL: githubapp.DefaultGitHubAPIBase,
	}
}

// noBranchValidator is the fail-open default when no broker is wired (Vaihe 1
// single-tenant dev mode without an App install). It accepts every branch; the
// CLI still gets the structural validation. Production deployments inject the
// broker-backed validator.
type noBranchValidator struct{}

func (noBranchValidator) CheckBranch(context.Context, string, string, string, string) error {
	return nil
}

func (v *branchValidatorBroker) CheckBranch(ctx context.Context, tenantID, org, repo, branch string) error {
	tok, err := v.broker.Token(ctx, tenantID, org)
	if errors.Is(err, githubapp.ErrInstallNotFound) {
		return ErrInstallationMissing
	}
	if err != nil {
		return fmt.Errorf("mint token for %s: %w", org, err)
	}
	url := fmt.Sprintf("%s/repos/%s/%s/branches/%s", v.apiURL, org, repo, branch)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+tok.Token)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := v.http.Do(req)
	if err != nil {
		return fmt.Errorf("github branches: %w", err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		return nil
	case http.StatusNotFound:
		return ErrBranchNotFound
	default:
		return fmt.Errorf("github branches: HTTP %d", resp.StatusCode)
	}
}

// handleProjectCreate is the POST /v1/projects entry. RBAC: CapProjectRegister
// (developer + admin both allowed — §7 row 1).
//
// Validation order (cheapest first):
//   1. JSON shape (decodeJSON).
//   2. Structural rules: name, owner_repo, labels, base_branch, runner_pool uuid,
//      secret_refs are strings (architecture invariant — references, not values).
//   3. Per-remote App-installation + branch-existence (network call, gated by
//      structural validation passing).
//   4. Persist via store.InsertProject.
//
// The handler intentionally re-validates everything the CLI may have already
// checked: the central is the trust boundary (§8 acceptance criterion).
func (s *Server) handleProjectCreate(w http.ResponseWriter, r *http.Request) {
	var req projectCreateReq
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	tenantID := tenantFromCtx(r.Context())
	if tenantID == "" {
		writeErr(w, http.StatusInternalServerError, "tenant not pinned")
		return
	}

	if err := validateProjectStructural(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	ctx, cancel := withTimeout(r, 30*time.Second)
	defer cancel()

	validator := s.BranchValidator
	if validator == nil {
		validator = noBranchValidator{}
	}
	if err := validateRemotesAgainstGitHub(ctx, validator, tenantID, &req); err != nil {
		// Distinguish installation-missing (412 — admin action required) from
		// branch-missing (422 — user-fixable input) from upstream-broken (502).
		switch {
		case errors.Is(err, ErrInstallationMissing):
			writeErr(w, http.StatusPreconditionFailed, err.Error())
		case errors.Is(err, ErrBranchNotFound):
			writeErr(w, http.StatusUnprocessableEntity, err.Error())
		default:
			writeErr(w, http.StatusBadGateway, err.Error())
		}
		return
	}

	insert := buildProjectInsert(tenantID, &req)
	id, err := store.InsertProject(ctx, s.Pool, insert)
	if errors.Is(err, store.ErrProjectExists) {
		writeErr(w, http.StatusConflict, "project name already exists in tenant")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "insert project: "+err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, projectCreateResp{ProjectID: id})
}

// validateProjectStructural enforces every §8 structural rule. Each branch
// returns a stable lowercase error so the wizard can diagnose without
// regex-matching prose. Defaults (labels=["auto-run"], base_branch="main")
// are applied here so the validation rules below see the *effective* values
// the row will be written with.
func validateProjectStructural(req *projectCreateReq) error {
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		return errors.New("name required")
	}
	if len(req.Name) > 128 {
		return errors.New("name too long (max 128)")
	}
	req.OwnerRepo = strings.TrimSpace(req.OwnerRepo)
	if !ownerRepoRegex.MatchString(req.OwnerRepo) {
		return errors.New("owner_repo must match owner/repo")
	}
	if req.BaseBranch == "" {
		req.BaseBranch = "main"
	}
	if !branchNameRegex.MatchString(req.BaseBranch) {
		return errors.New("base_branch contains invalid characters")
	}
	if len(req.Labels) == 0 {
		req.Labels = []string{"auto-run"}
	}
	for _, l := range req.Labels {
		if strings.TrimSpace(l) == "" {
			return errors.New("label must not be empty")
		}
	}
	if req.ClaudeTimeoutSeconds < 0 {
		return errors.New("claude_timeout_seconds must be positive")
	}
	if req.RunnerPool != "" && !uuidRegex.MatchString(req.RunnerPool) {
		return errors.New("runner_pool must be a uuid")
	}
	// secret_refs invariant: values are KEYS into secret_ref, not plaintext.
	// We can't fully prove the value isn't a secret, but we can reject obvious
	// shapes (leading "ghp_" / "github_pat_" / "sk-" prefixes — common token
	// prefixes) and reject any value that contains whitespace (secrets do not).
	for k, v := range req.SecretRefs {
		if strings.TrimSpace(k) == "" {
			return errors.New("secret_refs key must not be empty")
		}
		if v == "" {
			return errors.New("secret_refs[" + k + "] must reference a key, not be empty")
		}
		if strings.ContainsAny(v, " \t\n") {
			return errors.New("secret_refs[" + k + "] must be a reference key, not contain whitespace")
		}
		if hasSecretLikePrefix(v) {
			return errors.New("secret_refs[" + k + "] must be a reference key, not a token value (architecture: §8 secrets are references, not values)")
		}
	}
	// Default remotes: if none supplied, infer a single "origin" remote
	// pointing at owner_repo. This mirrors the scanner's fallback.
	if len(req.Remotes) == 0 {
		req.Remotes = []projectRemoteIn{{Remote: "origin", OwnerRepo: req.OwnerRepo}}
	}
	seen := map[string]struct{}{}
	for i := range req.Remotes {
		rem := &req.Remotes[i]
		rem.Remote = strings.TrimSpace(rem.Remote)
		rem.OwnerRepo = strings.TrimSpace(rem.OwnerRepo)
		rem.BaseBranch = strings.TrimSpace(rem.BaseBranch)
		if rem.Remote == "" {
			return errors.New("remotes[].remote required")
		}
		if !ownerRepoRegex.MatchString(rem.OwnerRepo) {
			return errors.New("remotes[" + rem.Remote + "].owner_repo must match owner/repo")
		}
		if rem.BaseBranch != "" && !branchNameRegex.MatchString(rem.BaseBranch) {
			return errors.New("remotes[" + rem.Remote + "].base_branch contains invalid characters")
		}
		if _, dup := seen[rem.Remote]; dup {
			return errors.New("remotes[].remote duplicated: " + rem.Remote)
		}
		seen[rem.Remote] = struct{}{}
	}
	return nil
}

// validateRemotesAgainstGitHub resolves the effective base_branch per remote
// (päätös 14: rem.BaseBranch ?? req.BaseBranch) and asks the validator if it
// exists. The validator is gated on an App-installation for the org; missing
// installation is its own error so the wizard can prompt the admin to register
// the App for that org.
func validateRemotesAgainstGitHub(ctx context.Context, v BranchValidator, tenantID string, req *projectCreateReq) error {
	for _, rem := range req.Remotes {
		owner, repo, ok := splitOwnerRepo(rem.OwnerRepo)
		if !ok {
			// splitOwnerRepo only fails on malformed input the structural
			// validator should have caught; surface it so we never silently
			// skip a remote.
			return fmt.Errorf("remote %s: cannot split owner_repo %q", rem.Remote, rem.OwnerRepo)
		}
		branch := rem.BaseBranch
		if branch == "" {
			branch = req.BaseBranch
		}
		if err := v.CheckBranch(ctx, tenantID, owner, repo, branch); err != nil {
			return fmt.Errorf("remote %s: %w", rem.Remote, err)
		}
	}
	return nil
}

func buildProjectInsert(tenantID string, req *projectCreateReq) store.ProjectInsert {
	remotes := make([]store.ProjectRemote, 0, len(req.Remotes))
	for _, r := range req.Remotes {
		remotes = append(remotes, store.ProjectRemote{
			Remote:     r.Remote,
			OwnerRepo:  r.OwnerRepo,
			BaseBranch: r.BaseBranch,
		})
	}
	var pool *string
	if req.RunnerPool != "" {
		v := req.RunnerPool
		pool = &v
	}
	return store.ProjectInsert{
		TenantID:             tenantID,
		Name:                 req.Name,
		OwnerRepo:            req.OwnerRepo,
		Remotes:              remotes,
		Labels:               req.Labels,
		BaseBranch:           req.BaseBranch,
		RunnerPool:           pool,
		ClaudeTimeoutSeconds: req.ClaudeTimeoutSeconds,
		MergePolicy:          req.MergePolicy,
		SecretRefs:           req.SecretRefs,
	}
}

// mergePolicyReq is the wire shape PUT /v1/projects/{id}/merge-policy accepts.
// §8 lists two known fields (label + conflict-flag); extra keys are rejected so
// typos surface immediately rather than silently disappearing into jsonb.
//
//   - label: the GitHub PR label that signals "ready to merge" (e.g.
//     "auto-merge"). Matches prwatch.Decide's mergeLabel argument. Optional;
//     prwatch defaults to "auto-merge" when empty.
//   - conflict_resolution: when true, BEHIND/DIRTY PRs are auto-rebased
//     (prwatch returns REBASE instead of WAIT_DIRTY). Optional; default false.
type mergePolicyReq struct {
	Label              *string `json:"label,omitempty"`
	ConflictResolution *bool   `json:"conflict_resolution,omitempty"`
}

// mergePolicyResp echoes the persisted policy. The handler returns the value
// it wrote so the dashboard's form can rerender from the canonical state.
type mergePolicyResp struct {
	MergePolicy map[string]any `json:"merge_policy"`
}

// handleProjectMergePolicy is PUT /v1/projects/{id}/merge-policy. RBAC:
// CapMergePolicyManage (admin-only — §7 row "Muokkaa merge-policya"). The
// tenant filter on store.UpdateMergePolicy guarantees an admin in tenant A
// cannot rewrite tenant B's policy even with a guessed id.
func (s *Server) handleProjectMergePolicy(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !uuidRegex.MatchString(id) {
		writeErr(w, http.StatusBadRequest, "id must be a uuid")
		return
	}
	var req mergePolicyReq
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	tenantID := tenantFromCtx(r.Context())
	if tenantID == "" {
		writeErr(w, http.StatusInternalServerError, "tenant not pinned")
		return
	}

	policy := map[string]any{}
	if req.Label != nil {
		label := strings.TrimSpace(*req.Label)
		// branchNameRegex is conservative enough for a GitHub label slug too —
		// labels with whitespace/quotes break `gh pr view` jq lookups anyway.
		// Empty string is allowed: it means "reset to prwatch default".
		if label != "" && !branchNameRegex.MatchString(label) {
			writeErr(w, http.StatusBadRequest, "label contains invalid characters")
			return
		}
		if len(label) > 64 {
			writeErr(w, http.StatusBadRequest, "label too long (max 64)")
			return
		}
		policy["label"] = label
	}
	if req.ConflictResolution != nil {
		policy["conflict_resolution"] = *req.ConflictResolution
	}

	ctx, cancel := withTimeout(r, 5*time.Second)
	defer cancel()
	if err := store.UpdateMergePolicy(ctx, s.Pool, tenantID, id, policy); err != nil {
		if errors.Is(err, store.ErrProjectNotFound) {
			writeErr(w, http.StatusNotFound, "unknown project")
			return
		}
		writeErr(w, http.StatusInternalServerError, "update merge_policy: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, mergePolicyResp{MergePolicy: policy})
}

// hasSecretLikePrefix is a cheap heuristic: token formats we *know* are
// secrets must not appear as a secret_refs value. Belt-and-braces; the real
// invariant is enforced by the secret_refs schema (a key, never a value).
func hasSecretLikePrefix(v string) bool {
	for _, prefix := range []string{"ghp_", "gho_", "ghu_", "ghs_", "github_pat_", "sk-", "xoxb-", "xoxp-"} {
		if strings.HasPrefix(v, prefix) {
			return true
		}
	}
	return false
}
