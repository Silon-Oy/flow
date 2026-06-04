-- §7 RBAC seam: each run is owned by the developer who started it, so a
-- developer can list "my runs" while admin lists the whole tenant. Pre-RBAC
-- runs (created before the column existed) stay NULL — handlers treat NULL
-- as "no owner" and only admin sees them.

BEGIN;

ALTER TABLE run
    ADD COLUMN app_user_id uuid REFERENCES app_user(id) ON DELETE SET NULL;

CREATE INDEX run_app_user_idx ON run (app_user_id);

COMMIT;
