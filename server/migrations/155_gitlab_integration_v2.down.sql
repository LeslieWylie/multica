-- Reverting the identity-key widening requires deleting non-GitHub rows
-- first so the old GitHub-only NOT NULL / narrower-key constraints can be
-- restored — same destructive-down tradeoff the original migration 128
-- documented for GitLab-sourced PR mirrors.
DELETE FROM github_pull_request WHERE installation_id IS NULL;

ALTER TABLE github_pull_request DROP CONSTRAINT github_pull_request_identity_key;
ALTER TABLE github_pull_request
    ADD CONSTRAINT github_pull_request_workspace_id_repo_owner_repo_name_pr_number_key
    UNIQUE (workspace_id, repo_owner, repo_name, pr_number);

ALTER TABLE github_pull_request
    ALTER COLUMN installation_id SET NOT NULL,
    DROP COLUMN IF EXISTS provider,
    DROP COLUMN IF EXISTS provider_host;

DROP TABLE IF EXISTS gitlab_integration;
