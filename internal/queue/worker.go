package queue

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/riverqueue/river"
	"go.uber.org/zap"

	"github.com/ashita-ai/mimir/internal/core"
	"github.com/ashita-ai/mimir/internal/store"
)

// ReviewWorker processes PR review jobs. It orchestrates the full pipeline:
// ingest → index → planner → runtime → policy → store → post.
//
// For M1, the worker upserts the PR record and logs. Pipeline stages
// (index, planner, runtime, policy) will be wired as they are implemented.
type ReviewWorker struct {
	river.WorkerDefaults[ReviewJobArgs]

	Store  *store.Store
	Logger *zap.Logger
}

func (w *ReviewWorker) Work(ctx context.Context, job *river.Job[ReviewJobArgs]) error {
	args := job.Args

	w.Logger.Info("starting PR review",
		zap.String("repo", args.RepoFullName),
		zap.Int("pr", args.PRNumber),
		zap.String("head_sha", args.HeadSHA),
		zap.Int("attempt", job.Attempt),
	)

	// Step 1: Upsert the PR record so all downstream tasks can reference it.
	pr := &core.PullRequest{
		ExternalPRID:   args.ExternalPRID,
		RepoFullName: args.RepoFullName,
		PRNumber:     args.PRNumber,
		HeadSHA:      args.HeadSHA,
		BaseSHA:      args.BaseSHA,
		Author:       args.Author,
		State:        core.PRStateOpen,
		Metadata:     json.RawMessage("{}"),
	}

	if err := w.Store.UpsertPullRequest(ctx, pr); err != nil {
		return fmt.Errorf("upsert pull request: %w", err)
	}

	w.Logger.Info("upserted PR record",
		zap.String("pr_id", pr.ID.String()),
		zap.String("repo", args.RepoFullName),
		zap.Int("pr", args.PRNumber),
	)

	// TODO(M1): wire remaining pipeline stages:
	// 2. FetchPR via ProviderAdapter (get diff, file list)
	// 3. BuildRepoMap via IndexAdapter (tree-sitter parse)
	// 4. GenerateTasks via planner (one ReviewTask per changed symbol)
	// 5. ExecuteTasks via runtime (fan-out with errgroup)
	// 6. FilterFindings via PolicyAdapter (confidence, dedup, triage)
	// 7. PersistFindings via StoreAdapter
	// 8. PostComments via ProviderAdapter

	w.Logger.Info("review job completed (pipeline stages pending implementation)",
		zap.String("pr_id", pr.ID.String()),
	)

	return nil
}
