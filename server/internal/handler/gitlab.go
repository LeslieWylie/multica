package handler

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/middleware"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// ── GitLab merge-request integration ────────────────────────────────────────
//
// Unlike GitHub App installations (one platform-wide app, per-account
// installation, GitHub-issued installation_id), GitLab has no equivalent
// "app" concept: each project owner configures its own project-level
// webhook with a shared secret token. This integration mirrors that model —
// one webhook_token per workspace (migration 128), used both as the URL
// path segment that routes an inbound webhook to a workspace AND as the
// value GitLab's X-Gitlab-Token header must echo (the project's Webhook
// "Secret token" field is set to the same value). There is deliberately no
// OAuth/installation flow, no repository auto-discovery, and no CI/pipeline
// sync — see the PR description for scope. The read path (issue detail PR
// card, ListPullRequestsByIssue) and the auto-link/auto-close engine
// (extractIdentifiers/lookupIssueByIdentifier/advanceIssueToDone) are
// entirely reused from github.go — this file only adds MR ingestion.

// GitLabIntegrationResponse is the JSON shape returned by the integration
// status/webhook-URL endpoints. WebhookURL is only ever included in the
// create/rotate response (the moment the plaintext token is known) — same
// "shown once" pattern as WebhookSubscriptionResponse's signing secret.
type GitLabIntegrationResponse struct {
	WorkspaceID string  `json:"workspace_id"`
	Configured  bool    `json:"configured"`
	WebhookURL  *string `json:"webhook_url,omitempty"`
	CreatedAt   *string `json:"created_at,omitempty"`
}

const gitlabWebhookTokenPrefix = "glwt_"

func generateGitLabWebhookToken() (string, error) {
	return generateCredential(gitlabWebhookTokenPrefix)
}

// GetGitLabIntegration reports whether the workspace has a GitLab webhook
// configured, without revealing the token (it is only ever shown once, on
// create/rotate — see CreateOrRotateGitLabIntegration). Mounted in the
// member-visible route group (mirrors ListGitHubInstallations) — auth is
// enforced by that group's middleware, not re-checked here.
func (h *Handler) GetGitLabIntegration(w http.ResponseWriter, r *http.Request) {
	workspaceID := chi.URLParam(r, "id")
	wsUUID, ok := parseUUIDOrBadRequest(w, workspaceID, "workspace id")
	if !ok {
		return
	}
	integ, err := h.Queries.GetGitLabIntegrationByWorkspace(r.Context(), wsUUID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeJSON(w, http.StatusOK, GitLabIntegrationResponse{
				WorkspaceID: workspaceID,
				Configured:  false,
			})
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to load GitLab integration")
		return
	}
	writeJSON(w, http.StatusOK, GitLabIntegrationResponse{
		WorkspaceID: workspaceID,
		Configured:  true,
		CreatedAt:   timestampToPtr(integ.CreatedAt),
	})
}

// CreateOrRotateGitLabIntegration issues a fresh webhook token for the
// workspace (creating the row if absent, replacing the token if already
// configured — ON CONFLICT DO UPDATE in the query). The full webhook URL is
// returned in the body exactly once; subsequent GetGitLabIntegration calls
// only report configured=true. Rotating invalidates the previous URL
// immediately (old inbound requests 401), matching webhook_subscription's
// rotate semantics elsewhere in this handler package. Mounted in the
// admin-only route group (mirrors GitHubConnect/DeleteGitHubInstallation) —
// this credential lets an inbound webhook mutate issue status, same
// privilege bar as CreateWebhookSubscription.
func (h *Handler) CreateOrRotateGitLabIntegration(w http.ResponseWriter, r *http.Request) {
	workspaceID := chi.URLParam(r, "id")
	wsUUID, ok := parseUUIDOrBadRequest(w, workspaceID, "workspace id")
	if !ok {
		return
	}
	member, ok := middleware.MemberFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusInternalServerError, "member not found in context")
		return
	}
	token, err := generateGitLabWebhookToken()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to generate webhook token")
		return
	}
	integ, err := h.Queries.CreateGitLabIntegration(r.Context(), db.CreateGitLabIntegrationParams{
		WorkspaceID:  wsUUID,
		WebhookToken: token,
		CreatedByID:  member.UserID,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to save GitLab integration")
		return
	}
	url := gitlabWebhookURL(r, integ.WebhookToken)
	writeJSON(w, http.StatusCreated, GitLabIntegrationResponse{
		WorkspaceID: workspaceID,
		Configured:  true,
		WebhookURL:  &url,
		CreatedAt:   timestampToPtr(integ.CreatedAt),
	})
}

// gitlabWebhookURL builds the absolute webhook URL a user pastes into
// GitLab's project Settings → Webhooks page. Derives scheme/host from the
// inbound request rather than a hardcoded base — this handler has no
// equivalent of GitHub App's fixed callback URL to anchor on, and the
// workspace could be served from any configured host.
func gitlabWebhookURL(r *http.Request, token string) string {
	scheme := "https"
	if r.TLS == nil && r.Header.Get("X-Forwarded-Proto") != "https" {
		scheme = "http"
	}
	return fmt.Sprintf("%s://%s/api/webhooks/gitlab/%s", scheme, r.Host, token)
}

// verifyGitLabToken constant-time compares the X-Gitlab-Token header against
// the URL path token. GitLab echoes back whatever is configured as the
// project webhook's "Secret token" field — operators are instructed to set
// that field to the same value as the URL token, so a mismatch here means
// either a misconfigured project or a forged request; both are rejected
// identically to avoid leaking which case occurred.
func verifyGitLabToken(header http.Header, urlToken string) bool {
	got := header.Get("X-Gitlab-Token")
	return subtle.ConstantTimeCompare([]byte(got), []byte(urlToken)) == 1
}

// HandleGitLabWebhook is the inbound merge-request webhook endpoint,
// mounted at POST /api/webhooks/gitlab/{token}. The path token is the sole
// workspace router (looked up via GetGitLabIntegrationByToken) and doubles
// as the webhook secret verified against X-Gitlab-Token.
func (h *Handler) HandleGitLabWebhook(w http.ResponseWriter, r *http.Request) {
	token := chi.URLParam(r, "token")
	if token == "" {
		writeError(w, http.StatusUnauthorized, "missing token")
		return
	}
	integ, err := h.Queries.GetGitLabIntegrationByToken(r.Context(), token)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			slog.Warn("gitlab: lookup integration failed", "err", err)
		}
		// Same response whether the token is malformed or simply unknown —
		// no information about which case occurred.
		writeError(w, http.StatusUnauthorized, "invalid token")
		return
	}
	if !verifyGitLabToken(r.Header, token) {
		writeError(w, http.StatusUnauthorized, "invalid token")
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxWebhookBodyBytes))
	if err != nil {
		writeError(w, http.StatusBadRequest, "read body failed")
		return
	}

	event := strings.TrimSpace(r.Header.Get("X-Gitlab-Event"))
	switch event {
	case "Merge Request Hook":
		h.handleMergeRequestEvent(r.Context(), integ.WorkspaceID, body)
	default:
		// Acknowledge every event so GitLab doesn't mark the endpoint
		// failing, but ignore types we don't model (push, note, pipeline,
		// etc. — those are handled by the separate Octo-notification
		// webhook path, not this issue-linking one).
	}
	w.WriteHeader(http.StatusAccepted)
}

// glMergeRequestPayload models only the fields this handler consumes from
// GitLab's "Merge Request Hook" payload. Field names/shape verified against
// adapter_gitlab.go's glMergeRequestEvent (Octo's GitLab adapter) during the
// A3 investigation — both sides parse the same GitLab-defined wire shape.
type glMergeRequestPayload struct {
	ObjectAttributes struct {
		IID                 int32  `json:"iid"`
		Title               string `json:"title"`
		Description         string `json:"description"`
		State               string `json:"state"`
		URL                 string `json:"url"`
		SourceBranch        string `json:"source_branch"`
		WorkInProgress      bool   `json:"work_in_progress"`
		MergeStatus         string `json:"merge_status"`
		DetailedMergeStatus string `json:"detailed_merge_status"`
		MergeCommitSha      string `json:"merge_commit_sha"`
	} `json:"object_attributes"`
	Project struct {
		PathWithNamespace string `json:"path_with_namespace"`
	} `json:"project"`
	User struct {
		Username string `json:"username"`
	} `json:"user"`
}

// handleMergeRequestEvent upserts the MR mirror row, then runs the same
// auto-link + auto-close-on-merge logic as GitHub's handlePullRequestEvent
// (github.go:866-956), reusing extractIdentifiers/extractClosingIdentifiers/
// lookupIssueByIdentifier/LinkIssueToPullRequest/advanceIssueToDone
// verbatim. GitLab MR webhooks don't include per-commit diff stats
// (additions/deletions/changed_files), so those fields are left at their
// zero value — the frontend already treats total==0 as "unknown" and hides
// the stats row (see pull-request-list.tsx), so this degrades gracefully
// rather than needing a GitLab-specific UI path.
func (h *Handler) handleMergeRequestEvent(ctx context.Context, workspaceID pgtype.UUID, body []byte) {
	var p glMergeRequestPayload
	if err := json.Unmarshal(body, &p); err != nil {
		slog.Warn("gitlab: bad merge_request payload", "err", err)
		return
	}

	state := deriveMRState(p.ObjectAttributes.State, p.ObjectAttributes.WorkInProgress)
	mergeable := deriveMRMergeableState(p.ObjectAttributes.DetailedMergeStatus, p.ObjectAttributes.MergeStatus)

	pathParts := strings.Split(p.Project.PathWithNamespace, "/")
	repoOwner, repoName := p.Project.PathWithNamespace, ""
	if n := len(pathParts); n >= 2 {
		repoOwner = strings.Join(pathParts[:n-1], "/")
		repoName = pathParts[n-1]
	}

	// GitLab's Merge Request Hook payload carries no separate MR
	// created_at/updated_at fields on object_attributes (unlike GitHub's
	// pull_request payload) — stamp both with "when this webhook was
	// processed" instead. This means pr_created_at drifts forward on every
	// webhook delivery rather than reflecting the MR's true creation time;
	// acceptable for v1 since the frontend only displays pr_updated_at
	// (relative "updated Xm ago"), not pr_created_at.
	now := pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true}
	pr, err := h.Queries.UpsertGitLabMergeRequest(ctx, db.UpsertGitLabMergeRequestParams{
		WorkspaceID:    workspaceID,
		RepoOwner:      repoOwner,
		RepoName:       repoName,
		PrNumber:       p.ObjectAttributes.IID,
		Title:          p.ObjectAttributes.Title,
		State:          state,
		HtmlUrl:        p.ObjectAttributes.URL,
		PrCreatedAt:    now,
		PrUpdatedAt:    now,
		HeadSha:        p.ObjectAttributes.MergeCommitSha,
		Branch:         strToText(p.ObjectAttributes.SourceBranch),
		AuthorLogin:    strToText(p.User.Username),
		MergedAt:       mergedAtIfState(state, now),
		MergeableState: mergeable,
	})
	if err != nil {
		slog.Warn("gitlab: upsert mr failed", "err", err)
		return
	}

	workspaceIDStr := uuidToString(workspaceID)
	resp := githubPullRequestToResponse(pr)

	// Unlike GitHub's handlePullRequestEvent, this does not check an
	// equivalent of workspaceAutoLinkPRsEnabled — there is no separate
	// "GitLab enabled" workspace setting for v1 (see PR description's "not
	// covered" section). Configuring the webhook at all (creating a
	// gitlab_integration row) is treated as consent to auto-link; a
	// workspace that wants MR mirroring without auto-link would need to
	// delete the integration entirely, same tradeoff GitHub's flag was
	// introduced (RFC MUL-2414) specifically to avoid.
	linkedIssueIDs := make([]string, 0)
	idents := extractIdentifiers(p.ObjectAttributes.Title, p.ObjectAttributes.Description, p.ObjectAttributes.SourceBranch)
	closingIdents := map[string]struct{}{}
	for _, c := range extractClosingIdentifiers(p.ObjectAttributes.Title, p.ObjectAttributes.Description) {
		closingIdents[c] = struct{}{}
	}
	prefix := h.getIssuePrefix(ctx, workspaceID)
	reevalIssues := make([]db.Issue, 0, len(idents))
	for _, id := range idents {
		issue, ok := h.lookupIssueByIdentifier(ctx, workspaceID, prefix, id)
		if !ok {
			continue
		}
		_, declared := closingIdents[id]
		// PreserveCloseIntent is always false here (GitHub's equivalent,
		// preserveCloseIntent in handlePullRequestEvent, guards against a
		// post-merge title/body edit clobbering the merge-time close
		// decision — this handler has no action field to detect that case
		// on, and in practice GitLab does not re-fire Merge Request Hook
		// for a merged MR's title/description after the fact, so the gap
		// is theoretical for v1).
		if err := h.Queries.LinkIssueToPullRequest(ctx, db.LinkIssueToPullRequestParams{
			IssueID:       issue.ID,
			PullRequestID: pr.ID,
			CloseIntent:   declared,
			LinkedByType:  strToText("system"),
			LinkedByID:    pgtype.UUID{},
		}); err != nil {
			slog.Warn("gitlab: link failed", "err", err)
			continue
		}
		linkedIssueIDs = append(linkedIssueIDs, uuidToString(issue.ID))
		reevalIssues = append(reevalIssues, issue)
	}

	if state == "merged" || state == "closed" {
		for _, issue := range reevalIssues {
			if issue.Status == "done" || issue.Status == "cancelled" {
				continue
			}
			counts, err := h.Queries.GetIssuePullRequestCloseAggregate(ctx, issue.ID)
			if err != nil {
				slog.Warn("gitlab: count linked mr states failed", "err", err, "issue_id", uuidToString(issue.ID))
				continue
			}
			if counts.OpenCount == 0 && counts.MergedWithCloseIntentCount > 0 {
				h.advanceIssueToDone(ctx, issue, workspaceIDStr, "gitlab_mr_merged")
			}
		}
	}

	h.publish(protocol.EventPullRequestUpdated, workspaceIDStr, "system", "", map[string]any{
		"pull_request":     resp,
		"linked_issue_ids": linkedIssueIDs,
	})
}

// deriveMRState maps GitLab's object_attributes.state ("opened", "closed",
// "merged", "locked") onto the shared github_pull_request.state vocabulary
// ("open", "closed", "merged", "draft"). "locked" (GitLab briefly locks an MR
// during merge processing) has no clean equivalent and is treated as
// "closed" rather than invented as a fifth state the CHECK constraint and
// frontend would both need to learn about.
func deriveMRState(state string, workInProgress bool) string {
	switch state {
	case "merged":
		return "merged"
	case "closed", "locked":
		return "closed"
	default: // "opened", "reopened", or any future value GitLab adds
		if workInProgress {
			return "draft"
		}
		return "open"
	}
}

// deriveMRMergeableState maps GitLab's merge-status vocabulary onto the two
// values the frontend actually renders (see GitHubPullRequestResponse's
// MergeableState doc comment: only "dirty"/"clean" drive UI, everything
// else — including this function's empty-string fallback, which renders as
// pgtype.Text{Valid:false} → nil — shows as "unknown"). Prefers
// detailed_merge_status (GitLab 15.6+) over the older merge_status field
// when both are present; falls back to merge_status for older GitLab
// instances that don't send the newer field.
func deriveMRMergeableState(detailed, legacy string) pgtype.Text {
	status := detailed
	if status == "" {
		status = legacy
	}
	switch status {
	case "cannot_be_merged", "cannot_be_merged_recheck":
		return pgtype.Text{String: "dirty", Valid: true}
	case "can_be_merged", "mergeable":
		return pgtype.Text{String: "clean", Valid: true}
	default:
		return pgtype.Text{}
	}
}

// mergedAtIfState stamps merged_at with the current time when the derived
// state is "merged" — GitLab's webhook payload carries no equivalent field
// on object_attributes (unlike GitHub's pull_request.merged_at), so "the
// moment this webhook was processed" is the closest available
// approximation. Returns an invalid (NULL) timestamp for every other
// state. Note UpsertGitLabMergeRequest's ON CONFLICT always overwrites
// merged_at from the incoming value (same as GitHub's upsert does for its
// own merged_at), so a later non-merge event on an already-merged MR would
// clear this back to NULL — an accepted v1 simplification (see the PR
// description's "not covered" section) since GitLab doesn't fire further
// state-changing events on a merged MR in practice.
func mergedAtIfState(state string, now pgtype.Timestamptz) pgtype.Timestamptz {
	if state != "merged" {
		return pgtype.Timestamptz{}
	}
	return now
}
