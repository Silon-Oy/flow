// Package secrets is the seam §9 specs: secrets are *references*, never values.
// The resolver hands a SECRET_REF.path (the value stored in DB) back to a
// concrete bytes payload at use-time, so the broker (e.g. internal/githubapp)
// never embeds a key store decision.
//
// Vaihe 1 ships only EnvResolver: it treats `path` as an environment variable
// name and reads its value from the central process env. That matches the bash
// `github-app-auth.sh` model (one App key in env) and keeps the seam alive for
// issue #10, which will add a pgcrypto-backed resolver behind the same
// interface.
package secrets

import (
	"errors"
	"fmt"
	"os"
)

// ErrNotFound is returned when a SECRET_REF.path cannot be resolved.
var ErrNotFound = errors.New("secret not found")

// Resolver hands the raw bytes for a given SECRET_REF.path back to the caller.
// Concrete implementations decide what `ref` means (env var name today,
// pgcrypto row id later). Implementations MUST NOT log the returned bytes.
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
