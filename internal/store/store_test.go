package store

import (
	"context"
	"os"
	"testing"
)

// testDSN returns the integration DSN from FLOW_TEST_DSN, or skips the test.
// CI / local runs set FLOW_TEST_DSN to a throwaway Postgres 16.
func testDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("FLOW_TEST_DSN")
	if dsn == "" {
		t.Skip("FLOW_TEST_DSN not set — skipping Postgres integration test")
	}
	return dsn
}

// TestMigrateAndOpen applies the schema and verifies the expected tables exist.
// It is idempotent: a second Migrate must be a no-op (ErrNoChange swallowed).
func TestMigrateAndOpen(t *testing.T) {
	dsn := testDSN(t)
	if err := Migrate(dsn); err != nil {
		t.Fatalf("first migrate: %v", err)
	}
	if err := Migrate(dsn); err != nil {
		t.Fatalf("second migrate (should be no-op): %v", err)
	}

	ctx := context.Background()
	st, err := Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()

	wantTables := []string{
		"tenant", "app_user", "project", "runner", "claimable_work",
		"lease", "run", "run_event", "github_app_install", "secret_ref", "egress_log",
	}
	for _, tbl := range wantTables {
		var exists bool
		err := st.Pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = $1)`, tbl).
			Scan(&exists)
		if err != nil {
			t.Fatalf("check table %s: %v", tbl, err)
		}
		if !exists {
			t.Errorf("expected table %q to exist after migrate", tbl)
		}
	}
}
