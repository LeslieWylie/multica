-- GitLab merge-request integration: mirrors GitHub's pull-request sync
-- (see github.sql UpsertGitHubPullRequest / ListPullRequestsByIssue) using
-- the same generalized github_pull_request table (migration 155). Kept in a
-- separate file — not appended to github.sql — because the two providers'
-- upsert/lookup queries never share a WHERE clause and this keeps each
-- provider's webhook-ingestion queries independently reviewable.

-- =====================
-- GitLab integration (per-project webhook registration — see migration 155
-- for why this is per-project rather than a single workspace-wide token)
-- =====================

-- name: CreateGitLabIntegration :one
-- ON CONFLICT re-registering the same (workspace, host, project) rotates its
-- secret rather than erroring — matches CreateWebhookSubscription's "PATCH
-- via re-create" ergonomics for a credential the operator may need to
-- reissue after a leak, without a separate rotate endpoint.
INSERT INTO gitlab_integration (
    workspace_id, gitlab_host, gitlab_project_id, gitlab_project_path,
    webhook_secret, created_by_id
) VALUES (
    $1, $2, $3, $4, $5, sqlc.narg('created_by_id')
)
ON CONFLICT (workspace_id, gitlab_host, gitlab_project_id) DO UPDATE SET
    gitlab_project_path = EXCLUDED.gitlab_project_path,
    webhook_secret = EXCLUDED.webhook_secret,
    updated_at = now()
RETURNING *;

-- name: GetGitLabIntegrationByID :one
-- The webhook route's lookup — keyed by the URL's routing id, which is NOT
-- itself a secret (see migration 155 doc comment); the actual credential is
-- webhook_secret, verified separately against X-Gitlab-Token.
SELECT * FROM gitlab_integration WHERE id = $1;

-- name: ListGitLabIntegrationsByWorkspace :many
SELECT * FROM gitlab_integration WHERE workspace_id = $1 ORDER BY created_at ASC;

-- name: DeleteGitLabIntegration :one
-- Scoped by (id, workspace_id) like DeleteGitHubInstallation, so a caller
-- can never delete another workspace's integration by guessing an id.
DELETE FROM gitlab_integration WHERE id = $1 AND workspace_id = $2
RETURNING id, workspace_id;

-- =====================
-- GitLab merge request mirror (github_pull_request, provider='gitlab')
-- =====================

-- name: UpsertGitLabMergeRequest :one
-- installation_id is always NULL for GitLab rows (no installation concept —
-- see migration 155). provider_host carries the registered integration's
-- gitlab_host, widening the identity key alongside provider so two GitLab
-- instances (or GitHub vs GitLab) sharing a repo_owner/repo_name string in
-- one workspace can't overwrite each other. mergeable_state has simpler
-- two-state semantics than GitHub's (no separate "clear on state-changing
-- action" pass): GitLab's merge_status/detailed_merge_status is reported
-- fresh on every webhook delivery, so the incoming value (possibly NULL when
-- GitLab hasn't computed it yet) always overwrites — no "preserve existing
-- column when absent" case like GitHub's metadata-only events.
INSERT INTO github_pull_request (
    workspace_id, installation_id, provider, provider_host, repo_owner, repo_name, pr_number,
    title, state, html_url, branch, author_login, author_avatar_url,
    merged_at, closed_at, pr_created_at, pr_updated_at,
    head_sha, mergeable_state
) VALUES (
    $1, NULL, 'gitlab', $2, $3, $4, $5,
    $6, $7, $8, sqlc.narg('branch'), sqlc.narg('author_login'), sqlc.narg('author_avatar_url'),
    sqlc.narg('merged_at'), sqlc.narg('closed_at'), $9, $10,
    $11, sqlc.narg('mergeable_state')
)
ON CONFLICT (workspace_id, provider, provider_host, repo_owner, repo_name, pr_number) DO UPDATE SET
    title = EXCLUDED.title,
    state = EXCLUDED.state,
    html_url = EXCLUDED.html_url,
    branch = EXCLUDED.branch,
    author_login = EXCLUDED.author_login,
    author_avatar_url = EXCLUDED.author_avatar_url,
    merged_at = EXCLUDED.merged_at,
    closed_at = EXCLUDED.closed_at,
    pr_updated_at = EXCLUDED.pr_updated_at,
    head_sha = EXCLUDED.head_sha,
    mergeable_state = EXCLUDED.mergeable_state,
    updated_at = now()
RETURNING *;
