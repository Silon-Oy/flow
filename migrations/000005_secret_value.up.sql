-- Issue #10 (§9): pgcrypto-backed secret store. The `pgcrypto` extension is
-- already enabled in 000001 (for gen_random_uuid()), so we only add the new
-- table here. The symmetric key lives in the central's Docker secret
-- (FLOW_SECRETS_DB_KEY); the central never logs it. Rotation is a re-encrypt
-- against the new key + commit — handled by a future admin CLI, not here.
--
-- One row per secret_ref whose store='postgres'. CASCADE on the FK so deleting
-- the ref also drops the ciphertext (no orphaned encrypted blobs).
--
-- §11.3 invariant boundary: this table holds the *value*; whether it is
-- delivered to a container as env or via the proxy is the secret_ref.delivery
-- column. Only delivery='env' materializes at lease time today; delivery='proxy'
-- is schema-valid but runtime returns "not yet supported" until the proxy-
-- injection issue lands.

BEGIN;

CREATE TABLE secret_value (
    ref_id      uuid PRIMARY KEY REFERENCES secret_ref(id) ON DELETE CASCADE,
    ciphertext  bytea NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);

COMMIT;
