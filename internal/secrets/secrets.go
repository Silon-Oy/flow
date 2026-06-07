// Package secrets is the seam §9 specs: secrets are *references*, never values.
// The resolver hands a SECRET_REF.path (the value stored in DB) back to a
// concrete bytes payload at use-time, so the broker (e.g. internal/githubapp)
// never embeds a key store decision.
//
// Two concrete resolvers ship:
//
//   - EnvResolver — Vaihe 1's seam-keeper. `ref` is an env var name read from
//     the central process env. Matches the bash `github-app-auth.sh` model
//     (one App key in env).
//   - PGCryptoResolver — Issue #10. `ref` is a secret_ref.id (uuid string);
//     the resolver decrypts secret_value.ciphertext with the central's
//     symmetric key (held in a Docker secret), so the plaintext only ever
//     lives in process memory.
//
// Registry routes Resolve calls to the right backend based on the
// `secret_ref.store` column ({"env","postgres"}). Callers that work over a
// secret_ref row use Registry; the legacy github-app broker still talks to a
// concrete Resolver because its `private_key_ref` column predates the store
// concept (env-only).
package secrets

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound is returned when a SECRET_REF cannot be resolved (missing row,
// unset env var, unknown store, etc.). Callers map to HTTP 404 / fail-closed.
var ErrNotFound = errors.New("secret not found")

// ErrDeliveryNotSupported is the runtime answer for delivery values whose
// implementation has not yet landed. The schema accepts the value; the runtime
// fails closed so a misconfigured ref cannot silently turn into a no-op. Issue
// #10 ships only delivery='env'; delivery='proxy' returns this until the
// proxy-injection broker lands.
var ErrDeliveryNotSupported = errors.New("delivery not supported")

// forbiddenEnvKeys: the central MUST NOT materialize these as container env
// vars. The GitHub credentials path is §11.3 proxy-injection, not env-bound,
// so a delivery='env' secret named GITHUB_TOKEN / GH_TOKEN would silently
// break the §11.3 invariant. Defense-in-depth: the POST /v1/secrets handler
// rejects them at the API surface, and runnerexec drops them again at render
// time so a future config-path that skips the handler still cannot leak them.
var forbiddenEnvKeys = map[string]bool{
	"GITHUB_TOKEN": true,
	"GH_TOKEN":     true,
}

// IsForbiddenEnvKey reports whether key would violate §11.3 if materialized as
// a container env var. Exported so the API handler and runnerexec.Spec share
// the single source of truth.
func IsForbiddenEnvKey(key string) bool {
	return forbiddenEnvKeys[key]
}

// Resolver hands the raw bytes for a given SECRET_REF.path back to the caller.
// Concrete implementations decide what `ref` means (env var name, secret_ref
// uuid, …). Implementations MUST NOT log the returned bytes.
type Resolver interface {
	Resolve(ref string) ([]byte, error)
}

// EnvResolver treats `ref` as an environment variable name. An empty value is
// reported as ErrNotFound so callers can distinguish "set but blank" from
// "not configured" without inspecting strings.
type EnvResolver struct{}

// Resolve looks up ref in the process environment.
func (EnvResolver) Resolve(ref string) ([]byte, error) {
	if ref == "" {
		return nil, fmt.Errorf("%w: empty ref", ErrNotFound)
	}
	v, ok := os.LookupEnv(ref)
	if !ok || v == "" {
		return nil, fmt.Errorf("%w: env %s", ErrNotFound, ref)
	}
	return []byte(v), nil
}

// PGCryptoResolver resolves secret_ref rows whose store='postgres' by reading
// secret_value.ciphertext and decrypting with the symmetric Key (loaded from a
// Docker secret, never logged). `ref` is the secret_ref.id as a uuid string.
//
// The decryption happens DB-side via pgp_sym_decrypt — the key still leaves
// the process to ride the libpq connection, which is acceptable for the
// Tailscale self-host posture (§9 MVP). A future hardening swaps to client-
// side decryption (e.g. AES-GCM) without changing the interface.
type PGCryptoResolver struct {
	Pool *pgxpool.Pool
	// Key is the symmetric key (Docker secret). Caller owns its lifetime;
	// the resolver only reads it.
	Key []byte
	// Timeout caps individual decrypt queries. Zero => 5s default.
	Timeout time.Duration
}

// Resolve decrypts the secret_value row whose ref_id matches ref.
func (r *PGCryptoResolver) Resolve(ref string) ([]byte, error) {
	if ref == "" {
		return nil, fmt.Errorf("%w: empty ref", ErrNotFound)
	}
	if r.Pool == nil {
		return nil, fmt.Errorf("%w: pgcrypto resolver pool not configured", ErrNotFound)
	}
	if len(r.Key) == 0 {
		return nil, fmt.Errorf("%w: pgcrypto resolver key not configured", ErrNotFound)
	}
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	var plain []byte
	err := r.Pool.QueryRow(ctx, `
		SELECT pgp_sym_decrypt(ciphertext, $1)
		  FROM secret_value
		 WHERE ref_id = $2::uuid`,
		string(r.Key), ref,
	).Scan(&plain)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: postgres ref %s", ErrNotFound, ref)
	}
	if err != nil {
		// Wrap without leaking the key or ciphertext.
		return nil, fmt.Errorf("pgcrypto resolve: %w", err)
	}
	return plain, nil
}

// Registry routes Resolve calls to the right backend based on a `store` tag.
// It is the runtime form of the SECRET_REF.store abstraction (§9): a single
// caller can iterate secret_ref rows of mixed stores and pick the resolver per
// row without branching.
//
// A nil resolver for a store reports ErrNotFound at resolve-time (fail-closed)
// rather than panicking — this keeps a half-configured deploy from crashing.
type Registry struct {
	Env Resolver
	Pg  Resolver
}

// Resolve picks the resolver for store and forwards ref. Empty store defaults
// to "env" so the Vaihe 1 callers that predate the store column keep working.
func (r *Registry) Resolve(store, ref string) ([]byte, error) {
	switch store {
	case "", "env":
		if r.Env == nil {
			return nil, fmt.Errorf("%w: env resolver not configured", ErrNotFound)
		}
		return r.Env.Resolve(ref)
	case "postgres":
		if r.Pg == nil {
			return nil, fmt.Errorf("%w: postgres resolver not configured", ErrNotFound)
		}
		return r.Pg.Resolve(ref)
	default:
		return nil, fmt.Errorf("%w: unknown store %q", ErrNotFound, store)
	}
}

// SecretRef is the projection of a secret_ref row the materializer reads.
// Mirrored here (not imported from internal/store) to keep this package free
// of upstream cycles.
type SecretRef struct {
	ID       string // uuid as text
	Key      string // logical env-style name
	Store    string // {"env","postgres"}
	Path     string // env: variable name; postgres: ignored (ID is the lookup)
	Delivery string // {"env","proxy"}
}

// MaterializeEnv resolves a single secret_ref row whose delivery is "env" into
// (key, value). For delivery="proxy" it returns ErrDeliveryNotSupported — the
// proxy-injection path is a separate issue, and the cycle review requires the
// runtime to refuse rather than silently no-op.
//
// Forbidden env keys (GITHUB_TOKEN / GH_TOKEN) return ErrDeliveryNotSupported
// regardless of store: those credentials MUST ride the §11.3 proxy path.
func MaterializeEnv(reg *Registry, ref SecretRef) (key, value string, err error) {
	if reg == nil {
		return "", "", fmt.Errorf("%w: nil registry", ErrNotFound)
	}
	switch ref.Delivery {
	case "env":
		// fall through
	case "proxy":
		return "", "", fmt.Errorf("%w: delivery=proxy (use the egress-proxy injection path, §11.3)", ErrDeliveryNotSupported)
	default:
		return "", "", fmt.Errorf("%w: unknown delivery %q", ErrDeliveryNotSupported, ref.Delivery)
	}
	if IsForbiddenEnvKey(ref.Key) {
		return "", "", fmt.Errorf("%w: key %s must use delivery=proxy (§11.3)", ErrDeliveryNotSupported, ref.Key)
	}
	lookup := ref.Path
	if ref.Store == "postgres" {
		lookup = ref.ID
	}
	plain, err := reg.Resolve(ref.Store, lookup)
	if err != nil {
		return "", "", err
	}
	return ref.Key, string(plain), nil
}

// MaterializeAllEnvForTenant reads every delivery='env' secret_ref for the
// tenant and returns {key: value}. Used at lease-acquire time to populate the
// container env (§9 + §11.3 / runner side).
//
// Fail-closed: if any row fails to resolve, the whole map is discarded and
// the error is surfaced. A delivery='env' secret pointing to a missing env
// var or a key the central can't decrypt is a configuration error — silently
// dropping it would hand the run a partial environment and mask a real
// problem.
func MaterializeAllEnvForTenant(ctx context.Context, pool *pgxpool.Pool, reg *Registry, tenantID string) (map[string]string, error) {
	if pool == nil {
		return nil, errors.New("secrets.MaterializeAllEnvForTenant: nil pool")
	}
	if reg == nil {
		return nil, errors.New("secrets.MaterializeAllEnvForTenant: nil registry")
	}
	if tenantID == "" {
		return nil, errors.New("secrets.MaterializeAllEnvForTenant: empty tenant_id")
	}
	rows, err := pool.Query(ctx, `
		SELECT id::text, key, store, path, delivery
		  FROM secret_ref
		 WHERE tenant_id = $1
		   AND delivery = 'env'`,
		tenantID)
	if err != nil {
		return nil, fmt.Errorf("query secret_ref: %w", err)
	}
	defer rows.Close()

	out := map[string]string{}
	for rows.Next() {
		var ref SecretRef
		if err := rows.Scan(&ref.ID, &ref.Key, &ref.Store, &ref.Path, &ref.Delivery); err != nil {
			return nil, fmt.Errorf("scan secret_ref: %w", err)
		}
		key, value, err := MaterializeEnv(reg, ref)
		if err != nil {
			return nil, fmt.Errorf("materialize %s: %w", ref.Key, err)
		}
		out[key] = value
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
