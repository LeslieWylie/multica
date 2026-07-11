-- GitLab merge-request integration: per-workspace webhook token + generalizing
-- the existing pull-request mirror table to accept non-GitHub rows.
--
-- Unlike GitHub App installations (one app, many workspace-scoped
-- installations, GitHub-issued installation_id), GitLab has no equivalent
-- "app" concept — each project gets its own webhook configured with a shared
-- secret token. This table holds one webhook_token per workspace; the token
-- is both the auth credential and the workspace router (looked up by token
-- on inbound webhook, same pattern as autopilot webhook triggers'
-- webhook_token column).
CREATE TABLE gitlab_integration (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id  UUID NOT NULL REFERENCES workspace(id) ON DELETE CASCADE UNIQUE,
    webhook_token TEXT NOT NULL UNIQUE,
    created_by_id UUID REFERENCES "user"(id) ON DELETE SET NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Generalize github_pull_request to also hold GitLab merge-request mirrors.
-- installation_id is GitHub-App-specific (there is no GitLab equivalent), so
-- it must become nullable for GitLab rows. provider distinguishes the two;
-- existing rows default to 'github' so no backfill is required. The
-- (workspace_id, repo_owner, repo_name, pr_number) unique constraint is left
-- as-is: repo_owner/repo_name are populated from each provider's own
-- namespace/path segments, which do not collide across providers in
-- practice (different hosts produce different path strings), so adding
-- provider to the key would only protect against a scenario that can't
-- occur without ALSO having identical repo_owner/repo_name across GitHub
-- and GitLab for the same workspace.
ALTER TABLE github_pull_request ADD COLUMN provider TEXT NOT NULL DEFAULT 'github';
ALTER TABLE github_pull_request ALTER COLUMN installation_id DROP NOT NULL;
