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
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// ── GitLab merge-request integration ────────────────────────────────────────
//
// Unlike GitHub App installations (one platform-wide app, per-account
// installation, GitHub-issued installation_id), GitLab has no equivalent
// "app" concept: each project owner configures its own project-level
// webhook with a shared secret token. This integration mirrors that
// per-project shape directly — one gitlab_integration row per registered
// GitLab project (migration 155), not one shared token per workspace. A
// workspace can register many projects, mirroring how Octo/Lark already
// support multiple bots per workspace. The webhook URL's path segment is the
// integration's id (a routing key, not itself a secret); the actual
// credential is webhook_secret, verified against the X-Gitlab-Token header.
// Inbound payloads are further checked against the registered
// gitlab_project_id (GitLab's stable numeric identity, not the renameable
// path) before being trusted — closing the hole where a v1 workspace-wide
// token could forge events for any project. The read path (issue detail PR
// card, ListPullRequestsByIssue) and the auto-link/auto-close engine
// (extractIdentifiers/lookupIssueByIdentifier/advanceIssueToDone) are
// entirely reused from github.go — this file only adds MR ingestion.

// GitLabIntegrationResponse is the JSON shape for one registered GitLab
// project integration. WebhookURL / WebhookSecret are only ever included in
// the create response, the moment the plaintext secret is known — same
// "shown once" pattern as WebhookSubscriptionResponse's signing secret.
type GitLabIntegrationResponse struct {
	ID                string  `json:"id"`
	WorkspaceID       string  `json:"workspace_id"`
	GitlabHost        string  `json:"gitlab_host"`
	GitlabProjectID   int64   `json:"gitlab_project_id"`
	GitlabProjectPath string  `json:"gitlab_project_path"`
	CreatedAt         string  `json:"created_at"`
	WebhookURL        *string `json:"webhook_url,omitempty"`
	WebhookSecret     *string `json:"webhook_secret,omitempty"`
}

type ListGitLabIntegrationsResponse struct {
	Integrations []GitLabIntegrationResponse `json:"integrations"`
}

type CreateGitLabIntegrationRequest struct {
	GitlabHost        string `json:"gitlab_host"`
	GitlabProjectID   int64  `json:"gitlab_project_id"`
	GitlabProjectPath string `json:"gitlab_project_path"`
}

func gitlabIntegrationToResponse(integ db.GitlabIntegration) GitLabIntegrationResponse {
	return GitLabIntegrationResponse{
		ID:                uuidToString(integ.ID),
		WorkspaceID:       uuidToString(integ.WorkspaceID),
		GitlabHost:        integ.GitlabHost,
		GitlabProjectID:   integ.GitlabProjectID,
		GitlabProjectPath: integ.GitlabProjectPath,
		CreatedAt:         timestampToString(integ.CreatedAt),
	}
}

const gitlabWebhookSecretPrefix = "glws_"

func generateGitLabWebhookSecret() (string, error) {
	return generateCredential(gitlabWebhookSecretPrefix)
}

// ListGitLabIntegrations lists the workspace's registered GitLab project
// integrations. Never includes webhook_secret — that is shown exactly once,
// in CreateGitLabIntegration's response. Mounted in the member-visible route
// group (mirrors ListGitHubInstallations) — auth is enforced by that group's
// middleware, not re-checked here.
func (h *Handler) ListGitLabIntegrations(w http.ResponseWriter, r *http.Request) {
	workspaceID := chi.URLParam(r, "id")
	wsUUID, ok := parseUUIDOrBadRequest(w, workspaceID, "workspace id")
	if !ok {
		return
	}
	rows, err := h.Queries.ListGitLabIntegrationsByWorkspace(r.Context(), wsUUID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load GitLab integrations")
		return
	}
	resp := ListGitLabIntegrationsResponse{Integrations: make([]GitLabIntegrationResponse, 0, len(rows))}
	for _, row := range rows {
		resp.Integrations = append(resp.Integrations, gitlabIntegrationToResponse(row))
	}
	writeJSON(w, http.StatusOK, resp)
}

// CreateGitLabIntegration registers a GitLab project's webhook: generates a
// fresh secret, stores the (host, numeric project id, display path) triple,
// and returns the full webhook URL + secret exactly once. Re-registering the
// same (workspace, host, project id) rotates the secret rather than erroring
// (the query's ON CONFLICT DO UPDATE) — the previous webhook URL keeps
// routing to the same integration row, but the old secret stops verifying
// immediately. Mounted in the admin-only route group (mirrors
// GitHubConnect/DeleteGitHubInstallation) — this credential lets an inbound
// webhook mutate issue status, same privilege bar as
// CreateWebhookSubscription.
func (h *Handler) CreateGitLabIntegration(w http.ResponseWriter, r *http.Request) {
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
	var req CreateGitLabIntegrationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	req.GitlabHost = strings.TrimSpace(req.GitlabHost)
	req.GitlabProjectPath = strings.TrimSpace(req.GitlabProjectPath)
	if req.GitlabHost == "" {
		writeError(w, http.StatusBadRequest, "gitlab_host is required")
		return
	}
	if req.GitlabProjectID <= 0 {
		writeError(w, http.StatusBadRequest, "gitlab_project_id must be a positive integer")
		return
	}
	if req.GitlabProjectPath == "" {
		writeError(w, http.StatusBadRequest, "gitlab_project_path is required")
		return
	}
	secret, err := generateGitLabWebhookSecret()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to generate webhook secret")
		return
	}
	integ, err := h.Queries.CreateGitLabIntegration(r.Context(), db.CreateGitLabIntegrationParams{
		WorkspaceID:       wsUUID,
		GitlabHost:        req.GitlabHost,
		GitlabProjectID:   req.GitlabProjectID,
		GitlabProjectPath: req.GitlabProjectPath,
		WebhookSecret:     secret,
		CreatedByID:       member.UserID,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to save GitLab integration")
		return
	}
	resp := gitlabIntegrationToResponse(integ)
	url := gitlabWebhookURL(r, uuidToString(integ.ID))
	resp.WebhookURL = &url
	resp.WebhookSecret = &secret
	writeJSON(w, http.StatusCreated, resp)
}

// DeleteGitLabIntegration removes a registered GitLab project integration.
// Scoped by (id, workspace_id) like DeleteGitHubInstallation, so a caller
// can never delete another workspace's integration by guessing an id.
// Admin-only, mirroring DeleteGitHubInstallation's privilege bar.
func (h *Handler) DeleteGitLabIntegration(w http.ResponseWriter, r *http.Request) {
	workspaceID := chi.URLParam(r, "id")
	wsUUID, ok := parseUUIDOrBadRequest(w, workspaceID, "workspace id")
	if !ok {
		return
	}
	id := chi.URLParam(r, "integrationId")
	idUUID, ok := parseUUIDOrBadRequest(w, id, "integration id")
	if !ok {
		return
	}
	if _, err := h.Queries.DeleteGitLabIntegration(r.Context(), db.DeleteGitLabIntegrationParams{
		ID:          idUUID,
		WorkspaceID: wsUUID,
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "GitLab integration not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to remove GitLab integration")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// gitlabWebhookURL builds the absolute webhook URL a user pastes into
// GitLab's project Settings → Webhooks page. Derives scheme/host from the
// inbound request rather than a hardcoded base — this handler has no
// equivalent of GitHub App's fixed callback URL to anchor on, and the
// workspace could be served from any configured host. integrationID is a
// routing key, not a secret (see the package doc comment) — it is fine for
// it to appear in the URL, unlike the webhook_secret.
func gitlabWebhookURL(r *http.Request, integrationID string) string {
	scheme := "https"
	if r.TLS == nil && r.Header.Get("X-Forwarded-Proto") != "https" {
		scheme = "http"
	}
	return fmt.Sprintf("%s://%s/api/webhooks/gitlab/%s", scheme, r.Host, integrationID)
}

// verifyGitLabSecret constant-time compares the X-Gitlab-Token header
// against the integration's registered secret. GitLab echoes back whatever
// is configured as the project webhook's "Secret token" field — operators
// are instructed to paste the secret shown at registration time into that
// field.
func verifyGitLabSecret(header http.Header, secret string) bool {
	got := header.Get("X-Gitlab-Token")
	return subtle.ConstantTimeCompare([]byte(got), []byte(secret)) == 1
}

// HandleGitLabWebhook is the inbound merge-request webhook endpoint, mounted
// at POST /api/webhooks/gitlab/{integrationId}. The path segment is a
// routing key (looked up via GetGitLabIntegrationByID); the actual
// credential is the X-Gitlab-Token header, verified against the
// integration's webhook_secret.
func (h *Handler) HandleGitLabWebhook(w http.ResponseWriter, r *http.Request) {
	integrationID := chi.URLParam(r, "integrationId")
	if integrationID == "" {
		writeError(w, http.StatusUnauthorized, "missing integration id")
		return
	}
	idUUID, err := util.ParseUUID(integrationID)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "invalid integration id")
		return
	}
	integ, err := h.Queries.GetGitLabIntegrationByID(r.Context(), idUUID)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			slog.Warn("gitlab: lookup integration failed", "err", err)
		}
		// Same response whether the id is malformed or simply unknown — no
		// information about which case occurred.
		writeError(w, http.StatusUnauthorized, "invalid integration id")
		return
	}
	if !verifyGitLabSecret(r.Header, integ.WebhookSecret) {
		writeError(w, http.StatusUnauthorized, "invalid webhook secret")
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
		var p glMergeRequestPayload
		if err := json.Unmarshal(body, &p); err != nil {
			slog.Warn("gitlab: bad merge_request payload", "err", err)
			w.WriteHeader(http.StatusAccepted)
			return
		}
		// The secret alone proves the request knows this integration's
		// credential; this additionally proves the payload is actually
		// describing the ONE project this integration was registered for,
		// not a different project whose admin (accidentally or not) pasted
		// this integration's URL/secret into their own project's webhook
		// settings. Unlike the token/secret check above, this happens after
		// auth already succeeded, so a specific 400 here is an operator-
		// facing misconfiguration signal, not information leaked to an
		// unauthenticated attacker.
		if p.Project.ID != integ.GitlabProjectID {
			slog.Warn("gitlab: payload project id does not match registered integration",
				"integration_id", integrationID, "registered_project_id", integ.GitlabProjectID, "payload_project_id", p.Project.ID)
			writeError(w, http.StatusBadRequest, "webhook is configured for a different GitLab project")
			return
		}
		h.handleMergeRequestEvent(r.Context(), integ, p)
	default:
		// Acknowledge every event so GitLab doesn't mark the endpoint
		// failing, but ignore types we don't model (push, note, pipeline,
		// etc. — those are handled by the separate Octo-notification
		// webhook path, not this issue-linking one).
	}
	w.WriteHeader(http.StatusAccepted)
}

// glMergeRequestPayload models only the fields this handler consumes from
// GitLab's "Merge Request Hook" payload. Field presence verified against
// GitLab's documented webhook payload reference (docs.gitlab.com/user/
// project/integrations/webhook_events/) — created_at/updated_at/merged_at/
// action/draft (draft replaces the deprecated work_in_progress) and
// project.id are all real fields GitLab actually sends, not assumed.
type glMergeRequestPayload struct {
	ObjectAttributes struct {
		IID                 int32  `json:"iid"`
		Title               string `json:"title"`
		Description         string `json:"description"`
		State               string `json:"state"`
		Action              string `json:"action"`
		URL                 string `json:"url"`
		SourceBranch        string `json:"source_branch"`
		Draft               bool   `json:"draft"`
		MergeStatus         string `json:"merge_status"`
		DetailedMergeStatus string `json:"detailed_merge_status"`
		MergeCommitSha      string `json:"merge_commit_sha"`
		CreatedAt           string `json:"created_at"`
		UpdatedAt           string `json:"updated_at"`
		// MergedAt is absent/empty until the MR is actually merged.
		MergedAt string `json:"merged_at"`
	} `json:"object_attributes"`
	Project struct {
		ID                int64  `json:"id"`
		PathWithNamespace string `json:"path_with_namespace"`
	} `json:"project"`
	User struct {
		Username string `json:"username"`
	} `json:"user"`
}

// gitlabTimeLayouts are the wire formats GitLab has used for
// object_attributes timestamps across versions: modern ISO 8601, and the
// legacy space-separated "<date> <time> <ZONE>" format shown in GitLab's own
// documented payload example. Both are tried; an unrecognized format warns
// and falls back rather than failing the whole webhook.
var gitlabTimeLayouts = []string{
	time.RFC3339,
	"2006-01-02 15:04:05 MST",
}

func parseGitLabTime(s string) pgtype.Timestamptz {
	if s == "" {
		return pgtype.Timestamptz{}
	}
	for _, layout := range gitlabTimeLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return pgtype.Timestamptz{Time: t.UTC(), Valid: true}
		}
	}
	slog.Warn("gitlab: unrecognized timestamp format", "value", s)
	return pgtype.Timestamptz{}
}

// handleMergeRequestEvent upserts the MR mirror row, then runs the same
// auto-link + auto-close-on-merge logic as GitHub's handlePullRequestEvent
// (github.go's pull_request handling), reusing extractIdentifiers/
// extractClosingIdentifiers/lookupIssueByIdentifier/LinkIssueToPullRequest/
// advanceIssueToDone verbatim. GitLab MR webhooks don't include per-commit
// diff stats (additions/deletions/changed_files), so those fields are left
// at their zero value — the frontend already treats total==0 as "unknown"
// and hides the stats row (see pull-request-list.tsx), so this degrades
// gracefully rather than needing a GitLab-specific UI path. The caller has
// already validated p.Project.ID against integ.GitlabProjectID.
func (h *Handler) handleMergeRequestEvent(ctx context.Context, integ db.GitlabIntegration, p glMergeRequestPayload) {
	workspaceID := integ.WorkspaceID
	state := deriveMRState(p.ObjectAttributes.State, p.ObjectAttributes.Draft)
	mergeable := deriveMRMergeableState(p.ObjectAttributes.DetailedMergeStatus, p.ObjectAttributes.MergeStatus)

	pathParts := strings.Split(p.Project.PathWithNamespace, "/")
	repoOwner, repoName := p.Project.PathWithNamespace, ""
	if n := len(pathParts); n >= 2 {
		repoOwner = strings.Join(pathParts[:n-1], "/")
		repoName = pathParts[n-1]
	}

	createdAt := parseGitLabTime(p.ObjectAttributes.CreatedAt)
	if !createdAt.Valid {
		createdAt = pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true}
	}
	updatedAt := parseGitLabTime(p.ObjectAttributes.UpdatedAt)
	if !updatedAt.Valid {
		updatedAt = pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true}
	}
	mergedAt := parseGitLabTime(p.ObjectAttributes.MergedAt)

	pr, err := h.Queries.UpsertGitLabMergeRequest(ctx, db.UpsertGitLabMergeRequestParams{
		WorkspaceID:    workspaceID,
		ProviderHost:   integ.GitlabHost,
		RepoOwner:      repoOwner,
		RepoName:       repoName,
		PrNumber:       p.ObjectAttributes.IID,
		Title:          p.ObjectAttributes.Title,
		State:          state,
		HtmlUrl:        p.ObjectAttributes.URL,
		PrCreatedAt:    createdAt,
		PrUpdatedAt:    updatedAt,
		HeadSha:        p.ObjectAttributes.MergeCommitSha,
		Branch:         strToText(p.ObjectAttributes.SourceBranch),
		AuthorLogin:    strToText(p.User.Username),
		MergedAt:       mergedAt,
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
	// covered" section). Registering the webhook at all (creating a
	// gitlab_integration row for a project) is treated as consent to
	// auto-link for that project; a workspace that wants MR mirroring
	// without auto-link would need to delete the integration entirely, same
	// tradeoff GitHub's flag was introduced (RFC MUL-2414) specifically to
	// avoid.
	linkedIssueIDs := make([]string, 0)
	idents := extractIdentifiers(p.ObjectAttributes.Title, p.ObjectAttributes.Description, p.ObjectAttributes.SourceBranch)
	closingIdents := map[string]struct{}{}
	for _, c := range extractClosingIdentifiers(p.ObjectAttributes.Title, p.ObjectAttributes.Description) {
		closingIdents[c] = struct{}{}
	}
	// close_intent should follow the MR title/description while the MR is
	// still editable before its terminal close/merge event. Once GitLab has
	// delivered a terminal action, later update events must not rewrite the
	// merge-time close decision — mirrors GitHub's preserveCloseIntent.
	preserveCloseIntent := deriveMRPreserveCloseIntent(p.ObjectAttributes.Action, state)
	prefix := h.getIssuePrefix(ctx, workspaceID)
	reevalIssues := make([]db.Issue, 0, len(idents))
	for _, id := range idents {
		issue, ok := h.lookupIssueByIdentifier(ctx, workspaceID, prefix, id)
		if !ok {
			continue
		}
		_, declared := closingIdents[id]
		closeIntent := declared && !preserveCloseIntent
		if err := h.Queries.LinkIssueToPullRequest(ctx, db.LinkIssueToPullRequestParams{
			IssueID:             issue.ID,
			PullRequestID:       pr.ID,
			CloseIntent:         closeIntent,
			PreserveCloseIntent: preserveCloseIntent,
			LinkedByType:        strToText("system"),
			LinkedByID:          pgtype.UUID{},
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
func deriveMRState(state string, draft bool) string {
	switch state {
	case "merged":
		return "merged"
	case "closed", "locked":
		return "closed"
	default: // "opened", "reopened", or any future value GitLab adds
		if draft {
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

// deriveMRPreserveCloseIntent mirrors GitHub's preserveCloseIntent
// (handlePullRequestEvent): while THIS webhook delivery is itself the
// terminal event (the MR just closed or just merged), close_intent should
// be computed fresh from the current title/description — that is the
// authoritative moment to lock in the decision. Once a terminal event has
// already been delivered, a LATER non-terminal update (e.g. a label change
// on an already-merged MR) must not rewrite that locked-in decision.
// GitLab models "closed without merging" and "merged" as two distinct
// action values — "close" and "merge" — unlike GitHub, which reports both
// under the single action "closed" (with reference to a merged flag
// alongside it); both are terminal here.
func deriveMRPreserveCloseIntent(action, state string) bool {
	isTerminalAction := action == "close" || action == "merge"
	return !isTerminalAction && (state == "merged" || state == "closed")
}
