package runstate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Silon-Oy/flow/internal/store"
)

func setup(t *testing.T) (*Store, string, string) {
	t.Helper()
	dsn := os.Getenv("FLOW_TEST_DSN")
	if dsn == "" {
		t.Skip("FLOW_TEST_DSN not set — skipping runstate integration test")
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

	name := fmt.Sprintf("rs-%d", time.Now().UnixNano())
	var tenantID, projectID string
	if err := pool.QueryRow(ctx, `INSERT INTO tenant (name) VALUES ($1) RETURNING id::text`, name).Scan(&tenantID); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO project (tenant_id, name, owner_repo) VALUES ($1, $2, 'o/r') RETURNING id::text`, tenantID, name).Scan(&projectID); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	return New(pool), tenantID, projectID
}

func strp(s string) *string { return &s }

func TestRunLifecycleAndEvents(t *testing.T) {
	s, tenantID, projectID := setup(t)
	ctx := context.Background()

	runID, err := s.CreateRun(ctx, tenantID, projectID, "origin", 42)
	if err != nil {
		t.Fatalf("create run: %v", err)
	}

	r, err := s.GetRun(ctx, tenantID, runID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if r.Status != StatusInitialized {
		t.Errorf("status = %q, want initialized", r.Status)
	}
	if r.IssueNumber != 42 {
		t.Errorf("issue = %d, want 42", r.IssueNumber)
	}

	// PATCH: only branch + current_state; status untouched.
	if err := s.PatchRun(ctx, tenantID, runID, Patch{Branch: strp("auto-run/issue-42"), CurrentState: strp("S8_Implement")}); err != nil {
		t.Fatalf("patch1: %v", err)
	}
	r, _ = s.GetRun(ctx, tenantID, runID)
	if r.Branch == nil || *r.Branch != "auto-run/issue-42" {
		t.Errorf("branch not patched: %v", r.Branch)
	}
	if r.Status != StatusInitialized {
		t.Errorf("status changed unexpectedly to %q (COALESCE leak)", r.Status)
	}

	// PATCH: finalize as completed.
	completed := StatusCompleted
	if err := s.PatchRun(ctx, tenantID, runID, Patch{Status: &completed, PRURL: strp("https://github.com/o/r/pull/1"), Finished: true}); err != nil {
		t.Fatalf("patch2: %v", err)
	}
	r, _ = s.GetRun(ctx, tenantID, runID)
	if r.Status != StatusCompleted {
		t.Errorf("status = %q, want completed", r.Status)
	}
	if r.FinishedAt == nil {
		t.Errorf("finished_at not set")
	}

	// Events: batch append + read back in order.
	events := []Event{
		{Event: "claimed", Data: json.RawMessage(`{"work_key":"wk-42"}`)},
		{Event: "cycle_review_decision", Data: json.RawMessage(`{"decision":"PROCEED"}`)},
		{Event: "implementer_result", Data: json.RawMessage(`{"result":"SUCCESS"}`)},
	}
	if err := s.AppendEvents(ctx, tenantID, runID, events); err != nil {
		t.Fatalf("append events: %v", err)
	}
	got, err := s.ListEvents(ctx, tenantID, runID)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("events = %d, want 3", len(got))
	}
	if got[0].Event != "claimed" || got[2].Event != "implementer_result" {
		t.Errorf("event order wrong: %v", []string{got[0].Event, got[1].Event, got[2].Event})
	}

	// Empty batch is a no-op.
	if err := s.AppendEvents(ctx, tenantID, runID, nil); err != nil {
		t.Errorf("empty batch should be no-op: %v", err)
	}

	// Cross-tenant access MUST surface ErrNotFound — existence does not leak.
	other := "00000000-0000-0000-0000-000000000000"
	if _, err := s.GetRun(ctx, other, runID); !errors.Is(err, ErrNotFound) {
		t.Errorf("cross-tenant GetRun err = %v, want ErrNotFound", err)
	}
	if err := s.PatchRun(ctx, other, runID, Patch{CurrentState: strp("S0")}); !errors.Is(err, ErrNotFound) {
		t.Errorf("cross-tenant PatchRun err = %v, want ErrNotFound", err)
	}
	if err := s.AppendEvents(ctx, other, runID, []Event{{Event: "noop"}}); !errors.Is(err, ErrNotFound) {
		t.Errorf("cross-tenant AppendEvents err = %v, want ErrNotFound", err)
	}
	if evs, err := s.ListEvents(ctx, other, runID); err != nil || len(evs) != 0 {
		t.Errorf("cross-tenant ListEvents = (%d events, %v), want (0, nil)", len(evs), err)
	}
}
