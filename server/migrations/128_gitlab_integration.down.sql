-- Reverting the NOT NULL constraint on installation_id requires no GitLab
-- rows (installation_id IS NULL) to remain — delete them first so the
-- ALTER doesn't fail on rows that predate GitHub-only enforcement. This is
-- a destructive down migration for any GitLab-sourced PR mirrors, which is
-- the expected tradeoff of rolling back a schema generalization: the down
-- path returns the table to a GitHub-only shape, so GitHub-only data is
-- what it can hold afterward.
DELETE FROM github_pull_request WHERE installation_id IS NULL;
ALTER TABLE github_pull_request ALTER COLUMN installation_id SET NOT NULL;
ALTER TABLE github_pull_request DROP COLUMN IF EXISTS provider;

DROP TABLE IF EXISTS gitlab_integration;
