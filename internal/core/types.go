package core

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// --- Enum types ---

// FindingCategory classifies the kind of review a finding addresses.
type FindingCategory string

const (
	CategorySecurity     FindingCategory = "security"
	CategoryLogic        FindingCategory = "logic"
	CategoryTestCoverage FindingCategory = "test_coverage"
	CategoryStyle        FindingCategory = "style"
	CategoryPerformance  FindingCategory = "performance"
)

// ConfidenceTier is the model's self-assessed confidence bucket.
type ConfidenceTier string

const (
	ConfidenceHigh   ConfidenceTier = "high"   // 0.80–1.00
	ConfidenceMedium ConfidenceTier = "medium" // 0.50–0.79
	ConfidenceLow    ConfidenceTier = "low"    // 0.00–0.49
)

// Severity indicates the impact level of a finding.
type Severity string

const (
	SeverityCritical Severity = "critical"
	SeverityHigh     Severity = "high"
	SeverityMedium   Severity = "medium"
	SeverityLow      Severity = "low"
	SeverityInfo     Severity = "info"
)

// PRState tracks the lifecycle of a pull request.
type PRState string

const (
	PRStateOpen   PRState = "open"
	PRStateClosed PRState = "closed"
	PRStateMerged PRState = "merged"
)

// TaskStatus tracks the execution lifecycle of a review task.
// "completed" means the task ran successfully and produced findings (possibly zero).
// "failed" means the task encountered an error (model timeout, parse failure, etc.).
// These are terminal states — a task never transitions out of completed or failed.
type TaskStatus string

const (
	TaskStatusPending   TaskStatus = "pending"
	TaskStatusRunning   TaskStatus = "running"
	TaskStatusCompleted TaskStatus = "completed"
	TaskStatusFailed    TaskStatus = "failed"
)

// PipelineStatus tracks the execution lifecycle of a pipeline run.
type PipelineStatus string

const (
	PipelineStatusRunning   PipelineStatus = "running"
	PipelineStatusCompleted PipelineStatus = "completed"
	PipelineStatusFailed    PipelineStatus = "failed"
)

// TaskType is the review lens applied to a changed symbol.
type TaskType string

const (
	TaskTypeSecurity     TaskType = "security"
	TaskTypeLogic        TaskType = "logic"
	TaskTypeTestCoverage TaskType = "test_coverage"
	TaskTypeStyle        TaskType = "style"
)

// RiskScore is the planner's assessment of how much review attention a
// symbol deserves. Higher values mean higher priority in task ordering.
type RiskScore float64

// HighRiskThreshold is the score above which a symbol always gets reviewed.
const HighRiskThreshold RiskScore = 0.7

// LowRiskThreshold is the score below which a symbol is skipped.
const LowRiskThreshold RiskScore = 0.3

// AddressedStatus tracks whether a finding's underlying code has changed.
type AddressedStatus string

const (
	AddressedUnaddressed     AddressedStatus = "unaddressed"
	AddressedLikelyAddressed AddressedStatus = "likely_addressed"
	AddressedConfirmed       AddressedStatus = "confirmed"
)

// SymbolKind classifies the type of a code symbol.
type SymbolKind string

const (
	SymbolFunc      SymbolKind = "func"
	SymbolMethod    SymbolKind = "method"
	SymbolType      SymbolKind = "type"
	SymbolInterface SymbolKind = "interface"
)

// --- Domain structs ---

// PullRequest is the normalized representation of a PR from any code host.
type PullRequest struct {
	ID           uuid.UUID       `json:"id"`
	ExternalPRID int64           `json:"external_pr_id"`
	RepoFullName string          `json:"repo_full_name"`
	PRNumber     int             `json:"pr_number"`
	HeadSHA      string          `json:"head_sha"`
	BaseSHA      string          `json:"base_sha"`
	Author       string          `json:"author"`
	State        PRState         `json:"state"`
	Diff         string          `json:"diff,omitempty"`
	ChangedFiles []string        `json:"changed_files,omitempty"`
	Metadata     json.RawMessage `json:"metadata,omitempty"`
	CreatedAt    time.Time       `json:"created_at"`
	UpdatedAt    time.Time       `json:"updated_at"`
}

// PipelineRun records a single execution of the review pipeline for a PR.
// This is the primary audit record: "we ran the pipeline at time T with config X."
type PipelineRun struct {
	ID                 uuid.UUID       `json:"id"`
	PullRequestID      uuid.UUID       `json:"pull_request_id"`
	HeadSHA            string          `json:"head_sha"`
	PromptVersion      string          `json:"prompt_version"`
	ConfigHash         string          `json:"config_hash"`
	Status             PipelineStatus  `json:"status"`
	TasksTotal         *int            `json:"tasks_total,omitempty"`
	TasksCompleted     *int            `json:"tasks_completed,omitempty"`
	TasksFailed        *int            `json:"tasks_failed,omitempty"`
	FindingsTotal      *int            `json:"findings_total,omitempty"`
	FindingsPosted     *int            `json:"findings_posted,omitempty"`
	FindingsSuppressed *int            `json:"findings_suppressed,omitempty"`
	StartedAt          time.Time       `json:"started_at"`
	CompletedAt        *time.Time      `json:"completed_at,omitempty"`
	Metadata           json.RawMessage `json:"metadata,omitempty"`
}

// ReviewTask is a single unit of review work: one changed symbol examined
// through one lens (security, logic, test_coverage, style).
type ReviewTask struct {
	ID            uuid.UUID  `json:"id"`
	PullRequestID uuid.UUID  `json:"pull_request_id"`
	PipelineRunID uuid.UUID  `json:"pipeline_run_id"`
	TaskType      TaskType   `json:"task_type"`
	FilePath      string     `json:"file_path"`
	Symbol        string     `json:"symbol,omitempty"`
	RiskScore     RiskScore  `json:"risk_score"`
	ModelID       string     `json:"model_id"`
	DiffHunk      *string    `json:"diff_hunk,omitempty"`
	Status        TaskStatus `json:"status"`
	Error         *string    `json:"error,omitempty"`
	StartedAt     *time.Time `json:"started_at,omitempty"`
	CompletedAt   *time.Time `json:"completed_at,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
}

// Finding is a single review observation produced by a model or static tool.
type Finding struct {
	ID            uuid.UUID `json:"id"`
	ReviewTaskID  uuid.UUID `json:"review_task_id"`
	PullRequestID uuid.UUID `json:"pull_request_id"`
	PipelineRunID uuid.UUID `json:"pipeline_run_id"`

	// Location within the changed file.
	RepoFullName string `json:"repo_full_name"`
	FilePath     string `json:"file_path"`
	StartLine    *int   `json:"start_line,omitempty"`
	EndLine      *int   `json:"end_line,omitempty"`
	Symbol       string `json:"symbol,omitempty"`

	// Classification.
	Category        FindingCategory `json:"category"`
	ConfidenceTier  ConfidenceTier  `json:"confidence_tier"`
	ConfidenceScore float64         `json:"confidence_score"`
	Severity        Severity        `json:"severity"`

	// Content shown to the reviewer.
	Title      string `json:"title"`
	Body       string `json:"body"`
	Suggestion string `json:"suggestion,omitempty"`

	// Fingerprinting for dedup (ADR-0002).
	LocationHash string  `json:"location_hash"`
	ContentHash  *string `json:"content_hash,omitempty"`
	HeadSHA      string  `json:"head_sha"`

	// Lifecycle.
	PostedAt          *time.Time      `json:"posted_at,omitempty"`
	ExternalCommentID *int64          `json:"external_comment_id,omitempty"`
	AddressedStatus   AddressedStatus `json:"addressed_status"`
	SuppressionReason *string         `json:"suppression_reason,omitempty"`
	DismissedAt       *time.Time      `json:"dismissed_at,omitempty"`
	DismissedBy       *string         `json:"dismissed_by,omitempty"`

	// Provenance — which model produced this finding, at what cost.
	ModelID          string          `json:"model_id"`
	PromptTokens     *int            `json:"prompt_tokens,omitempty"`
	CompletionTokens *int            `json:"completion_tokens,omitempty"`
	Metadata         json.RawMessage `json:"metadata,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// FindingEvent is a row in the append-only audit log for finding lifecycle transitions.
type FindingEvent struct {
	ID        uuid.UUID       `json:"id"`
	FindingID uuid.UUID       `json:"finding_id"`
	EventType string          `json:"event_type"`
	Actor     string          `json:"actor,omitempty"`
	OldValue  *string         `json:"old_value,omitempty"`
	NewValue  *string         `json:"new_value,omitempty"`
	Metadata  json.RawMessage `json:"metadata,omitempty"`
	CreatedAt time.Time       `json:"created_at"`
}

// SymbolTable is built once per pipeline run and discarded.
type SymbolTable struct {
	FileSymbols map[string][]Symbol    `json:"file_symbols"`
	SymbolRefs  map[string][]Reference `json:"symbol_refs"`
	TestFiles   map[string][]string    `json:"test_files"`
	ImportGraph map[string][]string    `json:"import_graph"`
}

// Symbol represents a named code entity extracted by tree-sitter.
type Symbol struct {
	Name       string     `json:"name"`
	Kind       SymbolKind `json:"kind"`
	FilePath   string     `json:"file_path"`
	StartLine  int        `json:"start_line"`
	EndLine    int        `json:"end_line"`
	Package    string     `json:"package"`
	Exported   bool       `json:"exported"`
	ParamCount int        `json:"param_count"`
}

// Reference is a location where a symbol is used.
type Reference struct {
	FilePath string `json:"file_path"`
	Line     int    `json:"line"`
	InFunc   string `json:"in_func,omitempty"`
}

// Slice is the token-budgeted context window for a single ReviewTask.
type Slice struct {
	DiffHunk    string `json:"diff_hunk"`
	CallGraph   string `json:"call_graph"`
	TestContext string `json:"test_context"`
	Truncated   bool   `json:"truncated"`
	Approximate bool   `json:"approximate"`
}

// TokenBudget specifies the maximum tokens allowed per slice section.
type TokenBudget struct {
	DiffHunk  int `json:"diff_hunk"`
	CallGraph int `json:"call_graph"`
	Tests     int `json:"tests"`
	Total     int `json:"total"`
}

// APIError represents a structured error from an external API (LLM provider, GitHub).
// Used by isRetryable to classify errors for retry decisions.
type APIError struct {
	StatusCode int            `json:"status_code"`
	Message    string         `json:"message"`
	Provider   string         `json:"provider"`
	RetryAfter *time.Duration `json:"retry_after,omitempty"`
}

func (e *APIError) Error() string {
	return fmt.Sprintf("%s API error %d: %s", e.Provider, e.StatusCode, e.Message)
}

// ValidateConfidenceTier checks that score matches tier.
// Policy may downgrade a tier but must never upgrade one.
func ValidateConfidenceTier(tier ConfidenceTier, score float64) error {
	switch tier {
	case ConfidenceHigh:
		if score < 0.80 {
			return fmt.Errorf("high confidence tier requires score >= 0.80, got %.2f", score)
		}
	case ConfidenceMedium:
		if score < 0.50 || score >= 0.80 {
			return fmt.Errorf("medium confidence tier requires 0.50 <= score < 0.80, got %.2f", score)
		}
	case ConfidenceLow:
		if score >= 0.50 {
			return fmt.Errorf("low confidence tier requires score < 0.50, got %.2f", score)
		}
	default:
		return fmt.Errorf("unknown confidence tier: %s", tier)
	}
	return nil
}
