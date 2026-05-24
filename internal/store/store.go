// Package store implements the StoreAdapter interface (pkg/adapter) against
// PostgreSQL using sqlc-generated query functions and pgx/v5.
package store

//go:generate sqlc generate -f ../../sqlc.yaml

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ashita-ai/mimir/internal/core"
	"github.com/ashita-ai/mimir/internal/store/dbsqlc"
	"github.com/ashita-ai/mimir/pkg/adapter"
)

// Store implements pkg/adapter.StoreAdapter against PostgreSQL.
type Store struct {
	pool *pgxpool.Pool
	q    *dbsqlc.Queries
	tx   pgx.Tx // non-nil when this Store is transaction-bound
}

// New creates a Store backed by the given connection pool.
func New(pool *pgxpool.Pool) *Store {
	return &Store{
		pool: pool,
		q:    dbsqlc.New(pool),
	}
}

// Pool exposes the underlying pool for components (e.g. River) that need it.
func (s *Store) Pool() *pgxpool.Pool {
	return s.pool
}

// ---------------------------------------------------------------------------
// Transactions
// ---------------------------------------------------------------------------

func (s *Store) WithTx(ctx context.Context, fn adapter.TxFunc) error {
	// If already transaction-bound, use a savepoint (pgx creates one
	// automatically when you call Begin on an existing Tx).
	var tx pgx.Tx
	var err error
	if s.tx != nil {
		tx, err = s.tx.Begin(ctx)
	} else {
		tx, err = s.pool.Begin(ctx)
	}
	if err != nil {
		return fmt.Errorf("store: begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	txStore := &Store{
		pool: s.pool,
		q:    s.q.WithTx(tx),
		tx:   tx,
	}

	if err := fn(txStore, tx); err != nil {
		return err
	}

	return tx.Commit(ctx)
}

// ---------------------------------------------------------------------------
// PullRequests
// ---------------------------------------------------------------------------

func (s *Store) UpsertPullRequest(ctx context.Context, pr *core.PullRequest) error {
	metadata := pr.Metadata
	if metadata == nil {
		metadata = json.RawMessage("{}")
	}

	row, err := s.q.UpsertPullRequest(ctx, dbsqlc.UpsertPullRequestParams{
		ExternalPrID: pr.ExternalPRID,
		RepoFullName: pr.RepoFullName,
		PrNumber:     int32(pr.PRNumber),
		HeadSha:      pr.HeadSHA,
		BaseSha:      pr.BaseSHA,
		Author:       pr.Author,
		State:        string(pr.State),
		Metadata:     metadata,
	})
	if err != nil {
		return fmt.Errorf("store: upsert pull request: %w", err)
	}

	pr.ID = row.ID
	pr.Metadata = row.Metadata
	pr.CreatedAt = row.CreatedAt
	pr.UpdatedAt = row.UpdatedAt
	return nil
}

func (s *Store) GetPullRequest(ctx context.Context, id uuid.UUID) (*core.PullRequest, error) {
	row, err := s.q.GetPullRequest(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("store: get pull request %s: %w", id, err)
	}

	return &core.PullRequest{
		ID:           row.ID,
		ExternalPRID: row.ExternalPrID,
		RepoFullName: row.RepoFullName,
		PRNumber:     int(row.PrNumber),
		HeadSHA:      row.HeadSha,
		BaseSHA:      row.BaseSha,
		Author:       row.Author,
		State:        core.PRState(row.State),
		Metadata:     row.Metadata,
		CreatedAt:    row.CreatedAt,
		UpdatedAt:    row.UpdatedAt,
	}, nil
}

func (s *Store) SoftDeletePullRequest(ctx context.Context, id uuid.UUID) error {
	n, err := s.q.SoftDeletePullRequest(ctx, id)
	if err != nil {
		return fmt.Errorf("store: soft delete pull request %s: %w", id, err)
	}
	if n == 0 {
		return fmt.Errorf("store: soft delete pull request %s: %w", id, adapter.ErrNotFound)
	}
	return nil
}

// ---------------------------------------------------------------------------
// PipelineRuns
// ---------------------------------------------------------------------------

func (s *Store) CreatePipelineRun(ctx context.Context, run *core.PipelineRun) error {
	metadata := run.Metadata
	if metadata == nil {
		metadata = json.RawMessage("{}")
	}

	row, err := s.q.CreatePipelineRun(ctx, dbsqlc.CreatePipelineRunParams{
		PullRequestID: run.PullRequestID,
		HeadSha:       run.HeadSHA,
		PromptVersion: run.PromptVersion,
		ConfigHash:    run.ConfigHash,
		Metadata:      metadata,
	})
	if err != nil {
		return fmt.Errorf("store: create pipeline run: %w", err)
	}

	run.ID = row.ID
	run.Status = core.PipelineStatus(row.Status)
	run.StartedAt = row.StartedAt
	run.CompletedAt = row.CompletedAt
	run.Metadata = row.Metadata
	return nil
}

func (s *Store) CompletePipelineRun(ctx context.Context, id uuid.UUID, status core.PipelineStatus, stats adapter.PipelineRunStats) error {
	n, err := s.q.CompletePipelineRun(ctx, dbsqlc.CompletePipelineRunParams{
		ID:                 id,
		Status:             string(status),
		TasksTotal:         int4FromInt(stats.TasksTotal),
		TasksCompleted:     int4FromInt(stats.TasksCompleted),
		TasksFailed:        int4FromInt(stats.TasksFailed),
		FindingsTotal:      int4FromInt(stats.FindingsTotal),
		FindingsPosted:     int4FromInt(stats.FindingsPosted),
		FindingsSuppressed: int4FromInt(stats.FindingsSuppressed),
	})
	if err != nil {
		return fmt.Errorf("store: complete pipeline run %s: %w", id, err)
	}
	if n == 0 {
		return fmt.Errorf("store: complete pipeline run %s: %w", id, adapter.ErrNotFound)
	}
	return nil
}

func (s *Store) GetPipelineRun(ctx context.Context, id uuid.UUID) (*core.PipelineRun, error) {
	row, err := s.q.GetPipelineRun(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("store: get pipeline run %s: %w", id, err)
	}
	run := pipelineRunFromRow(row)
	return &run, nil
}

func (s *Store) ListPipelineRunsForPR(ctx context.Context, pullRequestID uuid.UUID) ([]core.PipelineRun, error) {
	rows, err := s.q.ListPipelineRunsForPR(ctx, pullRequestID)
	if err != nil {
		return nil, fmt.Errorf("store: list pipeline runs for PR %s: %w", pullRequestID, err)
	}

	runs := make([]core.PipelineRun, len(rows))
	for i, row := range rows {
		runs[i] = pipelineRunFromRow(row)
	}
	return runs, nil
}

func (s *Store) ReconcileStalePipelineRuns(ctx context.Context, staleThreshold time.Duration) error {
	interval := pgtype.Interval{
		Microseconds: staleThreshold.Microseconds(),
		Valid:        true,
	}
	if err := s.q.ReconcileStalePipelineRuns(ctx, interval); err != nil {
		return fmt.Errorf("store: reconcile stale pipeline runs: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// ReviewTasks
// ---------------------------------------------------------------------------

func (s *Store) CreateReviewTask(ctx context.Context, task *core.ReviewTask) error {
	var diffHunk pgtype.Text
	if task.DiffHunk != nil {
		diffHunk = pgtype.Text{String: *task.DiffHunk, Valid: true}
	}

	row, err := s.q.CreateReviewTask(ctx, dbsqlc.CreateReviewTaskParams{
		PullRequestID: task.PullRequestID,
		PipelineRunID: task.PipelineRunID,
		TaskType:      string(task.TaskType),
		FilePath:      task.FilePath,
		Symbol:        task.Symbol,
		RiskScore:     float64(task.RiskScore),
		ModelID:       task.ModelID,
		DiffHunk:      diffHunk,
	})
	if err != nil {
		return fmt.Errorf("store: create review task: %w", err)
	}

	task.ID = row.ID
	task.Status = core.TaskStatus(row.Status)
	task.CreatedAt = row.CreatedAt
	return nil
}

func (s *Store) UpdateReviewTaskStatus(ctx context.Context, id uuid.UUID, status string, errMsg *string) error {
	n, err := s.q.UpdateReviewTaskStatus(ctx, dbsqlc.UpdateReviewTaskStatusParams{
		ID:     id,
		Status: status,
		Error:  textFromStringPtr(errMsg),
	})
	if err != nil {
		return fmt.Errorf("store: update review task status %s: %w", id, err)
	}
	if n == 0 {
		return fmt.Errorf("store: update review task status %s: %w", id, adapter.ErrNotFound)
	}
	return nil
}

func (s *Store) ListReviewTasksForPR(ctx context.Context, prID uuid.UUID) ([]core.ReviewTask, error) {
	rows, err := s.q.ListReviewTasksForPR(ctx, prID)
	if err != nil {
		return nil, fmt.Errorf("store: list review tasks for PR %s: %w", prID, err)
	}
	return reviewTasksFromRows(rows), nil
}

func (s *Store) ListReviewTasksForRun(ctx context.Context, runID uuid.UUID) ([]core.ReviewTask, error) {
	rows, err := s.q.ListReviewTasksForRun(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("store: list review tasks for run %s: %w", runID, err)
	}
	return reviewTasksFromRows(rows), nil
}

func (s *Store) CountTaskStats(ctx context.Context, runID uuid.UUID) (total, completed, failed int, err error) {
	row, err := s.q.CountCompletedAndTotal(ctx, runID)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("store: count task stats for run %s: %w", runID, err)
	}
	return int(row.Total), int(row.Completed), int(row.Failed), nil
}

// ---------------------------------------------------------------------------
// Findings
// ---------------------------------------------------------------------------

func (s *Store) CreateFinding(ctx context.Context, f *core.Finding) error {
	metadata := f.Metadata
	if metadata == nil {
		metadata = json.RawMessage("{}")
	}

	return s.WithTx(ctx, func(txStore adapter.StoreAdapter, _ pgx.Tx) error {
		st := txStore.(*Store)
		row, err := st.q.CreateFinding(ctx, dbsqlc.CreateFindingParams{
			ReviewTaskID:      f.ReviewTaskID,
			PullRequestID:     f.PullRequestID,
			PipelineRunID:     f.PipelineRunID,
			RepoFullName:      f.RepoFullName,
			FilePath:          f.FilePath,
			StartLine:         int4FromIntPtr(f.StartLine),
			EndLine:           int4FromIntPtr(f.EndLine),
			Symbol:            f.Symbol,
			Category:          string(f.Category),
			ConfidenceTier:    string(f.ConfidenceTier),
			ConfidenceScore:   f.ConfidenceScore,
			Severity:          string(f.Severity),
			Title:             f.Title,
			Body:              f.Body,
			Suggestion:        textFromString(f.Suggestion),
			LocationHash:      f.LocationHash,
			ContentHash:       textFromStringPtr(f.ContentHash),
			HeadSha:           f.HeadSHA,
			SuppressionReason: textFromStringPtr(f.SuppressionReason),
			ModelID:           f.ModelID,
			PromptTokens:      int4FromIntPtr(f.PromptTokens),
			CompletionTokens:  int4FromIntPtr(f.CompletionTokens),
			Metadata:          metadata,
		})
		if err != nil {
			return fmt.Errorf("store: create finding: %w", err)
		}

		f.ID = row.ID
		f.CreatedAt = row.CreatedAt
		f.UpdatedAt = row.UpdatedAt

		if err := txStore.CreateFindingEvent(ctx, f.ID, "created", "system", nil, nil); err != nil {
			return fmt.Errorf("store: create finding %s: event: %w", f.ID, err)
		}
		return nil
	})
}

func (s *Store) ListFindingsForPR(ctx context.Context, prID uuid.UUID) ([]core.Finding, error) {
	rows, err := s.q.ListFindingsForPR(ctx, prID)
	if err != nil {
		return nil, fmt.Errorf("store: list findings for PR %s: %w", prID, err)
	}
	return findingsFromRows(rows), nil
}

func (s *Store) ListFindingsForRun(ctx context.Context, runID uuid.UUID) ([]core.Finding, error) {
	rows, err := s.q.ListFindingsForRun(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("store: list findings for run %s: %w", runID, err)
	}
	return findingsFromRows(rows), nil
}

func (s *Store) FindPriorFinding(ctx context.Context, prID uuid.UUID, locationHash string) (*core.Finding, error) {
	row, err := s.q.FindPriorFinding(ctx, dbsqlc.FindPriorFindingParams{
		ID:           prID,
		LocationHash: locationHash,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("store: find prior finding: %w", err)
	}
	return &core.Finding{
		ID:           row.ID,
		LocationHash: row.LocationHash,
		ContentHash:  stringPtrFromText(row.ContentHash),
		HeadSHA:      row.HeadSha,
	}, nil
}

func (s *Store) ListUnaddressedFindings(ctx context.Context, prID uuid.UUID) ([]core.Finding, error) {
	rows, err := s.q.ListUnaddressedFindingsForPR(ctx, prID)
	if err != nil {
		return nil, fmt.Errorf("store: list unaddressed findings for PR %s: %w", prID, err)
	}
	return findingsFromRows(rows), nil
}

func (s *Store) ListUnpostedFindings(ctx context.Context, pipelineRunID uuid.UUID) ([]core.Finding, error) {
	rows, err := s.q.ListUnpostedFindings(ctx, pipelineRunID)
	if err != nil {
		return nil, fmt.Errorf("store: list unposted findings for run %s: %w", pipelineRunID, err)
	}
	return findingsFromRows(rows), nil
}

func (s *Store) MarkFindingPosted(ctx context.Context, id uuid.UUID, commentID int64) error {
	return s.WithTx(ctx, func(txStore adapter.StoreAdapter, _ pgx.Tx) error {
		if err := txStore.CreateFindingEvent(ctx, id, "posted", "system", nil, nil); err != nil {
			return fmt.Errorf("store: mark finding posted %s: event: %w", id, err)
		}
		st := txStore.(*Store)
		n, err := st.q.MarkFindingPosted(ctx, dbsqlc.MarkFindingPostedParams{
			ID:                id,
			ExternalCommentID: pgtype.Int8{Int64: commentID, Valid: true},
		})
		if err != nil {
			return fmt.Errorf("store: mark finding posted %s: %w", id, err)
		}
		if n == 0 {
			return fmt.Errorf("store: mark finding posted %s: %w", id, adapter.ErrNotFound)
		}
		return nil
	})
}

func (s *Store) MarkFindingAddressed(ctx context.Context, id uuid.UUID, status core.AddressedStatus) error {
	newVal := string(status)
	return s.WithTx(ctx, func(txStore adapter.StoreAdapter, _ pgx.Tx) error {
		if err := txStore.CreateFindingEvent(ctx, id, "addressed", "system", nil, &newVal); err != nil {
			return fmt.Errorf("store: mark finding addressed %s: event: %w", id, err)
		}
		st := txStore.(*Store)
		n, err := st.q.MarkFindingAddressed(ctx, dbsqlc.MarkFindingAddressedParams{
			ID:              id,
			AddressedStatus: newVal,
		})
		if err != nil {
			return fmt.Errorf("store: mark finding addressed %s: %w", id, err)
		}
		if n == 0 {
			return fmt.Errorf("store: mark finding addressed %s: %w", id, adapter.ErrNotFound)
		}
		return nil
	})
}

// ---------------------------------------------------------------------------
// Dismissed Fingerprints
// ---------------------------------------------------------------------------

func (s *Store) IsFingerprintDismissed(ctx context.Context, fingerprint, repoFullName string) (bool, error) {
	dismissed, err := s.q.IsFingerprintDismissed(ctx, dbsqlc.IsFingerprintDismissedParams{
		Fingerprint:  fingerprint,
		RepoFullName: repoFullName,
	})
	if err != nil {
		return false, fmt.Errorf("store: check dismissed fingerprint: %w", err)
	}
	return dismissed, nil
}

func (s *Store) DismissFingerprint(ctx context.Context, fingerprint, repoFullName, dismissedBy, reason string) error {
	if err := s.q.DismissFingerprint(ctx, dbsqlc.DismissFingerprintParams{
		Fingerprint:  fingerprint,
		RepoFullName: repoFullName,
		DismissedBy:  dismissedBy,
		Reason:       textFromString(reason),
	}); err != nil {
		return fmt.Errorf("store: dismiss fingerprint %s/%s: %w", repoFullName, fingerprint, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Finding Events
// ---------------------------------------------------------------------------

func (s *Store) CreateFindingEvent(ctx context.Context, findingID uuid.UUID, eventType, actor string, oldValue, newValue *string) error {
	if err := s.q.CreateFindingEvent(ctx, dbsqlc.CreateFindingEventParams{
		FindingID: findingID,
		EventType: eventType,
		Actor:     actor,
		OldValue:  textFromStringPtr(oldValue),
		NewValue:  textFromStringPtr(newValue),
		Metadata:  json.RawMessage("{}"),
	}); err != nil {
		return fmt.Errorf("store: create finding event for %s: %w", findingID, err)
	}
	return nil
}

func (s *Store) ListEventsForFinding(ctx context.Context, findingID uuid.UUID) ([]core.FindingEvent, error) {
	rows, err := s.q.ListEventsForFinding(ctx, findingID)
	if err != nil {
		return nil, fmt.Errorf("store: list events for finding %s: %w", findingID, err)
	}

	events := make([]core.FindingEvent, len(rows))
	for i, row := range rows {
		events[i] = findingEventFromRow(row)
	}
	return events, nil
}

// ---------------------------------------------------------------------------
// pgtype <-> core conversion helpers
// ---------------------------------------------------------------------------

func textFromString(s string) pgtype.Text {
	if s == "" {
		return pgtype.Text{Valid: false}
	}
	return pgtype.Text{String: s, Valid: true}
}

func textFromStringPtr(s *string) pgtype.Text {
	if s == nil {
		return pgtype.Text{Valid: false}
	}
	return pgtype.Text{String: *s, Valid: true}
}

func stringFromText(t pgtype.Text) string {
	if !t.Valid {
		return ""
	}
	return t.String
}

func stringPtrFromText(t pgtype.Text) *string {
	if !t.Valid {
		return nil
	}
	return &t.String
}

func int4FromIntPtr(p *int) pgtype.Int4 {
	if p == nil {
		return pgtype.Int4{Valid: false}
	}
	return pgtype.Int4{Int32: int32(*p), Valid: true}
}

func int4FromInt(v int) pgtype.Int4 {
	return pgtype.Int4{Int32: int32(v), Valid: true}
}

func intPtrFromInt4(i pgtype.Int4) *int {
	if !i.Valid {
		return nil
	}
	v := int(i.Int32)
	return &v
}

func int64PtrFromInt8(i pgtype.Int8) *int64 {
	if !i.Valid {
		return nil
	}
	return &i.Int64
}

func reviewTaskFromRow(row dbsqlc.ReviewTask) core.ReviewTask {
	return core.ReviewTask{
		ID:            row.ID,
		PullRequestID: row.PullRequestID,
		PipelineRunID: row.PipelineRunID,
		TaskType:      core.TaskType(row.TaskType),
		FilePath:      row.FilePath,
		Symbol:        row.Symbol,
		RiskScore:     core.RiskScore(row.RiskScore),
		ModelID:       row.ModelID,
		DiffHunk:      stringPtrFromText(row.DiffHunk),
		Status:        core.TaskStatus(row.Status),
		Error:         stringPtrFromText(row.Error),
		StartedAt:     row.StartedAt,
		CompletedAt:   row.CompletedAt,
		CreatedAt:     row.CreatedAt,
	}
}

func reviewTasksFromRows(rows []dbsqlc.ReviewTask) []core.ReviewTask {
	tasks := make([]core.ReviewTask, len(rows))
	for i, row := range rows {
		tasks[i] = reviewTaskFromRow(row)
	}
	return tasks
}

func findingFromRow(row dbsqlc.Finding) core.Finding {
	return core.Finding{
		ID:                row.ID,
		ReviewTaskID:      row.ReviewTaskID,
		PullRequestID:     row.PullRequestID,
		PipelineRunID:     row.PipelineRunID,
		RepoFullName:      row.RepoFullName,
		FilePath:          row.FilePath,
		StartLine:         intPtrFromInt4(row.StartLine),
		EndLine:           intPtrFromInt4(row.EndLine),
		Symbol:            row.Symbol,
		Category:          core.FindingCategory(row.Category),
		ConfidenceTier:    core.ConfidenceTier(row.ConfidenceTier),
		ConfidenceScore:   row.ConfidenceScore,
		Severity:          core.Severity(row.Severity),
		Title:             row.Title,
		Body:              row.Body,
		Suggestion:        stringFromText(row.Suggestion),
		LocationHash:      row.LocationHash,
		ContentHash:       stringPtrFromText(row.ContentHash),
		HeadSHA:           row.HeadSha,
		PostedAt:          row.PostedAt,
		ExternalCommentID: int64PtrFromInt8(row.ExternalCommentID),
		AddressedStatus:   core.AddressedStatus(row.AddressedStatus),
		SuppressionReason: stringPtrFromText(row.SuppressionReason),
		DismissedAt:       row.DismissedAt,
		DismissedBy:       stringPtrFromText(row.DismissedBy),
		ModelID:           row.ModelID,
		PromptTokens:      intPtrFromInt4(row.PromptTokens),
		CompletionTokens:  intPtrFromInt4(row.CompletionTokens),
		Metadata:          row.Metadata,
		CreatedAt:         row.CreatedAt,
		UpdatedAt:         row.UpdatedAt,
	}
}

func findingsFromRows(rows []dbsqlc.Finding) []core.Finding {
	findings := make([]core.Finding, len(rows))
	for i, row := range rows {
		findings[i] = findingFromRow(row)
	}
	return findings
}

func pipelineRunFromRow(row dbsqlc.PipelineRun) core.PipelineRun {
	return core.PipelineRun{
		ID:                 row.ID,
		PullRequestID:      row.PullRequestID,
		HeadSHA:            row.HeadSha,
		PromptVersion:      row.PromptVersion,
		ConfigHash:         row.ConfigHash,
		Status:             core.PipelineStatus(row.Status),
		TasksTotal:         intPtrFromInt4(row.TasksTotal),
		TasksCompleted:     intPtrFromInt4(row.TasksCompleted),
		TasksFailed:        intPtrFromInt4(row.TasksFailed),
		FindingsTotal:      intPtrFromInt4(row.FindingsTotal),
		FindingsPosted:     intPtrFromInt4(row.FindingsPosted),
		FindingsSuppressed: intPtrFromInt4(row.FindingsSuppressed),
		StartedAt:          row.StartedAt,
		CompletedAt:        row.CompletedAt,
		Metadata:           row.Metadata,
	}
}

func findingEventFromRow(row dbsqlc.FindingEvent) core.FindingEvent {
	return core.FindingEvent{
		ID:        row.ID,
		FindingID: row.FindingID,
		EventType: row.EventType,
		Actor:     row.Actor,
		OldValue:  stringPtrFromText(row.OldValue),
		NewValue:  stringPtrFromText(row.NewValue),
		Metadata:  row.Metadata,
		CreatedAt: row.CreatedAt,
	}
}
