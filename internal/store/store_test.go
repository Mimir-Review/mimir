package store_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pressly/goose/v3"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/ashita-ai/mimir/internal/core"
	"github.com/ashita-ai/mimir/internal/store"
	"github.com/ashita-ai/mimir/pkg/adapter"
)

// Compile-time check: *store.Store satisfies adapter.StoreAdapter.
var _ adapter.StoreAdapter = (*store.Store)(nil)

// testDatabaseURL returns the database URL for integration tests.
// Defaults to the docker-compose local dev database.
// Set MIMIR_TEST_DATABASE_URL to override.
func testDatabaseURL() string {
	if v := os.Getenv("MIMIR_TEST_DATABASE_URL"); v != "" {
		return v
	}
	return "postgres://mimir:mimir@localhost:5433/mimir?sslmode=disable"
}

// setupTestDB creates a pgxpool, runs migrations, and returns the pool
// and a cleanup function that truncates all tables.
func setupTestDB(t *testing.T) (*pgxpool.Pool, *store.Store) {
	t.Helper()

	dbURL := testDatabaseURL()
	ctx := context.Background()

	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Skipf("skipping integration test: cannot connect to database: %v", err)
	}

	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Skipf("skipping integration test: database not reachable: %v", err)
	}

	// Run river migrations.
	riverMigrator, err := rivermigrate.New(riverpgxv5.New(pool), nil)
	require.NoError(t, err)
	_, err = riverMigrator.Migrate(ctx, rivermigrate.DirectionUp, nil)
	require.NoError(t, err)

	// Run app migrations via goose.
	sqlDB, err := sql.Open("pgx", dbURL)
	require.NoError(t, err)
	defer sqlDB.Close()

	migrationFS, err := fs.Sub(store.Migrations, "migrations")
	require.NoError(t, err)

	provider, err := goose.NewProvider(goose.DialectPostgres, sqlDB, migrationFS)
	require.NoError(t, err)

	_, err = provider.Up(ctx)
	require.NoError(t, err)

	// Truncate tables before each test (order matters for FK constraints).
	tables := []string{
		"finding_events", "findings", "review_tasks",
		"pipeline_runs", "dismissed_fingerprints", "pull_requests",
	}
	for _, table := range tables {
		_, err := pool.Exec(ctx, fmt.Sprintf("TRUNCATE %s CASCADE", table))
		require.NoError(t, err)
	}

	t.Cleanup(func() {
		pool.Close()
	})

	return pool, store.New(pool)
}

// ---------------------------------------------------------------------------
// PullRequest tests
// ---------------------------------------------------------------------------

func TestUpsertAndGetPullRequest(t *testing.T) {
	_, st := setupTestDB(t)
	ctx := context.Background()

	pr := &core.PullRequest{
		ExternalPRID: 12345,
		RepoFullName: "ashita-ai/mimir",
		PRNumber:     42,
		HeadSHA:      "abc123",
		BaseSHA:      "def456",
		Author:       "testuser",
		State:        core.PRStateOpen,
		Metadata:     json.RawMessage(`{"source":"test"}`),
	}

	// Insert.
	err := st.UpsertPullRequest(ctx, pr)
	require.NoError(t, err)
	assert.NotEqual(t, uuid.Nil, pr.ID)
	assert.False(t, pr.CreatedAt.IsZero())

	// Get.
	got, err := st.GetPullRequest(ctx, pr.ID)
	require.NoError(t, err)
	assert.Equal(t, pr.ID, got.ID)
	assert.Equal(t, pr.RepoFullName, got.RepoFullName)
	assert.Equal(t, pr.PRNumber, got.PRNumber)
	assert.Equal(t, pr.HeadSHA, got.HeadSHA)
	assert.Equal(t, pr.Author, got.Author)
	assert.JSONEq(t, `{"source":"test"}`, string(got.Metadata))

	// Upsert same (external_pr_id, head_sha) — should update, not insert.
	pr.State = core.PRStateMerged
	err = st.UpsertPullRequest(ctx, pr)
	require.NoError(t, err)

	got2, err := st.GetPullRequest(ctx, pr.ID)
	require.NoError(t, err)
	assert.Equal(t, core.PRStateMerged, got2.State)
}

func TestGetPullRequest_NotFound(t *testing.T) {
	_, st := setupTestDB(t)
	ctx := context.Background()

	_, err := st.GetPullRequest(ctx, uuid.New())
	require.Error(t, err)
}

func TestSoftDeletePullRequest(t *testing.T) {
	_, st := setupTestDB(t)
	ctx := context.Background()

	pr := insertTestPR(t, st)

	err := st.SoftDeletePullRequest(ctx, pr.ID)
	require.NoError(t, err)

	// GetPullRequest filters by deleted_at IS NULL, so this should error.
	_, err = st.GetPullRequest(ctx, pr.ID)
	require.Error(t, err)
}

func TestUpsertPullRequest_ClearsDeletedAt(t *testing.T) {
	_, st := setupTestDB(t)
	ctx := context.Background()

	pr := insertTestPR(t, st)

	// Soft-delete the PR.
	err := st.SoftDeletePullRequest(ctx, pr.ID)
	require.NoError(t, err)

	_, err = st.GetPullRequest(ctx, pr.ID)
	require.Error(t, err, "should be invisible after soft delete")

	// Upsert with the same (external_pr_id, head_sha) should clear deleted_at.
	pr.State = core.PRStateOpen
	err = st.UpsertPullRequest(ctx, pr)
	require.NoError(t, err)

	got, err := st.GetPullRequest(ctx, pr.ID)
	require.NoError(t, err, "should be visible again after upsert clears deleted_at")
	assert.Equal(t, core.PRStateOpen, got.State)
}

// ---------------------------------------------------------------------------
// PipelineRun tests
// ---------------------------------------------------------------------------

func TestCreateAndGetPipelineRun(t *testing.T) {
	_, st := setupTestDB(t)
	ctx := context.Background()

	pr := insertTestPR(t, st)

	run := &core.PipelineRun{
		PullRequestID: pr.ID,
		HeadSHA:       pr.HeadSHA,
		PromptVersion: "v1.0",
		ConfigHash:    "abc123",
		Metadata:      json.RawMessage("{}"),
	}

	err := st.CreatePipelineRun(ctx, run)
	require.NoError(t, err)
	assert.NotEqual(t, uuid.Nil, run.ID)
	assert.Equal(t, core.PipelineStatusRunning, run.Status)

	got, err := st.GetPipelineRun(ctx, run.ID)
	require.NoError(t, err)
	assert.Equal(t, run.ID, got.ID)
	assert.Equal(t, pr.ID, got.PullRequestID)
	assert.Equal(t, "v1.0", got.PromptVersion)
	assert.Equal(t, core.PipelineStatusRunning, got.Status)
}

func TestCompletePipelineRun(t *testing.T) {
	_, st := setupTestDB(t)
	ctx := context.Background()

	pr := insertTestPR(t, st)
	run := insertTestPipelineRun(t, st, pr.ID, pr.HeadSHA)

	err := st.CompletePipelineRun(ctx, run.ID, core.PipelineStatusCompleted, adapter.PipelineRunStats{
		TasksTotal:         5,
		TasksCompleted:     4,
		TasksFailed:        1,
		FindingsTotal:      3,
		FindingsPosted:     2,
		FindingsSuppressed: 1,
	})
	require.NoError(t, err)

	got, err := st.GetPipelineRun(ctx, run.ID)
	require.NoError(t, err)
	assert.Equal(t, core.PipelineStatusCompleted, got.Status)
	require.NotNil(t, got.TasksTotal)
	assert.Equal(t, 5, *got.TasksTotal)
	require.NotNil(t, got.FindingsTotal)
	assert.Equal(t, 3, *got.FindingsTotal)
	assert.NotNil(t, got.CompletedAt)
}

func TestReconcileStalePipelineRuns(t *testing.T) {
	_, st := setupTestDB(t)
	ctx := context.Background()

	pr := insertTestPR(t, st)
	run := insertTestPipelineRun(t, st, pr.ID, pr.HeadSHA)

	// Use a zero threshold so the freshly-created run is considered stale.
	err := st.ReconcileStalePipelineRuns(ctx, 0)
	require.NoError(t, err)

	got, err := st.GetPipelineRun(ctx, run.ID)
	require.NoError(t, err)
	assert.Equal(t, core.PipelineStatusFailed, got.Status)
}

func TestListPipelineRunsForPR(t *testing.T) {
	_, st := setupTestDB(t)
	ctx := context.Background()

	pr := insertTestPR(t, st)
	insertTestPipelineRun(t, st, pr.ID, pr.HeadSHA)

	runs, err := st.ListPipelineRunsForPR(ctx, pr.ID)
	require.NoError(t, err)
	assert.Len(t, runs, 1)
}

// ---------------------------------------------------------------------------
// ReviewTask tests
// ---------------------------------------------------------------------------

func TestCreateAndListReviewTasks(t *testing.T) {
	_, st := setupTestDB(t)
	ctx := context.Background()

	pr := insertTestPR(t, st)
	run := insertTestPipelineRun(t, st, pr.ID, pr.HeadSHA)

	task := &core.ReviewTask{
		PullRequestID: pr.ID,
		PipelineRunID: run.ID,
		TaskType:      core.TaskTypeSecurity,
		FilePath:      "internal/auth/token.go",
		Symbol:        "ValidateToken",
		RiskScore:     0.85,
		ModelID:       "claude-opus-4-6",
	}

	err := st.CreateReviewTask(ctx, task)
	require.NoError(t, err)
	assert.NotEqual(t, uuid.Nil, task.ID)
	assert.Equal(t, core.TaskStatusPending, task.Status)

	// List by PR — should include our task.
	tasks, err := st.ListReviewTasksForPR(ctx, pr.ID)
	require.NoError(t, err)
	require.Len(t, tasks, 1)
	assert.Equal(t, task.ID, tasks[0].ID)
	assert.Equal(t, "ValidateToken", tasks[0].Symbol)
	assert.Equal(t, "claude-opus-4-6", tasks[0].ModelID)
	assert.Equal(t, run.ID, tasks[0].PipelineRunID)

	// List by run — should include our task.
	tasksByRun, err := st.ListReviewTasksForRun(ctx, run.ID)
	require.NoError(t, err)
	require.Len(t, tasksByRun, 1)
	assert.Equal(t, task.ID, tasksByRun[0].ID)
}

func TestUpdateReviewTaskStatus(t *testing.T) {
	_, st := setupTestDB(t)
	ctx := context.Background()

	pr := insertTestPR(t, st)
	run := insertTestPipelineRun(t, st, pr.ID, pr.HeadSHA)
	task := insertTestTask(t, st, pr.ID, run.ID)

	// Transition to running.
	err := st.UpdateReviewTaskStatus(ctx, task.ID, string(core.TaskStatusRunning), nil)
	require.NoError(t, err)

	// Transition to failed — should set error.
	errMsg := "model timeout"
	err = st.UpdateReviewTaskStatus(ctx, task.ID, string(core.TaskStatusFailed), &errMsg)
	require.NoError(t, err)
}

func TestCountTaskStats(t *testing.T) {
	_, st := setupTestDB(t)
	ctx := context.Background()

	pr := insertTestPR(t, st)
	run := insertTestPipelineRun(t, st, pr.ID, pr.HeadSHA)

	// Create two tasks.
	task1 := insertTestTask(t, st, pr.ID, run.ID)
	insertTestTask(t, st, pr.ID, run.ID)

	total, completed, failed, err := st.CountTaskStats(ctx, run.ID)
	require.NoError(t, err)
	assert.Equal(t, 2, total)
	assert.Equal(t, 0, completed)
	assert.Equal(t, 0, failed)

	// Complete one.
	require.NoError(t, st.UpdateReviewTaskStatus(ctx, task1.ID, string(core.TaskStatusCompleted), nil))

	total, completed, failed, err = st.CountTaskStats(ctx, run.ID)
	require.NoError(t, err)
	assert.Equal(t, 2, total)
	assert.Equal(t, 1, completed)
	assert.Equal(t, 0, failed)
}

// ---------------------------------------------------------------------------
// Finding tests
// ---------------------------------------------------------------------------

func TestCreateAndListFindings(t *testing.T) {
	_, st := setupTestDB(t)
	ctx := context.Background()

	pr := insertTestPR(t, st)
	run := insertTestPipelineRun(t, st, pr.ID, pr.HeadSHA)
	task := insertTestTask(t, st, pr.ID, run.ID)

	line := 42
	f := &core.Finding{
		ReviewTaskID:    task.ID,
		PullRequestID:   pr.ID,
		PipelineRunID:   run.ID,
		RepoFullName:    "ashita-ai/mimir",
		FilePath:        "internal/auth/token.go",
		StartLine:       &line,
		Symbol:          "ValidateToken",
		Category:        core.CategorySecurity,
		ConfidenceTier:  core.ConfidenceHigh,
		ConfidenceScore: 0.92,
		Severity:        core.SeverityHigh,
		Title:           "Unchecked JWT expiry",
		Body:            "The token validation does not verify the exp claim.",
		LocationHash:    core.LocationFingerprint("ashita-ai/mimir", "internal/auth/token.go", "ValidateToken", core.CategorySecurity),
		HeadSHA:         pr.HeadSHA,
		ModelID:         "claude-opus-4-6",
		Metadata:        json.RawMessage("{}"),
	}

	err := st.CreateFinding(ctx, f)
	require.NoError(t, err)
	assert.NotEqual(t, uuid.Nil, f.ID)

	// List findings for PR.
	findings, err := st.ListFindingsForPR(ctx, pr.ID)
	require.NoError(t, err)
	require.Len(t, findings, 1)
	assert.Equal(t, f.Title, findings[0].Title)
	assert.Equal(t, 0.92, findings[0].ConfidenceScore)
	assert.Equal(t, core.SeverityHigh, findings[0].Severity)
	assert.Equal(t, 42, *findings[0].StartLine)
	assert.Equal(t, run.ID, findings[0].PipelineRunID)
	assert.Equal(t, pr.HeadSHA, findings[0].HeadSHA)

	// CreateFinding must record a 'created' event in finding_events.
	events, err := st.ListEventsForFinding(ctx, f.ID)
	require.NoError(t, err)
	require.Len(t, events, 1, "CreateFinding should record exactly one 'created' event")
	assert.Equal(t, "created", events[0].EventType)
	assert.Equal(t, "system", events[0].Actor)
}

func TestMarkFindingPosted(t *testing.T) {
	_, st := setupTestDB(t)
	ctx := context.Background()

	pr := insertTestPR(t, st)
	run := insertTestPipelineRun(t, st, pr.ID, pr.HeadSHA)
	task := insertTestTask(t, st, pr.ID, run.ID)
	f := insertTestFinding(t, st, pr.ID, run.ID, task.ID, pr.HeadSHA)

	err := st.MarkFindingPosted(ctx, f.ID, 999888)
	require.NoError(t, err)

	findings, err := st.ListFindingsForPR(ctx, pr.ID)
	require.NoError(t, err)
	require.Len(t, findings, 1)
	assert.NotNil(t, findings[0].PostedAt)
	assert.NotNil(t, findings[0].ExternalCommentID)
	assert.Equal(t, int64(999888), *findings[0].ExternalCommentID)
}

func TestMarkFindingAddressed(t *testing.T) {
	_, st := setupTestDB(t)
	ctx := context.Background()

	pr := insertTestPR(t, st)
	run := insertTestPipelineRun(t, st, pr.ID, pr.HeadSHA)
	task := insertTestTask(t, st, pr.ID, run.ID)
	f := insertTestFinding(t, st, pr.ID, run.ID, task.ID, pr.HeadSHA)

	err := st.MarkFindingAddressed(ctx, f.ID, core.AddressedLikelyAddressed)
	require.NoError(t, err)

	findings, err := st.ListFindingsForPR(ctx, pr.ID)
	require.NoError(t, err)
	require.Len(t, findings, 1)
	assert.Equal(t, core.AddressedLikelyAddressed, findings[0].AddressedStatus)
}

func TestFindingLocationHashUniqueness(t *testing.T) {
	_, st := setupTestDB(t)
	ctx := context.Background()

	pr := insertTestPR(t, st)
	run := insertTestPipelineRun(t, st, pr.ID, pr.HeadSHA)
	task := insertTestTask(t, st, pr.ID, run.ID)

	hash := core.LocationFingerprint("repo", "file.go", "Fn", core.CategoryLogic)

	f1 := makeFinding(pr.ID, run.ID, task.ID, hash, pr.HeadSHA)
	err := st.CreateFinding(ctx, f1)
	require.NoError(t, err)

	// Same location_hash + same PR + same head_sha should violate the unique index.
	f2 := makeFinding(pr.ID, run.ID, task.ID, hash, pr.HeadSHA)
	err = st.CreateFinding(ctx, f2)
	require.Error(t, err, "duplicate (location_hash, pull_request_id, head_sha) should fail")
}

func TestFindPriorFinding(t *testing.T) {
	_, st := setupTestDB(t)
	ctx := context.Background()

	pr := insertTestPR(t, st)
	run := insertTestPipelineRun(t, st, pr.ID, pr.HeadSHA)
	task := insertTestTask(t, st, pr.ID, run.ID)

	hash := core.LocationFingerprint("ashita-ai/mimir", "file.go", "Fn", core.CategoryLogic)
	f := makeFinding(pr.ID, run.ID, task.ID, hash, pr.HeadSHA)
	require.NoError(t, st.CreateFinding(ctx, f))

	// Should find prior finding by PR ID + location hash.
	prior, err := st.FindPriorFinding(ctx, pr.ID, hash)
	require.NoError(t, err)
	require.NotNil(t, prior)
	assert.Equal(t, f.ID, prior.ID)

	// A second PR row for the same logical PR (same external_pr_id, different
	// head_sha — simulates a force-push) should still find the prior finding.
	pr2 := &core.PullRequest{
		ExternalPRID: pr.ExternalPRID, // same logical PR
		RepoFullName: pr.RepoFullName,
		PRNumber:     pr.PRNumber,
		HeadSHA:      "forcepush1",
		BaseSHA:      pr.BaseSHA,
		Author:       pr.Author,
		State:        core.PRStateOpen,
		Metadata:     json.RawMessage("{}"),
	}
	require.NoError(t, st.UpsertPullRequest(ctx, pr2))
	require.NotEqual(t, pr.ID, pr2.ID, "force-push should create a new PR row")

	prior, err = st.FindPriorFinding(ctx, pr2.ID, hash)
	require.NoError(t, err)
	require.NotNil(t, prior, "should find finding across force-push via external_pr_id")
	assert.Equal(t, f.ID, prior.ID)

	// Completely different logical PR — should not find.
	otherPR := insertTestPR(t, st)
	prior, err = st.FindPriorFinding(ctx, otherPR.ID, hash)
	require.NoError(t, err)
	assert.Nil(t, prior)
}

func TestListUnaddressedFindings(t *testing.T) {
	_, st := setupTestDB(t)
	ctx := context.Background()

	pr := insertTestPR(t, st)
	run := insertTestPipelineRun(t, st, pr.ID, pr.HeadSHA)
	task := insertTestTask(t, st, pr.ID, run.ID)

	// Create a finding WITH content_hash set (required by query filter).
	contentHash := "abc123hash"
	f := makeFinding(pr.ID, run.ID, task.ID,
		core.LocationFingerprint("ashita-ai/mimir", "file.go", "Fn", core.CategoryLogic),
		pr.HeadSHA,
	)
	f.ContentHash = &contentHash
	require.NoError(t, st.CreateFinding(ctx, f))

	// Should appear (unaddressed + content_hash IS NOT NULL).
	unaddressed, err := st.ListUnaddressedFindings(ctx, pr.ID)
	require.NoError(t, err)
	assert.Len(t, unaddressed, 1)

	// Address it.
	require.NoError(t, st.MarkFindingAddressed(ctx, f.ID, core.AddressedLikelyAddressed))

	// Should disappear.
	unaddressed, err = st.ListUnaddressedFindings(ctx, pr.ID)
	require.NoError(t, err)
	assert.Empty(t, unaddressed)
}

func TestListUnpostedFindings(t *testing.T) {
	_, st := setupTestDB(t)
	ctx := context.Background()

	pr := insertTestPR(t, st)
	run := insertTestPipelineRun(t, st, pr.ID, pr.HeadSHA)
	task := insertTestTask(t, st, pr.ID, run.ID)
	f := insertTestFinding(t, st, pr.ID, run.ID, task.ID, pr.HeadSHA)

	// Complete the pipeline run (ListUnpostedFindings JOINs pipeline_runs WHERE status = 'completed').
	require.NoError(t, st.CompletePipelineRun(ctx, run.ID, core.PipelineStatusCompleted, adapter.PipelineRunStats{
		TasksTotal: 1, TasksCompleted: 1,
		FindingsTotal: 1,
	}))

	// Should appear (not posted, not suppressed, run completed).
	unposted, err := st.ListUnpostedFindings(ctx, run.ID)
	require.NoError(t, err)
	assert.Len(t, unposted, 1)

	// Post it.
	require.NoError(t, st.MarkFindingPosted(ctx, f.ID, 123))

	// Should disappear.
	unposted, err = st.ListUnpostedFindings(ctx, run.ID)
	require.NoError(t, err)
	assert.Empty(t, unposted)
}

// ---------------------------------------------------------------------------
// DismissedFingerprint tests
// ---------------------------------------------------------------------------

func TestIsFingerprintDismissed(t *testing.T) {
	_, st := setupTestDB(t)
	ctx := context.Background()

	hash := "abc123hash"
	repo := "ashita-ai/mimir"

	// Not dismissed yet.
	dismissed, err := st.IsFingerprintDismissed(ctx, hash, repo)
	require.NoError(t, err)
	assert.False(t, dismissed)

	// Dismiss it.
	err = st.DismissFingerprint(ctx, hash, repo, "reviewer", "false positive")
	require.NoError(t, err)

	// Now it should be dismissed.
	dismissed, err = st.IsFingerprintDismissed(ctx, hash, repo)
	require.NoError(t, err)
	assert.True(t, dismissed)

	// Different repo — not dismissed.
	dismissed, err = st.IsFingerprintDismissed(ctx, hash, "other/repo")
	require.NoError(t, err)
	assert.False(t, dismissed)

	// Upsert should not error.
	err = st.DismissFingerprint(ctx, hash, repo, "reviewer2", "updated reason")
	require.NoError(t, err)
}

// ---------------------------------------------------------------------------
// FindingEvent tests
// ---------------------------------------------------------------------------

func TestCreateAndListFindingEvents(t *testing.T) {
	_, st := setupTestDB(t)
	ctx := context.Background()

	pr := insertTestPR(t, st)
	run := insertTestPipelineRun(t, st, pr.ID, pr.HeadSHA)
	task := insertTestTask(t, st, pr.ID, run.ID)
	f := insertTestFinding(t, st, pr.ID, run.ID, task.ID, pr.HeadSHA)

	newVal := "thumbs_up"
	err := st.CreateFindingEvent(ctx, f.ID, "thumbs_up", "testuser", nil, &newVal)
	require.NoError(t, err)

	events, err := st.ListEventsForFinding(ctx, f.ID)
	require.NoError(t, err)
	require.Len(t, events, 1)
	assert.Equal(t, "thumbs_up", events[0].EventType)
	assert.Equal(t, "testuser", events[0].Actor)
	require.NotNil(t, events[0].NewValue)
	assert.Equal(t, "thumbs_up", *events[0].NewValue)
}

// ---------------------------------------------------------------------------
// WithTx test
// ---------------------------------------------------------------------------

func TestWithTx_Commit(t *testing.T) {
	_, st := setupTestDB(t)
	ctx := context.Background()

	var prID uuid.UUID
	err := st.WithTx(ctx, func(txStore adapter.StoreAdapter, _ pgx.Tx) error {
		pr := &core.PullRequest{
			ExternalPRID: time.Now().UnixNano(),
			RepoFullName: "ashita-ai/mimir",
			PRNumber:     99,
			HeadSHA:      "txtest",
			BaseSHA:      "base",
			Author:       "txuser",
			State:        core.PRStateOpen,
			Metadata:     json.RawMessage("{}"),
		}
		if err := txStore.UpsertPullRequest(ctx, pr); err != nil {
			return err
		}
		prID = pr.ID
		return nil
	})
	require.NoError(t, err)

	// Should be visible outside tx.
	got, err := st.GetPullRequest(ctx, prID)
	require.NoError(t, err)
	assert.Equal(t, "txtest", got.HeadSHA)
}

func TestWithTx_Rollback(t *testing.T) {
	_, st := setupTestDB(t)
	ctx := context.Background()

	var prID uuid.UUID
	err := st.WithTx(ctx, func(txStore adapter.StoreAdapter, _ pgx.Tx) error {
		pr := &core.PullRequest{
			ExternalPRID: time.Now().UnixNano(),
			RepoFullName: "ashita-ai/mimir",
			PRNumber:     99,
			HeadSHA:      "rollbacktest",
			BaseSHA:      "base",
			Author:       "txuser",
			State:        core.PRStateOpen,
			Metadata:     json.RawMessage("{}"),
		}
		if err := txStore.UpsertPullRequest(ctx, pr); err != nil {
			return err
		}
		prID = pr.ID
		return fmt.Errorf("simulated error")
	})
	require.Error(t, err)

	// Should NOT be visible — rolled back.
	_, err = st.GetPullRequest(ctx, prID)
	require.Error(t, err)
}

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

func insertTestPR(t *testing.T, st *store.Store) *core.PullRequest {
	t.Helper()
	pr := &core.PullRequest{
		ExternalPRID: time.Now().UnixNano(), // unique per test
		RepoFullName: "ashita-ai/mimir",
		PRNumber:     1,
		HeadSHA:      uuid.New().String()[:8],
		BaseSHA:      "base000",
		Author:       "testuser",
		State:        core.PRStateOpen,
		Metadata:     json.RawMessage("{}"),
	}
	require.NoError(t, st.UpsertPullRequest(context.Background(), pr))
	return pr
}

func insertTestPipelineRun(t *testing.T, st *store.Store, prID uuid.UUID, headSHA string) *core.PipelineRun {
	t.Helper()
	run := &core.PipelineRun{
		PullRequestID: prID,
		HeadSHA:       headSHA,
		PromptVersion: "v1.0-test",
		ConfigHash:    "testhash",
		Metadata:      json.RawMessage("{}"),
	}
	require.NoError(t, st.CreatePipelineRun(context.Background(), run))
	return run
}

func insertTestTask(t *testing.T, st *store.Store, prID, runID uuid.UUID) *core.ReviewTask {
	t.Helper()
	task := &core.ReviewTask{
		PullRequestID: prID,
		PipelineRunID: runID,
		TaskType:      core.TaskTypeSecurity,
		FilePath:      "internal/auth/token.go",
		Symbol:        "ValidateToken",
		RiskScore:     0.8,
		ModelID:       "test-model",
	}
	require.NoError(t, st.CreateReviewTask(context.Background(), task))
	return task
}

func insertTestFinding(t *testing.T, st *store.Store, prID, runID, taskID uuid.UUID, headSHA string) *core.Finding {
	t.Helper()
	f := makeFinding(prID, runID, taskID, core.LocationFingerprint("ashita-ai/mimir", "file.go", "Fn", core.CategoryLogic), headSHA)
	require.NoError(t, st.CreateFinding(context.Background(), f))
	return f
}

func makeFinding(prID, runID, taskID uuid.UUID, locationHash, headSHA string) *core.Finding {
	line := 10
	return &core.Finding{
		ReviewTaskID:    taskID,
		PullRequestID:   prID,
		PipelineRunID:   runID,
		RepoFullName:    "ashita-ai/mimir",
		FilePath:        "file.go",
		StartLine:       &line,
		Symbol:          "Fn",
		Category:        core.CategoryLogic,
		ConfidenceTier:  core.ConfidenceMedium,
		ConfidenceScore: 0.65,
		Severity:        core.SeverityMedium,
		Title:           "Test finding",
		Body:            "This is a test finding.",
		LocationHash:    locationHash,
		HeadSHA:         headSHA,
		ModelID:         "test-model",
		Metadata:        json.RawMessage("{}"),
	}
}
