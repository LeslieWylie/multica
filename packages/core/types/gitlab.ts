// GitLabIntegrationResponse mirrors the server's GitLabIntegrationResponse
// (server/internal/handler/gitlab.go). Per-project registration — unlike
// GitHub's App installation model, GitLab has no equivalent "app" concept,
// so a workspace registers each GitLab project it wants MR sync for
// individually (mirrors how Octo/Lark already support multiple bots per
// workspace rather than a single shared credential).
export interface GitLabIntegrationResponse {
  id: string;
  workspace_id: string;
  gitlab_host: string;
  gitlab_project_id: number;
  gitlab_project_path: string;
  created_at: string;
  /** Only present in the create response, the moment the plaintext secret
   * is known — same "shown once" pattern as webhook subscriptions' signing
   * secret. Absent on subsequent list calls. */
  webhook_url?: string;
  webhook_secret?: string;
}

export interface ListGitLabIntegrationsResponse {
  integrations: GitLabIntegrationResponse[];
}

export interface CreateGitLabIntegrationRequest {
  gitlab_host: string;
  gitlab_project_id: number;
  gitlab_project_path: string;
}
