-- §7(b) runner → central machine identity. `flowctl runner register` makes the
-- central mint a long-lived runner token; only its SHA-256 hash is stored here.
-- The raw token is returned once (in the register response) and lives on the
-- runner as a Docker secret. The runner-token middleware hashes the presented
-- bearer and looks the runner up by this column, scoping the token to
-- runner-write endpoints only (§7(b): "Scope vain runner-endpointteihin").
--
-- Nullable: rows predating issue #6 (and the env-bootstrapped runner) carry no
-- token. A NULL hash never matches a presented bearer — the lookup filters
-- NULLs out via the partial index — so a tokenless runner is fail-closed by
-- construction (it can never authenticate a runner-write request).

BEGIN;

ALTER TABLE runner ADD COLUMN token_hash bytea;

-- At most one runner per token, but many NULLs allowed (multiple tokenless
-- legacy/bootstrap rows must coexist).
CREATE UNIQUE INDEX runner_token_hash_idx
    ON runner (token_hash)
    WHERE token_hash IS NOT NULL;

COMMIT;
