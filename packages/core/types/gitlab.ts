// GitLabIntegrationResponse mirrors the server's GitLabIntegrationResponse
// (server/internal/handler/gitlab.go). Unlike GitHub's installation model,
// there's only ever one integration per workspace and no management handle
// beyond the token itself — see GitHubMark's sibling GitLabMark doc comment
// for the settings-tab side of this.
export interface GitLabIntegrationResponse {
  workspace_id: string;
  configured: boolean;
  /** Only present in the create/rotate response, the moment the plaintext
   * token is known — same "shown once" pattern as webhook subscriptions'
   * signing secret. Absent on subsequent GET calls. */
  webhook_url?: string;
  created_at?: string;
}
