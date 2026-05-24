package adapter

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ashita-ai/mimir/internal/core"
)

// ErrNotFound is returned when a mutating operation targets a row that does
// not exist (or, for soft-deletable entities, has already been deleted).
var ErrNotFound = errors.New("not found")

// ---------------------------------------------------------------------------
// ProviderAdapter — code hosting platform (GitHub M1, GitLab M2+)
// ---------------------------------------------------------------------------

// ProviderAdapter fetches PR data and posts review results to a code host.
type ProviderAdapter interface {
	// FetchPR returns normalized PR metadata, diff, and file list.
	FetchPR(ctx context.Context, repoFullName string, prNumber int) (*core.PullRequest, error)

	// ListCommits returns commit SHAs for the PR in chronological order.
	ListCommits(ctx context.Context, repoFullName string, prNumber int) ([]string, error)

	// PostComment posts a finding as an inline review comment on a specific diff line.
	// Returns the platform's comment ID for tracking.
	PostComment(ctx context.Context, req PostCommentRequest) (int64, error)

	// PostSummaryComment posts or updates a top-level PR comment (not inline).
	// Returns the platform's comment ID.
	PostSummaryComment(ctx context.Context, repoFullName string, prNumber int, body string) (int64, error)
}

// PostCommentRequest contains the parameters for posting an inline review comment.
type PostCommentRequest struct {
	RepoFullName string `json:"repo_full_name"`
	PRNumber     int    `json:"pr_number"`
	CommitSHA    string `json:"commit_sha"`
	FilePath     string `json:"file_path"`
	Line         int    `json:"line"`
	Body         string `json:"body"`
}

// ---------------------------------------------------------------------------
// ModelAdapter — LLM provider (Anthropic M1; OpenAI, Gemini M2+)
// ---------------------------------------------------------------------------

// ModelAdapter sends a review task + context slice to a model and returns findings.
type ModelAdapter interface {
	// Infer runs model inference for a single review task.
	Infer(ctx context.Context, task core.ReviewTask, slice core.Slice) ([]core.Finding, error)

	// ModelID returns the identifier string (e.g. "claude-opus-4-6").
	ModelID() string

	// MaxTokens returns the model's context window size in tokens.
	MaxTokens() int
}

// ---------------------------------------------------------------------------
// StaticToolAdapter — static analysis tools (semgrep, golangci-lint, custom)
// ---------------------------------------------------------------------------

// StaticToolAdapter runs a static analysis tool over changed files.
type StaticToolAdapter interface {
	// Run executes the tool and returns findings.
	Run(ctx context.Context, task core.ReviewTask, files []string) ([]core.Finding, error)

	// ToolName returns the tool identifier (e.g. "semgrep", "golangci-lint").
	ToolName() string

	// Languages returns the file extensions this tool supports (e.g. [".go", ".py"]).
	Languages() []string
}

// ---------------------------------------------------------------------------
// IndexAdapter — semantic repo map (tree-sitter M1, LSP M2)
// ---------------------------------------------------------------------------

// IndexAdapter builds and queries the semantic repo map for a review run.
type IndexAdapter interface {
	// BuildSymbolTable runs the change cone algorithm (ADR-0004).
	BuildSymbolTable(ctx context.Context, repoPath, baseSHA, headSHA string) (*core.SymbolTable, error)

	// Query returns context content for a review task within the token budget.
	Query(ctx context.Context, req IndexRequest) (*IndexResult, error)

	// IsApproximate returns true if the index is heuristic (not type-resolved).
	IsApproximate() bool
}

// IndexRequest specifies what the index should look up and how much room it has.
type IndexRequest struct {
	RepoPath    string           `json:"repo_path"`
	ChangedFile string           `json:"changed_file"`
	Symbol      string           `json:"symbol"`
	Budget      core.TokenBudget `json:"budget"`
}

// IndexResult holds the context content returned by IndexAdapter.Query.
type IndexResult struct {
	DiffHunk    string `json:"diff_hunk"`
	CallGraph   string `json:"call_graph"`
	TestContext string `json:"test_context"`
}

// ---------------------------------------------------------------------------
// PolicyAdapter — finding gating, triage, and escalation
// ---------------------------------------------------------------------------

// PolicyAdapter gates findings before they are posted to the code host.
type PolicyAdapter interface {
	// Triage partitions findings into posting tiers.
	Triage(ctx context.Context, findings []core.Finding) (inline, summary, suppress []core.Finding)

	// ShouldEscalate returns true if the finding bypasses normal posting rules.
	ShouldEscalate(ctx context.Context, f core.Finding) bool
}

// ---------------------------------------------------------------------------
// StoreAdapter — persistence (PostgreSQL only, ADR-0002)
// ---------------------------------------------------------------------------

// TxFunc is a callback that receives a transaction-scoped StoreAdapter and
// the raw pgx.Tx for components (e.g. River) that need direct tx access.
type TxFunc func(txStore StoreAdapter, tx pgx.Tx) error

// PipelineRunStats is passed to CompletePipelineRun to record final counts.
type PipelineRunStats struct {
	TasksTotal         int
	TasksCompleted     int
	TasksFailed        int
	FindingsTotal      int
	FindingsPosted     int
	FindingsSuppressed int
}

// StoreAdapter is the persistence boundary for all domain objects.
type StoreAdapter interface {
	// --- Transactions ---

	WithTx(ctx context.Context, fn TxFunc) error

	// --- Pull Requests ---

	UpsertPullRequest(ctx context.Context, pr *core.PullRequest) error
	GetPullRequest(ctx context.Context, id uuid.UUID) (*core.PullRequest, error)
	SoftDeletePullRequest(ctx context.Context, id uuid.UUID) error

	// --- Pipeline Runs ---

	CreatePipelineRun(ctx context.Context, run *core.PipelineRun) error
	CompletePipelineRun(ctx context.Context, id uuid.UUID, status core.PipelineStatus, stats PipelineRunStats) error
	GetPipelineRun(ctx context.Context, id uuid.UUID) (*core.PipelineRun, error)
	ListPipelineRunsForPR(ctx context.Context, prID uuid.UUID) ([]core.PipelineRun, error)
	ReconcileStalePipelineRuns(ctx context.Context, staleThreshold time.Duration) error

	// --- Review Tasks ---

	CreateReviewTask(ctx context.Context, task *core.ReviewTask) error
	UpdateReviewTaskStatus(ctx context.Context, id uuid.UUID, status string, errMsg *string) error
	ListReviewTasksForPR(ctx context.Context, prID uuid.UUID) ([]core.ReviewTask, error)
	ListReviewTasksForRun(ctx context.Context, runID uuid.UUID) ([]core.ReviewTask, error)
	CountTaskStats(ctx context.Context, runID uuid.UUID) (total, completed, failed int, err error)

	// --- Findings ---

	CreateFinding(ctx context.Context, f *core.Finding) error
	ListFindingsForPR(ctx context.Context, prID uuid.UUID) ([]core.Finding, error)
	ListFindingsForRun(ctx context.Context, runID uuid.UUID) ([]core.Finding, error)
	MarkFindingPosted(ctx context.Context, id uuid.UUID, commentID int64) error
	MarkFindingAddressed(ctx context.Context, id uuid.UUID, status core.AddressedStatus) error
	FindPriorFinding(ctx context.Context, prID uuid.UUID, locationHash string) (*core.Finding, error)
	ListUnaddressedFindings(ctx context.Context, prID uuid.UUID) ([]core.Finding, error)
	ListUnpostedFindings(ctx context.Context, pipelineRunID uuid.UUID) ([]core.Finding, error)

	// --- Dismissed Fingerprints ---

	IsFingerprintDismissed(ctx context.Context, fingerprint, repoFullName string) (bool, error)
	DismissFingerprint(ctx context.Context, fingerprint, repoFullName, dismissedBy, reason string) error

	// --- Finding Events ---

	CreateFindingEvent(ctx context.Context, findingID uuid.UUID, eventType, actor string, oldValue, newValue *string) error
	ListEventsForFinding(ctx context.Context, findingID uuid.UUID) ([]core.FindingEvent, error)
}
