-- §7(a) human → central auth. flowctl runs `flowctl login`, the central
-- exchanges a GitHub device-flow code for a github user, upserts an app_user
-- row, then mints an opaque session token whose SHA-256 hash is stored here.
-- The raw token only ever leaves the central in the /v1/auth/device/poll
-- response; afterwards we only ever compare hashes.
--
-- Token is opaque (random bytes, hex-encoded) rather than a JWT: a self-hosted
-- Tailscale deploy has no need for stateless verification, and revocation is a
-- single DELETE.

BEGIN;

CREATE TABLE user_session (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id      uuid NOT NULL REFERENCES app_user(id) ON DELETE CASCADE,
    token_hash   bytea NOT NULL,
    expires_at   timestamptz NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    last_used_at timestamptz,
    UNIQUE (token_hash)
);

CREATE INDEX user_session_user_idx    ON user_session (user_id);
CREATE INDEX user_session_expires_idx ON user_session (expires_at);

COMMIT;
