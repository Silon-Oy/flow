-- Flow central schema (§6 data model).
--
-- Vaihe 1 is single-tenant in DATA, but tenant_id is present throughout so the
-- multi-tenancy of Vaihe 2 is a data-population change, not a schema migration.
-- A bootstrap tenant + a bootstrap project are NOT inserted here; the central
-- service seeds them (or a future wizard does) so this migration stays purely
-- structural.

BEGIN;

CREATE EXTENSION IF NOT EXISTS pgcrypto; -- gen_random_uuid()

-- ── TENANT ────────────────────────────────────────────────────────────────
CREATE TABLE tenant (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name        text NOT NULL UNIQUE,
    created_at  timestamptz NOT NULL DEFAULT now()
);

-- ── ROLE (enum) + USER ──────────────────────────────────────────────────────
-- Roles are admin | developer (§7). The schema is present in Vaihe 1; RBAC
-- enforcement (middleware) lands in Vaihe 2.
CREATE TYPE user_role AS ENUM ('admin', 'developer');

CREATE TABLE app_user (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   uuid NOT NULL REFERENCES tenant(id) ON DELETE CASCADE,
    github_login text NOT NULL,
    role        user_role NOT NULL DEFAULT 'developer',
    created_at  timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, github_login)
);

-- ── PROJECT ─────────────────────────────────────────────────────────────────
CREATE TABLE project (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id    uuid NOT NULL REFERENCES tenant(id) ON DELETE CASCADE,
    name         text NOT NULL,
    owner_repo   text NOT NULL,
    remotes      jsonb NOT NULL DEFAULT '[]'::jsonb,
    labels       jsonb NOT NULL DEFAULT '["auto-run"]'::jsonb,
    base_branch  text NOT NULL DEFAULT 'main',
    runner_pool  uuid,
    secret_refs  jsonb NOT NULL DEFAULT '{}'::jsonb,
    merge_policy jsonb NOT NULL DEFAULT '{}'::jsonb,
    claude_config jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at   timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, name)
);

-- ── RUNNER ────────────────────────────────────────────────────────────────
-- Replaces the hostname-hardcoded Studio role: a runner registers and reports
-- capacity. status is free-text (online/offline/draining) kept as text to
-- avoid an enum migration when states evolve.
CREATE TABLE runner (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id      uuid NOT NULL REFERENCES tenant(id) ON DELETE CASCADE,
    hostname       text NOT NULL,
    capacity       int NOT NULL DEFAULT 1,
    active_leases  int NOT NULL DEFAULT 0,
    last_heartbeat timestamptz,
    status         text NOT NULL DEFAULT 'online',
    capabilities   jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at     timestamptz NOT NULL DEFAULT now()
);

-- ── CLAIMABLE_WORK ──────────────────────────────────────────────────────────
-- The queue the scanner populates from GitHub auto-run issues. work_key is the
-- uniqueness key the lease arbitrates on:
--   (tenant_id, project_id, remote, issue_number, kind) -> a stable text key.
CREATE TABLE claimable_work (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id    uuid NOT NULL REFERENCES tenant(id) ON DELETE CASCADE,
    project_id   uuid NOT NULL REFERENCES project(id) ON DELETE CASCADE,
    work_key     text NOT NULL UNIQUE,
    remote       text NOT NULL DEFAULT 'origin',
    issue_number int NOT NULL,
    kind         text NOT NULL DEFAULT 'develop', -- develop | pr_watch | clean
    created_at   timestamptz NOT NULL DEFAULT now()
);

-- ── LEASE ─────────────────────────────────────────────────────────────────
-- The atomic ownership record. The acquire path selects an unclaimed
-- claimable_work row FOR UPDATE SKIP LOCKED and inserts an active lease; the DB
-- is the arbiter (§5). A partial unique index enforces "at most one ACTIVE
-- lease per work_key" so a buggy double-insert cannot create a split lease.
CREATE TABLE lease (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   uuid NOT NULL REFERENCES tenant(id) ON DELETE CASCADE,
    work_key    text NOT NULL REFERENCES claimable_work(work_key) ON DELETE CASCADE,
    runner_id   uuid NOT NULL REFERENCES runner(id) ON DELETE CASCADE,
    status      text NOT NULL DEFAULT 'active', -- active | released | reaped
    acquired_at timestamptz NOT NULL DEFAULT now(),
    expires_at  timestamptz NOT NULL
);

CREATE UNIQUE INDEX lease_one_active_per_work
    ON lease (work_key)
    WHERE status = 'active';

CREATE INDEX lease_expiry_idx ON lease (expires_at) WHERE status = 'active';

-- ── RUN ─────────────────────────────────────────────────────────────────────
-- Generalised run.json. The status enum is preserved verbatim from the bash
-- orchestrator (lib/state.sh).
CREATE TYPE run_status AS ENUM (
    'initialized', 'completed', 'blocked', 'lost_race', 'cancelled',
    'merged', 'pr_conflicted', 'timed_out', 'awaiting_clarification'
);

CREATE TABLE run (
    id                  uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id           uuid NOT NULL REFERENCES tenant(id) ON DELETE CASCADE,
    project_id          uuid NOT NULL REFERENCES project(id) ON DELETE CASCADE,
    runner_id           uuid REFERENCES runner(id) ON DELETE SET NULL,
    lease_id            uuid REFERENCES lease(id) ON DELETE SET NULL,
    remote              text NOT NULL DEFAULT 'origin',
    issue_number        int NOT NULL,
    status              run_status NOT NULL DEFAULT 'initialized',
    current_state       text,
    branch              text,
    pr_url              text,
    blocked_reason      text,
    retry_count         int NOT NULL DEFAULT 0,
    timeout_phase       text,
    clarification_round int NOT NULL DEFAULT 0,
    started_at          timestamptz NOT NULL DEFAULT now(),
    finished_at         timestamptz
);

CREATE INDEX run_project_idx ON run (project_id);
CREATE INDEX run_status_idx  ON run (status);

-- ── RUN_EVENT ─────────────────────────────────────────────────────────────
-- state.jsonl, row-per-row (event + JSONB data + ts). Append-only telemetry.
-- seq is a monotonic insertion counter: when two events share a ts (a batch
-- inserted within the same clock tick), seq preserves the order the runner
-- emitted them. ORDER BY (ts, seq) is therefore a stable chronological-then-
-- insertion order, which a random uuid PK cannot provide.
CREATE TABLE run_event (
    id     uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    seq    bigserial NOT NULL,
    run_id uuid NOT NULL REFERENCES run(id) ON DELETE CASCADE,
    event  text NOT NULL,
    data   jsonb NOT NULL DEFAULT '{}'::jsonb,
    ts     timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX run_event_run_idx ON run_event (run_id, ts, seq);

-- ── GITHUB_APP_INSTALL ──────────────────────────────────────────────────────
-- Per tenant/org App installation. private_key_ref is a SECRET_REF pointer, not
-- the key itself (secrets are references, never values).
CREATE TABLE github_app_install (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id       uuid NOT NULL REFERENCES tenant(id) ON DELETE CASCADE,
    org             text NOT NULL,
    app_id          bigint NOT NULL,
    installation_id bigint NOT NULL,
    private_key_ref text NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, org)
);

-- ── SECRET_REF ──────────────────────────────────────────────────────────────
-- A reference to a secret, never the value. delivery ∈ {proxy, env} (§9).
CREATE TABLE secret_ref (
    id        uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id uuid NOT NULL REFERENCES tenant(id) ON DELETE CASCADE,
    key       text NOT NULL,
    store     text NOT NULL DEFAULT 'postgres',
    path      text NOT NULL,
    delivery  text NOT NULL DEFAULT 'env', -- proxy | env
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, key)
);

-- ── EGRESS_LOG ──────────────────────────────────────────────────────────────
-- §11.6: proxy logs {lease_id, tenant_id, run_id, host, allowed|denied, ts}
-- (never content / credentials). Surfaced read-only to the admin dashboard.
CREATE TABLE egress_log (
    id        uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id uuid REFERENCES tenant(id) ON DELETE CASCADE,
    run_id    uuid REFERENCES run(id) ON DELETE CASCADE,
    lease_id  uuid REFERENCES lease(id) ON DELETE SET NULL,
    host      text NOT NULL,
    allowed   boolean NOT NULL,
    ts        timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX egress_log_run_idx ON egress_log (run_id, ts);

COMMIT;
