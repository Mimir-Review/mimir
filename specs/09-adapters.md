# Spec 09: Adapter Interfaces

> **Status:** Approved
> **Date:** 2026-03-27
> **Package:** `pkg/adapter`
> **Scratchpad:** Promotes `scratchpad/plugin-interfaces.md` to locked spec

---

## Design Principles

1. Every method accepts `context.Context` first. Cancellation propagates through the pipeline.
2. Wrap errors at boundaries: `fmt.Errorf("github: fetch PR: %w", err)`. No raw I/O errors escape.
3. Adapters are stateless per-request. Long-lived state (DB pools, HTTP clients, token caches) is injected at construction.
4. Each interface has ≤ 5 methods. Split if larger.
5. No adapter implementation lives in `pkg/adapter`. The package exports only types and interfaces.

---

## ProviderAdapter

```go
// ProviderAdapter abstracts the code hosting platform.
// M1: GitHubProvider. M2+: GitLabProvider.
type ProviderAdapter interface {
    // FetchPR returns normalized PR metadata, diff, and file list.
    FetchPR(ctx context.Context, repoFullName string, prNumber int) (*core.PullRequest, error)

    // ListCommits returns commit SHAs for the PR in chronological order.
    ListCommits(ctx context.Context, repoFullName string, prNumber int) ([]string, error)

    // PostComment posts a finding as an inline review comment.
    // Returns the platform's comment ID.
    PostComment(ctx context.Context, req PostCommentRequest) (int64, error)

    // PostSummaryComment posts or updates a top-level PR comment.
    // Returns the comment ID.
    PostSummaryComment(ctx context.Context, repoFullName string, prNumber int, body string) (int64, error)
}

type PostCommentRequest struct {
    RepoFullName string
    PRNumber     int
    CommitSHA    string
    FilePath     string
    Line         int
    Body         string
}
```

**M1 implementation:** `internal/ingest.GitHubProvider`

---

## ModelAdapter

```go
// ModelAdapter abstracts LLM inference.
// M1: AnthropicModel (direct API). M2+: BedrockModel, OpenAIModel, GeminiModel.
type ModelAdapter interface {
    // Infer sends a review task + context slice to the model.
    // Returns structured findings parsed from the model's response.
    Infer(ctx context.Context, task core.ReviewTask, slice core.Slice) ([]core.Finding, error)

    // ModelID returns the model identifier (e.g. "claude-opus-4-6").
    ModelID() string

    // MaxTokens returns the model's context window size.
    MaxTokens() int
}
```

**M1 implementation:** `internal/runtime.AnthropicModel`

### AnthropicModel Constructor

```go
type AnthropicModelConfig struct {
    APIKey     string
    ModelID    string // e.g. "claude-opus-4-6"
    MaxTokens  int
    HTTPClient *http.Client // optional; defaults to http.DefaultClient with timeout
}

func NewAnthropicModel(cfg AnthropicModelConfig) *AnthropicModel
```

The `Infer` method:
1. Assembles the Messages API request (system prompt + user message with slice)
2. Sends via `POST https://api.anthropic.com/v1/messages`
3. Parses the structured JSON response from the model's output
4. Maps to `[]core.Finding`, populating `ModelID`, `PromptTokens`, `CompletionTokens` from the API response

---

## StaticToolAdapter

```go
// StaticToolAdapter abstracts static analysis tools.
// M1: SemgrepTool. M2: GolangciLintTool, custom.
type StaticToolAdapter interface {
    // Run executes the tool on the given files and returns findings.
    Run(ctx context.Context, task core.ReviewTask, files []string) ([]core.Finding, error)

    // ToolName returns the tool identifier (e.g. "semgrep").
    ToolName() string

    // Languages returns file extensions this tool supports (e.g. [".go", ".py"]).
    Languages() []string
}
```

**M1 implementation:** `internal/runtime.SemgrepTool` — runs `semgrep --json` with a curated ruleset. Static tool findings are injected into the model prompt as grounding context AND collected as findings in their own right. M2 adds golangci-lint and custom tool support.

---

## IndexAdapter

```go
// IndexAdapter abstracts the semantic repo index.
// M1: TreeSitterIndex. M2: LSPIndex (gopls).
type IndexAdapter interface {
    // BuildSymbolTable constructs the in-memory symbol table for a PR.
    // Called once per pipeline run.
    BuildSymbolTable(ctx context.Context, repoPath string, baseSHA, headSHA string) (*core.SymbolTable, error)

    // Query returns context content for a review task within the token budget.
    Query(ctx context.Context, req IndexRequest) (*IndexResult, error)

    // IsApproximate returns true if the index is heuristic (not type-resolved).
    IsApproximate() bool
}

type IndexRequest struct {
    RepoPath    string
    ChangedFile string
    Symbol      string
    Budget      core.TokenBudget
}

type IndexResult struct {
    DiffHunk    string // raw diff + surrounding context
    CallGraph   string // formatted caller/callee context
    TestContext string // formatted test function content
}
```

**Change from scratchpad draft:** Added `BuildSymbolTable` as a separate method (was implicit in `Query`). The symbol table is built once and shared across all `Query` calls for the same pipeline run. This avoids re-parsing files for every task.

**M1 implementation:** `internal/index.TreeSitterIndex`

---

## PolicyAdapter

```go
// PolicyAdapter gates findings before posting.
type PolicyAdapter interface {
    // Triage partitions findings into posting tiers.
    // Returns: inline (posted as diff comments), summary (summary table only),
    // suppress (not posted).
    // Enforces escalation rules and dedup. All high-confidence findings are posted inline.
    Triage(ctx context.Context, findings []core.Finding) (inline, summary, suppress []core.Finding)

    // ShouldEscalate returns true if the finding bypasses normal posting rules.
    // Used by Triage internally; exposed for testing and simple policy implementations.
    ShouldEscalate(ctx context.Context, f core.Finding) bool
}
```

**Change from scratchpad draft:** Removed `ShouldPost`. `Triage` is the primary API and subsumes per-finding posting decisions. `ShouldEscalate` is retained for testing and simple overrides.

**M1 implementation:** `internal/policy.DefaultPolicy`

---

## StoreAdapter

```go
// StoreAdapter is the persistence boundary for all domain objects.
type StoreAdapter interface {
    // Pull requests
    UpsertPullRequest(ctx context.Context, pr *core.PullRequest) error
    GetPullRequest(ctx context.Context, id uuid.UUID) (*core.PullRequest, error)
    SoftDeletePullRequest(ctx context.Context, id uuid.UUID) error

    // Pipeline runs
    CreatePipelineRun(ctx context.Context, run *core.PipelineRun) error
    CompletePipelineRun(ctx context.Context, id uuid.UUID, status core.PipelineStatus, stats PipelineRunStats) error
    GetPipelineRun(ctx context.Context, id uuid.UUID) (*core.PipelineRun, error)
    ListPipelineRunsForPR(ctx context.Context, prID uuid.UUID) ([]core.PipelineRun, error)

    // Review tasks
    CreateReviewTask(ctx context.Context, task *core.ReviewTask) error
    UpdateReviewTaskStatus(ctx context.Context, id uuid.UUID, status string, errMsg *string) error
    ListReviewTasksForPR(ctx context.Context, prID uuid.UUID) ([]core.ReviewTask, error)
    ListReviewTasksForRun(ctx context.Context, runID uuid.UUID) ([]core.ReviewTask, error)
    CountTaskStats(ctx context.Context, runID uuid.UUID) (total, completed, failed int, err error)

    // Findings
    CreateFinding(ctx context.Context, f *core.Finding) error
    ListFindingsForPR(ctx context.Context, prID uuid.UUID) ([]core.Finding, error)
    ListFindingsForRun(ctx context.Context, runID uuid.UUID) ([]core.Finding, error)
    MarkFindingPosted(ctx context.Context, id uuid.UUID, commentID int64) error
    MarkFindingAddressed(ctx context.Context, id uuid.UUID, status core.AddressedStatus) error
    FindPriorFinding(ctx context.Context, prID uuid.UUID, locationHash string) (*core.Finding, error)
    ListUnaddressedFindings(ctx context.Context, prID uuid.UUID) ([]core.Finding, error)
    ListUnpostedFindings(ctx context.Context, pipelineRunID uuid.UUID) ([]core.Finding, error)

    // Pipeline run lifecycle
    ReconcileStalePipelineRuns(ctx context.Context, staleThreshold time.Duration) error

    // Dismissals
    IsFingerprintDismissed(ctx context.Context, fingerprint, repoFullName string) (bool, error)
    DismissFingerprint(ctx context.Context, fingerprint, repoFullName, dismissedBy, reason string) error

    // Events (append-only audit log)
    CreateFindingEvent(ctx context.Context, findingID uuid.UUID, eventType, actor string, oldValue, newValue *string) error
    ListEventsForFinding(ctx context.Context, findingID uuid.UUID) ([]core.FindingEvent, error)

    // Transactions — callback receives a tx-bound StoreAdapter + raw pgx.Tx for River.
    // All store operations within fn execute on the same transaction.
    WithTx(ctx context.Context, fn func(txStore StoreAdapter, tx pgx.Tx) error) error
}

// PipelineRunStats is passed to CompletePipelineRun to record final counts.
type PipelineRunStats struct {
    TasksTotal      int
    TasksCompleted  int
    TasksFailed     int
    FindingsTotal   int
    FindingsPosted  int // inline + summary findings actually posted to the hosting platform
    FindingsSuppressed int // suppressed (duplicate, low_confidence, dismissed)
}
```

**Changes from scratchpad draft:**
- Added `CountTaskStats` for summary comment generation
- Added `FindPriorFinding` and `ListUnaddressedFindings` for dedup and addressed-in-next-commit
- Added `DismissFingerprint` and `CreateFindingEvent`
- Added `WithTx` for transactional operations — **provides a tx-bound StoreAdapter**, not a raw pgx.Tx, to prevent the caller from accidentally using the pool-backed store within a transaction
- `MarkFindingAddressed` takes `AddressedStatus` instead of being boolean
- Added `PipelineRun` CRUD methods for audit trail
- Added `SoftDeletePullRequest` — no hard deletes; all FKs use `ON DELETE RESTRICT`
- Added `ListEventsForFinding` for audit log queries
- `CreateFindingEvent` takes `oldValue`/`newValue` for change tracking

**M1 implementation:** `internal/store.PostgresStore`

---

## Constructor Patterns

Every adapter implementation follows the same pattern:

```go
// Constructor accepts explicit dependencies. No global state, no init().
func NewGitHubProvider(cfg GitHubProviderConfig) *GitHubProvider

// Config struct contains everything the adapter needs.
type GitHubProviderConfig struct {
    AppID          int64
    PrivateKeyPath string
    WebhookSecret  string
    Token          string     // PAT fallback
    HTTPClient     *http.Client
}
```

The wiring layer in `cmd/mimir` constructs all adapters and injects them into pipeline stages. No dependency injection framework — explicit construction.

The `GitHubProviderConfig` example above is implementation-specific (M1). A `GitLabProviderConfig` would have different fields (personal access token, project ID, etc.) but the same constructor pattern. The `ProviderAdapter` interface is the contract — implementations are free to require whatever config they need.

---

## Testing Contract

Each interface should have a conformance test suite that any implementation can import:

```go
// pkg/adapter/adaptertest/provider_suite.go
func TestProviderAdapter(t *testing.T, newProvider func() adapter.ProviderAdapter) {
    t.Run("FetchPR returns normalized PR", func(t *testing.T) { ... })
    t.Run("PostComment is idempotent", func(t *testing.T) { ... })
    t.Run("PostSummaryComment updates on retry", func(t *testing.T) { ... })
}
```

M1 scope: write the test suite for `StoreAdapter` (integration tests against real PostgreSQL). Other suites can use test doubles initially and get conformance suites in M2.
