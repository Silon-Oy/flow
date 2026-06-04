BEGIN;

DROP INDEX IF EXISTS runner_token_hash_idx;
ALTER TABLE runner DROP COLUMN IF EXISTS token_hash;

COMMIT;
