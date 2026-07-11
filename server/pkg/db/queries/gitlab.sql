-- GitLab merge-request integration: mirrors GitHub's pull-request sync
-- (see github.sql UpsertGitHubPullRequest / ListPullRequestsByIssue) using
-- the same generalized github_pull_request table (migration 128). Kept in a
-- separate file — not appended to github.sql — because the two providers'
-- upsert/lookup queries never share a WHERE clause and this keeps each
-- provider's webhook-ingestion queries independently reviewable.

-- =====================
-- GitLab integration (per-workspace webhook token)
-- =====================

-- name: CreateGitLabIntegration :one
INSERT INTO gitlab_integration (workspace_id, webhook_token, created_by_id)
VALUES ($1, $2, sqlc.narg('created_by_id'))
ON CONFLICT (workspace_id) DO UPDATE SET
    webhook_token = EXCLUDED.webhook_token,
    updated_at = now()
RETURNING *;

-- name: GetGitLabIntegrationByToken :one
SELECT * FROM gitlab_integration WHERE webhook_token = $1;

-- name: GetGitLabIntegrationByWorkspace :one
SELECT * FROM gitlab_integration WHERE workspace_id = $1;

-- =====================
-- GitLab merge request mirror (github_pull_request, provider='gitlab')
-- =====================

-- name: UpsertGitLabMergeRequest :one
-- installation_id is always NULL for GitLab rows (no installation concept —
-- see migration 128). mergeable_state has simpler two-state semantics than
-- GitHub's (no separate "clear on state-changing action" pass): GitLab's
-- merge_status/detailed_merge_status is reported fresh on every webhook
-- delivery, so the incoming value (possibly NULL when GitLab hasn't
-- computed it yet) always overwrites — no "preserve existing column when
-- absent" case like GitHub's metadata-only events.
INSERT INTO github_pull_request (
    workspace_id, installation_id, provider, repo_owner, repo_name, pr_number,
    title, state, html_url, branch, author_login, author_avatar_url,
    merged_at, closed_at, pr_created_at, pr_updated_at,
    head_sha, mergeable_state
) VALUES (
    $1, NULL, 'gitlab', $2, $3, $4,
    $5, $6, $7, sqlc.narg('branch'), sqlc.narg('author_login'), sqlc.narg('author_avatar_url'),
    sqlc.narg('merged_at'), sqlc.narg('closed_at'), $8, $9,
    $10, sqlc.narg('mergeable_state')
)
ON CONFLICT (workspace_id, repo_owner, repo_name, pr_number) DO UPDATE SET
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
