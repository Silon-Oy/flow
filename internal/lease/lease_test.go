package lease

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Silon-Oy/flow/internal/store"
)

// setup migrates the test DB and returns a pool plus a freshly-seeded tenant +
// project + runner. Each test gets a unique tenant so they don't interfere.
func setup(t *testing.T) (*pgxpool.Pool, string, string) {
	t.Helper()
	dsn := os.Getenv("FLOW_TEST_DSN")
	if dsn == "" {
		t.Skip("FLOW_TEST_DSN not set — skipping lease integration test")
	}
	if err := store.Migrate(dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)

	var tenantID, projectID, runnerID string
	name := fmt.Sprintf("t-%d", time.Now().UnixNano())
	if err := pool.QueryRow(ctx,
		`INSERT INTO tenant (name) VALUES ($1) RETURNING id::text`, name).Scan(&tenantID); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO project (tenant_id, name, owner_repo) VALUES ($1, $2, 'o/r') RETURNING id::text`,
		tenantID, name).Scan(&projectID); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO runner (tenant_id, hostname) VALUES ($1, 'host') RETURNING id::text`,
		tenantID).Scan(&runnerID); err != nil {
		t.Fatalf("seed runner: %v", err)
	}
	return pool, tenantID, runnerID
}

func seedWork(t *testing.T, pool *pgxpool.Pool, tenantID, projectID, workKey string, issue int) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`INSERT INTO claimable_work (tenant_id, project_id, work_key, remote, issue_number, kind)
		 VALUES ($1, $2, $3, 'origin', $4, 'develop')`,
		tenantID, projectID, workKey, issue)
	if err != nil {
		t.Fatalf("seed work %s: %v", workKey, err)
	}
}

func projectFor(t *testing.T, pool *pgxpool.Pool, tenantID string) string {
	t.Helper()
	var pid string
	if err := pool.QueryRow(context.Background(),
		`SELECT id::text FROM project WHERE tenant_id = $1 LIMIT 1`, tenantID).Scan(&pid); err != nil {
		t.Fatalf("project lookup: %v", err)
	}
	return pid
}

// TestAcquireReleaseRoundtrip: a single work item is acquired once; a second
// acquire returns ErrNoWork; after release+reaping it can be re-acquired.
func TestAcquireReleaseRoundtrip(t *testing.T) {
	pool, tenantID, runnerID := setup(t)
	ctx := context.Background()
	projectID := projectFor(t, pool, tenantID)
	seedWork(t, pool, tenantID, projectID, "wk-1-"+tenantID, 1)

	m := NewManager(pool)
	l, w, err := m.Acquire(ctx, tenantID, runnerID, []string{"develop"})
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if w.IssueNumber != 1 {
		t.Errorf("issue = %d, want 1", w.IssueNumber)
	}

	// Second acquire finds nothing (the only work is leased).
	if _, _, err := m.Acquire(ctx, tenantID, runnerID, []string{"develop"}); err != ErrNoWork {
		t.Errorf("second acquire = %v, want ErrNoWork", err)
	}

	// Heartbeat extends the active lease.
	ok, err := m.Heartbeat(ctx, tenantID, l.ID)
	if err != nil || !ok {
		t.Errorf("heartbeat = %v,%v want true,nil", ok, err)
	}

	// Cross-tenant heartbeat MUST silently match zero rows — proves the §7
	// boundary is enforced in SQL, not just at the handler layer.
	if okOther, err := m.Heartbeat(ctx, "00000000-0000-0000-0000-000000000000", l.ID); err != nil || okOther {
		t.Errorf("cross-tenant heartbeat = %v,%v want false,nil", okOther, err)
	}

	// Release returns the work to the queue.
	if err := m.Release(ctx, tenantID, l.ID); err != nil {
		t.Fatalf("release: %v", err)
	}
	if _, _, err := m.Acquire(ctx, tenantID, runnerID, []string{"develop"}); err != nil {
		t.Errorf("re-acquire after release = %v, want nil", err)
	}
}

// TestNoSplitBrain is the load-bearing invariant: N goroutines racing to acquire
// the SAME single work item produce EXACTLY ONE winner. This is the property
// the whole lease protocol exists to guarantee (§5: two runners can never get
// the same work).
func TestNoSplitBrain(t *testing.T) {
	pool, tenantID, runnerID := setup(t)
	ctx := context.Background()
	projectID := projectFor(t, pool, tenantID)
	seedWork(t, pool, tenantID, projectID, "wk-race-"+tenantID, 7)

	m := NewManager(pool)
	const racers = 20
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins := 0
	noWork := 0
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l, _, err := m.Acquire(ctx, tenantID, runnerID, []string{"develop"})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil && l != nil:
				wins++
			case err == ErrNoWork:
				noWork++
			default:
				t.Errorf("unexpected acquire error: %v", err)
			}
		}()
	}
	wg.Wait()

	if wins != 1 {
		t.Errorf("split-brain: %d goroutines won the same work (want exactly 1)", wins)
	}
	if noWork != racers-1 {
		t.Errorf("expected %d ErrNoWork, got %d", racers-1, noWork)
	}
}

// TestReapReturnsExpiredWork: an un-heartbeated lease expires; reaping marks it
// reaped and the work is claimable again (the crashed-runner recovery path).
func TestReapReturnsExpiredWork(t *testing.T) {
	pool, tenantID, runnerID := setup(t)
	ctx := context.Background()
	projectID := projectFor(t, pool, tenantID)
	seedWork(t, pool, tenantID, projectID, "wk-reap-"+tenantID, 9)

	// 1ms TTL so the lease expires effectively immediately.
	m := NewManager(pool).WithTTL(time.Millisecond)
	if _, _, err := m.Acquire(ctx, tenantID, runnerID, []string{"develop"}); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	time.Sleep(20 * time.Millisecond)

	n, err := m.Reap(ctx)
	if err != nil {
		t.Fatalf("reap: %v", err)
	}
	if n != 1 {
		t.Errorf("reaped %d, want 1", n)
	}

	// Work is claimable again after reaping.
	if _, _, err := m.Acquire(ctx, tenantID, runnerID, []string{"develop"}); err != nil {
		t.Errorf("re-acquire after reap = %v, want nil", err)
	}
}
