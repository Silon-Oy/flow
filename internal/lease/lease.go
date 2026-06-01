// Package lease implements the centralized work-ownership protocol (§5),
// replacing the bash mkdir-lock + @me-assignment arbiter.
//
// The database is the arbiter: Acquire selects an unclaimed claimable_work row
// FOR UPDATE SKIP LOCKED and inserts an active lease in one transaction. Two
// runners can never obtain the same work — no race, no @me verification, no
// sleep-patching.
//
// Invariants (§5, §10):
//   - At most one ACTIVE lease per work_key (enforced by a partial unique index
//     AND the SKIP LOCKED select).
//   - A lease expires after TTL unless heartbeated; a crashed runner stops
//     heartbeating, the lease expires, and the work returns to the queue.
//   - Acquire is fail-closed: if the central DB is unreachable, no work is
//     handed out (the caller must surface the error, never fall back to a
//     second arbiter).
package lease

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Default protocol parameters (§5).
const (
	DefaultTTL           = 15 * time.Minute
	DefaultHeartbeatTTL  = 15 * time.Minute // each heartbeat re-extends by TTL
	HeartbeatInterval    = 60 * time.Second
	DefaultReapInterval  = 60 * time.Second
)

// ErrNoWork is returned by Acquire when no claimable work is available for the
// requested kinds. It is NOT an error condition — the runner backs off and
// retries. Distinguished from a DB error so the caller does not fail-closed
// spuriously.
var ErrNoWork = errors.New("lease: no claimable work available")

// Work describes the unit a lease authorizes.
type Work struct {
	WorkKey     string
	ProjectID   string
	TenantID    string
	Remote      string
	IssueNumber int
	Kind        string
}

// Lease is an active ownership record.
type Lease struct {
	ID        string
	WorkKey   string
	RunnerID  string
	TenantID  string
	Status    string
	ExpiresAt time.Time
}

// Manager owns the lease lifecycle against a pgx pool.
type Manager struct {
	pool *pgxpool.Pool
	ttl  time.Duration
}

// NewManager returns a Manager with the default TTL.
func NewManager(pool *pgxpool.Pool) *Manager {
	return &Manager{pool: pool, ttl: DefaultTTL}
}

// WithTTL overrides the lease TTL (used by tests to exercise expiry quickly).
func (m *Manager) WithTTL(ttl time.Duration) *Manager {
	m.ttl = ttl
	return m
}

// Acquire atomically claims the oldest unclaimed work matching any of kinds for
// the given tenant and assigns it to runnerID. Returns ErrNoWork when nothing
// is claimable. The whole operation is one transaction so SKIP LOCKED makes the
// DB the sole arbiter.
func (m *Manager) Acquire(ctx context.Context, tenantID, runnerID string, kinds []string) (*Lease, *Work, error) {
	tx, err := m.pool.Begin(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var w Work
	err = tx.QueryRow(ctx, `
		SELECT w.work_key, w.project_id::text, w.tenant_id::text, w.remote, w.issue_number, w.kind
		  FROM claimable_work w
		 WHERE w.tenant_id = $1
		   AND w.kind = ANY($2)
		   AND NOT EXISTS (
		       SELECT 1 FROM lease l
		        WHERE l.work_key = w.work_key
		          AND l.status = 'active'
		          AND l.expires_at > now())
		 ORDER BY w.created_at ASC
		 FOR UPDATE OF w SKIP LOCKED
		 LIMIT 1`,
		tenantID, kinds,
	).Scan(&w.WorkKey, &w.ProjectID, &w.TenantID, &w.Remote, &w.IssueNumber, &w.Kind)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, ErrNoWork
	}
	if err != nil {
		return nil, nil, err
	}

	var l Lease
	err = tx.QueryRow(ctx, `
		INSERT INTO lease (tenant_id, work_key, runner_id, status, acquired_at, expires_at)
		VALUES ($1, $2, $3, 'active', now(), now() + make_interval(secs => $4))
		RETURNING id::text, work_key, runner_id::text, tenant_id::text, status, expires_at`,
		tenantID, w.WorkKey, runnerID, m.ttl.Seconds(),
	).Scan(&l.ID, &l.WorkKey, &l.RunnerID, &l.TenantID, &l.Status, &l.ExpiresAt)
	if err != nil {
		return nil, nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, nil, err
	}
	return &l, &w, nil
}

// Heartbeat re-extends an active lease's expiry by TTL. Returns false if the
// lease no longer exists or is not active (e.g. already reaped) — the caller
// must then stop work or re-acquire.
func (m *Manager) Heartbeat(ctx context.Context, leaseID string) (bool, error) {
	tag, err := m.pool.Exec(ctx, `
		UPDATE lease
		   SET expires_at = now() + make_interval(secs => $2)
		 WHERE id = $1 AND status = 'active' AND expires_at > now()`,
		leaseID, m.ttl.Seconds())
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// Release marks an active lease as released so the work can be re-claimed (or
// removed by the scanner). Idempotent.
func (m *Manager) Release(ctx context.Context, leaseID string) error {
	_, err := m.pool.Exec(ctx,
		`UPDATE lease SET status = 'released' WHERE id = $1 AND status = 'active'`, leaseID)
	return err
}

// Reap marks every expired active lease as reaped, returning the count. Run on
// a background ticker (DefaultReapInterval). This is the liveness mechanism: a
// crashed runner's lease expires and is reaped, returning the work to the queue.
func (m *Manager) Reap(ctx context.Context) (int64, error) {
	tag, err := m.pool.Exec(ctx,
		`UPDATE lease SET status = 'reaped' WHERE status = 'active' AND expires_at <= now()`)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
