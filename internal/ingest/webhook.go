package ingest

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/google/go-github/v67/github"
	"go.uber.org/zap"
)

// PREvent is the normalized data extracted from a GitHub pull_request webhook.
// The serve layer converts this into a River job.
type PREvent struct {
	RepoFullName string `json:"repo_full_name"`
	PRNumber     int    `json:"pr_number"`
	ExternalPRID   int64  `json:"external_pr_id"`
	HeadSHA      string `json:"head_sha"`
	BaseSHA      string `json:"base_sha"`
	Author       string `json:"author"`
	Action       string `json:"action"` // "opened", "synchronize", or "reopened"
}

// WebhookHandler receives GitHub webhook events and dispatches PR events
// via the OnPREvent callback. It validates the HMAC signature and ignores
// event types / actions that aren't relevant to the review pipeline.
type WebhookHandler struct {
	// OnPREvent is called for every PR opened/synchronize/reopened event.
	// The serve layer wires this to enqueue a River job.
	// The context is the HTTP request context, not the server's lifecycle context.
	OnPREvent func(ctx context.Context, event PREvent) error

	// Secret is the GitHub webhook secret for HMAC validation.
	// Must be non-empty; the serve command enforces this at startup.
	Secret []byte

	Logger *zap.Logger
}

func (h *WebhookHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	payload, err := github.ValidatePayload(r, h.Secret)
	if err != nil {
		h.Logger.Warn("webhook signature validation failed", zap.Error(err))
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}

	eventType := github.WebHookType(r)
	if eventType != "pull_request" {
		// Acknowledge but ignore non-PR events.
		w.WriteHeader(http.StatusOK)
		return
	}

	var event github.PullRequestEvent
	if err := json.Unmarshal(payload, &event); err != nil {
		h.Logger.Error("failed to parse pull_request event", zap.Error(err))
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}

	action := event.GetAction()
	if action != "opened" && action != "synchronize" && action != "reopened" {
		w.WriteHeader(http.StatusOK)
		return
	}

	pr := event.GetPullRequest()
	if pr == nil {
		http.Error(w, "missing pull_request in payload", http.StatusBadRequest)
		return
	}

	repo := event.GetRepo()
	if repo == nil {
		http.Error(w, "missing repo in payload", http.StatusBadRequest)
		return
	}

	prEvent := PREvent{
		RepoFullName: repo.GetFullName(),
		PRNumber:     pr.GetNumber(),
		ExternalPRID:   pr.GetID(),
		HeadSHA:      pr.GetHead().GetSHA(),
		BaseSHA:      pr.GetBase().GetSHA(),
		Author:       pr.GetUser().GetLogin(),
		Action:       action,
	}

	h.Logger.Info("received PR event",
		zap.String("repo", prEvent.RepoFullName),
		zap.Int("pr", prEvent.PRNumber),
		zap.String("action", action),
		zap.String("head_sha", prEvent.HeadSHA),
	)

	if err := h.OnPREvent(r.Context(), prEvent); err != nil {
		h.Logger.Error("failed to enqueue review job",
			zap.String("repo", prEvent.RepoFullName),
			zap.Int("pr", prEvent.PRNumber),
			zap.Error(err),
		)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusAccepted)
}
