// Package runstate is the central-side store for RUN and RUN_EVENT (§6),
// generalizing the bash run.json / state.jsonl. The runner pushes telemetry
// here (PATCH run, batched events); the dashboard reads it.
//
// The status enum is preserved verbatim from lib/state.sh.
package runstate

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Status is the run lifecycle enum, preserved from the bash orchestrator.
type Status string

const (
	StatusInitialized          Status = "initialized"
	StatusCompleted            Status = "completed"
	StatusBlocked              Status = "blocked"
	StatusLostRace             Status = "lost_race"
	StatusCancelled            Status = "cancelled"
	StatusMerged               Status = "merged"
	StatusPRConflicted         Status = "pr_conflicted"
	StatusTimedOut             Status = "timed_out"
	StatusAwaitingClarification Status = "awaiting_clarification"
)

// Run mirrors the RUN entity.
type Run struct {
	ID                 string     `json:"id"`
	TenantID           string     `json:"tenant_id"`
	ProjectID          string     `json:"project_id"`
	RunnerID           *string    `json:"runner_id,omitempty"`
	LeaseID            *string    `json:"lease_id,omitempty"`
	Remote             string     `json:"remote"`
	IssueNumber        int        `json:"issue_number"`
	Status             Status     `json:"status"`
	CurrentState       *string    `json:"current_state,omitempty"`
	Branch             *string    `json:"branch,omitempty"`
	PRURL              *string    `json:"pr_url,omitempty"`
	BlockedReason      *string    `json:"blocked_reason,omitempty"`
	RetryCount         int        `json:"retry_count"`
	TimeoutPhase       *string    `json:"timeout_phase,omitempty"`
	ClarificationRound int        `json:"clarification_round"`
	StartedAt          time.Time  `json:"started_at"`
	FinishedAt         *time.Time `json:"finished_at,omitempty"`
}

// Event mirrors a RUN_EVENT row (state.jsonl line).
type Event struct {
	Event string          `json:"event"`
	Data  json.RawMessage `json:"data,omitempty"`
	TS    time.Time       `json:"ts"`
}

// Patch carries optional run fields to update (telemetry PATCH). A nil pointer
// leaves the field unchanged.
type Patch struct {
	Status             *Status
	CurrentState       *string
	Branch             *string
	PRURL              *string
	BlockedReason      *string
	RetryCount         *int
	TimeoutPhase       *string
	ClarificationRound *int
	RunnerID           *string
	LeaseID            *string
	Finished           bool // when true, sets finished_at = now()
}

// Store persists runs and events.
type Store struct {
	pool *pgxpool.Pool
}

// New returns a runstate Store over the given pool.
func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// CreateRun inserts a new run in the initialized state and returns its id.
func (s *Store) CreateRun(ctx context.Context, tenantID, projectID, remote string, issueNumber int) (string, error) {
	var id string
	err := s.pool.QueryRow(ctx, `
		INSERT INTO run (tenant_id, project_id, remote, issue_number, status, current_state)
		VALUES ($1, $2, $3, $4, 'initialized', 'S0_Idle')
		RETURNING id::text`,
		tenantID, projectID, remote, issueNumber).Scan(&id)
	return id, err
}

// PatchRun applies the non-nil fields of p to the run atomically.
func (s *Store) PatchRun(ctx context.Context, runID string, p Patch) error {
	// Build a COALESCE-based update so only provided fields change. Each $N is a
	// pointer; pgx encodes a nil pointer as SQL NULL, and COALESCE(NULL, col)
	// keeps the existing value.
	_, err := s.pool.Exec(ctx, `
		UPDATE run SET
		  status              = COALESCE($2, status),
		  current_state       = COALESCE($3, current_state),
		  branch              = COALESCE($4, branch),
		  pr_url              = COALESCE($5, pr_url),
		  blocked_reason      = COALESCE($6, blocked_reason),
		  retry_count         = COALESCE($7, retry_count),
		  timeout_phase       = COALESCE($8, timeout_phase),
		  clarification_round = COALESCE($9, clarification_round),
		  runner_id           = COALESCE($10::uuid, runner_id),
		  lease_id            = COALESCE($11::uuid, lease_id),
		  finished_at         = CASE WHEN $12 THEN now() ELSE finished_at END
		WHERE id = $1`,
		runID,
		statusArg(p.Status),
		p.CurrentState, p.Branch, p.PRURL, p.BlockedReason,
		p.RetryCount, p.TimeoutPhase, p.ClarificationRound,
		p.RunnerID, p.LeaseID, p.Finished)
	return err
}

// statusArg converts a *Status to a *string for the query (nil stays nil).
func statusArg(s *Status) *string {
	if s == nil {
		return nil
	}
	v := string(*s)
	return &v
}

// AppendEvents bulk-inserts a batch of run events (the telemetry push path:
// batched 5s / 20 events). Order is preserved.
func (s *Store) AppendEvents(ctx context.Context, runID string, events []Event) error {
	if len(events) == 0 {
		return nil
	}
	batch := &pgx.Batch{}
	for _, e := range events {
		data := e.Data
		if len(data) == 0 {
			data = json.RawMessage(`{}`)
		}
		ts := e.TS
		if ts.IsZero() {
			ts = time.Now().UTC()
		}
		batch.Queue(
			`INSERT INTO run_event (run_id, event, data, ts) VALUES ($1, $2, $3, $4)`,
			runID, e.Event, []byte(data), ts)
	}
	br := s.pool.SendBatch(ctx, batch)
	defer br.Close()
	for range events {
		if _, err := br.Exec(); err != nil {
			return err
		}
	}
	return nil
}

// GetRun loads a single run by id.
func (s *Store) GetRun(ctx context.Context, runID string) (*Run, error) {
	var r Run
	err := s.pool.QueryRow(ctx, `
		SELECT id::text, tenant_id::text, project_id::text, runner_id::text, lease_id::text,
		       remote, issue_number, status, current_state, branch, pr_url, blocked_reason,
		       retry_count, timeout_phase, clarification_round, started_at, finished_at
		  FROM run WHERE id = $1`, runID).
		Scan(&r.ID, &r.TenantID, &r.ProjectID, &r.RunnerID, &r.LeaseID,
			&r.Remote, &r.IssueNumber, &r.Status, &r.CurrentState, &r.Branch, &r.PRURL,
			&r.BlockedReason, &r.RetryCount, &r.TimeoutPhase, &r.ClarificationRound,
			&r.StartedAt, &r.FinishedAt)
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// ListEvents returns the events for a run in chronological order.
func (s *Store) ListEvents(ctx context.Context, runID string) ([]Event, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT event, data, ts FROM run_event WHERE run_id = $1 ORDER BY ts ASC, seq ASC`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		var data []byte
		if err := rows.Scan(&e.Event, &data, &e.TS); err != nil {
			return nil, err
		}
		e.Data = json.RawMessage(data)
		out = append(out, e)
	}
	return out, rows.Err()
}
