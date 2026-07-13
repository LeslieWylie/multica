-- GitLab merge-request integration, v2: per-project (not per-workspace)
-- registration. See the PR description for the design rationale — a single
-- workspace-wide webhook token trusted any payload claiming any project
-- path, letting a holder of that token forge events for a project it was
-- never issued for. Each row here is scoped to exactly one GitLab project on
-- one GitLab instance; the webhook handler validates the inbound payload's
-- project.id against gitlab_project_id before trusting it, not just the
-- URL/secret. Mirrors how Octo/Lark already support multiple bots per
-- workspace rather than a single shared credential.
CREATE TABLE gitlab_integration (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id        UUID NOT NULL REFERENCES workspace(id) ON DELETE CASCADE,
    -- Host of the GitLab instance (e.g. "gitlab.com" or a self-hosted
    -- domain) — disambiguates two different instances that happen to have a
    -- project at the same path within one workspace.
    gitlab_host         TEXT NOT NULL,
    -- GitLab's numeric project id — stable identity used to validate inbound
    -- webhook payloads. path_with_namespace can be renamed; the numeric id
    -- cannot.
    gitlab_project_id   BIGINT NOT NULL,
    -- path_with_namespace at registration time — display only, not used for
    -- trust decisions (see gitlab_project_id above).
    gitlab_project_path TEXT NOT NULL,
    -- Compared against the X-Gitlab-Token header. Cleartext at rest,
    -- matching webhook_subscription.secret's documented tradeoff (migration
    -- 121) — no hashed-secrets-at-rest infrastructure to layer on yet.
    webhook_secret      TEXT NOT NULL,
    created_by_id       UUID REFERENCES "user"(id) ON DELETE SET NULL,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (workspace_id, gitlab_host, gitlab_project_id)
);
CREATE INDEX idx_gitlab_integration_workspace ON gitlab_integration(workspace_id);

-- provider disambiguates GitHub vs GitLab rows that happen to share a repo
-- path string within one workspace (e.g. both have "acme/backend").
-- provider_host further disambiguates two different GitLab instances (or a
-- self-hosted instance vs gitlab.com) sharing a project path. Both default
-- to the GitHub-shaped value ('github' / '') so every existing row is
-- unaffected. installation_id becomes nullable — GitLab rows never have a
-- GitHub App installation.
ALTER TABLE github_pull_request
    ADD COLUMN provider TEXT NOT NULL DEFAULT 'github',
    ADD COLUMN provider_host TEXT NOT NULL DEFAULT '',
    ALTER COLUMN installation_id DROP NOT NULL;

-- Widen the PR identity key so GitHub and GitLab rows (or two GitLab
-- instances) sharing a repo_owner/repo_name string in the same workspace no
-- longer collide and silently overwrite each other via ON CONFLICT DO
-- UPDATE. The original inline UNIQUE(workspace_id, repo_owner, repo_name,
-- pr_number) from migration 079 got an auto-generated name that Postgres
-- silently truncates to 63 bytes (NAMEDATALEN) — long enough here to drop
-- the "_key" suffix entirely, so hardcoding the truncated name would be
-- fragile. Look it up by its actual columns instead.
DO $$
DECLARE
    cname text;
BEGIN
    SELECT c.conname INTO cname
    FROM pg_constraint c
    WHERE c.conrelid = 'github_pull_request'::regclass
      AND c.contype = 'u'
      AND (
          SELECT array_agg(a.attname ORDER BY a.attname)
          FROM unnest(c.conkey) AS k(attnum)
          JOIN pg_attribute a ON a.attrelid = c.conrelid AND a.attnum = k.attnum
      ) = ARRAY['pr_number', 'repo_name', 'repo_owner', 'workspace_id']::name[]
    LIMIT 1;
    IF cname IS NOT NULL THEN
        EXECUTE format('ALTER TABLE github_pull_request DROP CONSTRAINT %I', cname);
    END IF;
END $$;

ALTER TABLE github_pull_request
    ADD CONSTRAINT github_pull_request_identity_key
    UNIQUE (workspace_id, provider, provider_host, repo_owner, repo_name, pr_number);
