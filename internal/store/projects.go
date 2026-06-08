package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrProjectExists is returned when (tenant_id, name) collides on insert. The
// UNIQUE constraint on project(tenant_id, name) is the source of truth — this
// sentinel maps the pgx-level "23505" violation onto a stable error the HTTP
// layer can translate to 409.
var ErrProjectExists = errors.New("project: name already exists in tenant")

// ProjectRemote is one row of PROJECT.remotes[] (§8, päätös 14): a logical git
// remote name, its resolved owner/repo, and an optional per-remote base_branch
// override. The override is the only field the architecture made optional —
// resolution is remotes[i].BaseBranch ?? PROJECT.BaseBranch.
type ProjectRemote struct {
	Remote     string `json:"remote"`
	OwnerRepo  string `json:"owner_repo"`
	BaseBranch string `json:"base_branch,omitempty"`
}

// ProjectInsert is the shape InsertProject accepts. It mirrors the §8 wizard
// data model fields one-to-one (name, owner_repo, remotes[], labels[],
// base_branch, runner_pool, claude_timeout_seconds, merge_policy, secret_refs).
// SecretRefs values are KEYS into the secret_ref table — never plaintext values
// (architecture invariant).
type ProjectInsert struct {
	TenantID             string
	Name                 string
	OwnerRepo            string
	Remotes              []ProjectRemote
	Labels               []string
	BaseBranch           string
	RunnerPool           *string // uuid; nil = no pool pinned (Vaihe 1)
	ClaudeTimeoutSeconds int
	MergePolicy          map[string]any
	SecretRefs           map[string]string
}

// InsertProject writes one PROJECT row and returns its id. The merge_policy /
// claude_config / remotes / labels / secret_refs columns are jsonb — encoded
// here so the handler does not have to know the column types. Returns
// ErrProjectExists on a (tenant_id, name) collision so the handler can map it
// to 409 without leaking SQL state codes.
func InsertProject(ctx context.Context, pool *pgxpool.Pool, p ProjectInsert) (string, error) {
	if p.TenantID == "" {
		return "", errors.New("store: tenant_id required")
	}
	remotesJSON, err := json.Marshal(coalesceRemotes(p.Remotes))
	if err != nil {
		return "", fmt.Errorf("encode remotes: %w", err)
	}
	labelsJSON, err := json.Marshal(coalesceLabels(p.Labels))
	if err != nil {
		return "", fmt.Errorf("encode labels: %w", err)
	}
	mergeJSON, err := json.Marshal(coalesceMap(p.MergePolicy))
	if err != nil {
		return "", fmt.Errorf("encode merge_policy: %w", err)
	}
	secretsJSON, err := json.Marshal(coalesceStringMap(p.SecretRefs))
	if err != nil {
		return "", fmt.Errorf("encode secret_refs: %w", err)
	}
	claudeJSON, err := json.Marshal(coalesceClaudeConfig(p.ClaudeTimeoutSeconds))
	if err != nil {
		return "", fmt.Errorf("encode claude_config: %w", err)
	}

	baseBranch := p.BaseBranch
	if baseBranch == "" {
		baseBranch = "main"
	}

	var id string
	err = pool.QueryRow(ctx, `
		INSERT INTO project (
			tenant_id, name, owner_repo, remotes, labels, base_branch,
			runner_pool, secret_refs, merge_policy, claude_config
		) VALUES ($1, $2, $3, $4::jsonb, $5::jsonb, $6, $7, $8::jsonb, $9::jsonb, $10::jsonb)
		RETURNING id::text`,
		p.TenantID, p.Name, p.OwnerRepo, remotesJSON, labelsJSON, baseBranch,
		p.RunnerPool, secretsJSON, mergeJSON, claudeJSON,
	).Scan(&id)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return "", ErrProjectExists
		}
		return "", err
	}
	return id, nil
}

// ProjectRecord is the read shape returned by GetProject. Mirrors the columns
// the wizard / dashboard need to surface back to the operator.
type ProjectRecord struct {
	ID                   string            `json:"id"`
	TenantID             string            `json:"tenant_id"`
	Name                 string            `json:"name"`
	OwnerRepo            string            `json:"owner_repo"`
	Remotes              []ProjectRemote   `json:"remotes"`
	Labels               []string          `json:"labels"`
	BaseBranch           string            `json:"base_branch"`
	RunnerPool           *string           `json:"runner_pool,omitempty"`
	ClaudeTimeoutSeconds int               `json:"claude_timeout_seconds,omitempty"`
	MergePolicy          map[string]any    `json:"merge_policy,omitempty"`
	SecretRefs           map[string]string `json:"secret_refs,omitempty"`
}

// GetProject loads one tenant-scoped project by id. Tenant filter is part of
// the WHERE clause — callers must NOT pass an empty tenantID (a §7 invariant
// crossover).
func GetProject(ctx context.Context, pool *pgxpool.Pool, tenantID, id string) (*ProjectRecord, error) {
	if tenantID == "" || id == "" {
		return nil, errors.New("store: tenant_id and id required")
	}
	var (
		rec                       ProjectRecord
		remotesRaw, labelsRaw     []byte
		mergeRaw, secretsRaw      []byte
		claudeRaw                 []byte
		pool_                     *string
	)
	err := pool.QueryRow(ctx, `
		SELECT id::text, tenant_id::text, name, owner_repo, remotes, labels,
		       base_branch, runner_pool::text, secret_refs, merge_policy, claude_config
		  FROM project WHERE tenant_id = $1 AND id = $2`,
		tenantID, id,
	).Scan(&rec.ID, &rec.TenantID, &rec.Name, &rec.OwnerRepo, &remotesRaw, &labelsRaw,
		&rec.BaseBranch, &pool_, &secretsRaw, &mergeRaw, &claudeRaw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrProjectNotFound
	}
	if err != nil {
		return nil, err
	}
	rec.RunnerPool = pool_
	if len(remotesRaw) > 0 {
		_ = json.Unmarshal(remotesRaw, &rec.Remotes)
	}
	if len(labelsRaw) > 0 {
		_ = json.Unmarshal(labelsRaw, &rec.Labels)
	}
	if len(mergeRaw) > 0 {
		_ = json.Unmarshal(mergeRaw, &rec.MergePolicy)
	}
	if len(secretsRaw) > 0 {
		_ = json.Unmarshal(secretsRaw, &rec.SecretRefs)
	}
	if len(claudeRaw) > 0 {
		var cc struct {
			TimeoutSeconds int `json:"timeout_seconds"`
		}
		if err := json.Unmarshal(claudeRaw, &cc); err == nil {
			rec.ClaudeTimeoutSeconds = cc.TimeoutSeconds
		}
	}
	return &rec, nil
}

// ErrProjectNotFound is returned by GetProject when the (tenant_id, id) pair
// has no row. Callers (HTTP layer) map this to 404.
var ErrProjectNotFound = errors.New("project: not found")

// UpdateMergePolicy writes the merge_policy column on a tenant-scoped project.
// Returns ErrProjectNotFound if the (tenant_id, id) pair has no row — the
// handler maps that to 404 without leaking existence across tenants. The
// payload is jsonb so the value passes through as a JSON object; structural
// validation is the caller's job.
func UpdateMergePolicy(ctx context.Context, pool *pgxpool.Pool, tenantID, id string, policy map[string]any) error {
	if tenantID == "" || id == "" {
		return errors.New("store: tenant_id and id required")
	}
	mergeJSON, err := json.Marshal(coalesceMap(policy))
	if err != nil {
		return fmt.Errorf("encode merge_policy: %w", err)
	}
	tag, err := pool.Exec(ctx, `
		UPDATE project
		   SET merge_policy = $3::jsonb
		 WHERE tenant_id = $1 AND id = $2`,
		tenantID, id, mergeJSON,
	)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrProjectNotFound
	}
	return nil
}

func coalesceRemotes(in []ProjectRemote) []ProjectRemote {
	if in == nil {
		return []ProjectRemote{}
	}
	return in
}

func coalesceLabels(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}

func coalesceMap(in map[string]any) map[string]any {
	if in == nil {
		return map[string]any{}
	}
	return in
}

func coalesceStringMap(in map[string]string) map[string]string {
	if in == nil {
		return map[string]string{}
	}
	return in
}

func coalesceClaudeConfig(timeoutSeconds int) map[string]any {
	if timeoutSeconds <= 0 {
		return map[string]any{}
	}
	return map[string]any{"timeout_seconds": timeoutSeconds}
}
