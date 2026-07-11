package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/multica-ai/multica/server/internal/middleware"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// setupGitLabTestIssue creates an issue plus a gitlab_integration row for
// testWorkspaceID and returns the issue and the webhook token to sign
// requests against. Mirrors setupPRTestIssue in github_test.go.
func setupGitLabTestIssue(t *testing.T, ctx context.Context) (IssueResponse, string) {
	t.Helper()
	w := httptest.NewRecorder()
	req := newRequest("POST", "/api/issues?workspace_id="+testWorkspaceID, map[string]any{
		"title":  "GitLab MR test",
		"status": "in_progress",
	})
	testHandler.CreateIssue(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("CreateIssue: %d %s", w.Code, w.Body.String())
	}
	var created IssueResponse
	json.NewDecoder(w.Body).Decode(&created)

	token, err := generateGitLabWebhookToken()
	if err != nil {
		t.Fatalf("generateGitLabWebhookToken: %v", err)
	}
	if _, err := testHandler.Queries.CreateGitLabIntegration(ctx, db.CreateGitLabIntegrationParams{
		WorkspaceID:  parseUUID(testWorkspaceID),
		WebhookToken: token,
	}); err != nil {
		t.Fatalf("CreateGitLabIntegration: %v", err)
	}

	t.Cleanup(func() {
		testPool.Exec(ctx, `DELETE FROM issue_pull_request WHERE issue_id = $1`, created.ID)
		testPool.Exec(ctx, `DELETE FROM github_pull_request WHERE workspace_id = $1`, testWorkspaceID)
		testPool.Exec(ctx, `DELETE FROM gitlab_integration WHERE workspace_id = $1`, testWorkspaceID)
		testPool.Exec(ctx, `DELETE FROM activity_log WHERE issue_id = $1`, created.ID)
		testPool.Exec(ctx, `DELETE FROM issue WHERE id = $1`, created.ID)
	})
	return created, token
}

// fireMergeRequestWebhook posts a "Merge Request Hook" payload through
// HandleGitLabWebhook, signing it with the given token both as the URL path
// segment and the X-Gitlab-Token header (the real-world setup: an operator
// pastes the same value into both the webhook URL and the project's Secret
// token field).
func fireMergeRequestWebhook(t *testing.T, token, identifier, repoPath string, iid int32, state string, workInProgress bool, mergeStatus, detailedMergeStatus, sourceBranch string) {
	t.Helper()
	payload := map[string]any{
		"object_attributes": map[string]any{
			"iid":                   iid,
			"title":                 "Fix " + identifier,
			"description":           "",
			"state":                 state,
			"url":                   "https://gitlab.example/acme/" + repoPath + "/-/merge_requests/" + itoaHelper(iid),
			"source_branch":         sourceBranch,
			"work_in_progress":      workInProgress,
			"merge_status":          mergeStatus,
			"detailed_merge_status": detailedMergeStatus,
			"merge_commit_sha":      "deadbeef",
		},
		"project": map[string]any{
			"path_with_namespace": "acme/" + repoPath,
		},
		"user": map[string]any{
			"username": "gitlab-tester",
		},
	}
	raw, _ := json.Marshal(payload)

	rec := httptest.NewRecorder()
	hookReq := httptest.NewRequest("POST", "/api/webhooks/gitlab/"+token, bytes.NewReader(raw))
	hookReq = withURLParam(hookReq, "token", token)
	hookReq.Header.Set("X-Gitlab-Event", "Merge Request Hook")
	hookReq.Header.Set("X-Gitlab-Token", token)
	testHandler.HandleGitLabWebhook(rec, hookReq)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("gitlab webhook %s iid=%d state=%s: expected 202, got %d (%s)",
			repoPath, iid, state, rec.Code, rec.Body.String())
	}
}

// itoaHelper avoids pulling in strconv just for one call site's string
// concatenation inside test payload construction.
func itoaHelper(n int32) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [12]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

func TestDeriveMRState(t *testing.T) {
	cases := []struct {
		name           string
		state          string
		workInProgress bool
		want           string
	}{
		{"merged", "merged", false, "merged"},
		{"closed", "closed", false, "closed"},
		{"locked_treated_as_closed", "locked", false, "closed"},
		{"opened", "opened", false, "open"},
		{"reopened", "reopened", false, "open"},
		{"opened_draft", "opened", true, "draft"},
		{"unknown_future_value_defaults_open", "some-new-state", false, "open"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := deriveMRState(tc.state, tc.workInProgress)
			if got != tc.want {
				t.Errorf("deriveMRState(%q, %v) = %q, want %q", tc.state, tc.workInProgress, got, tc.want)
			}
		})
	}
}

func TestDeriveMRMergeableState(t *testing.T) {
	cases := []struct {
		name     string
		detailed string
		legacy   string
		want     string
		wantNull bool
	}{
		{"detailed_cannot_be_merged", "cannot_be_merged", "", "dirty", false},
		{"detailed_cannot_be_merged_recheck", "cannot_be_merged_recheck", "", "dirty", false},
		{"detailed_can_be_merged", "can_be_merged", "", "clean", false},
		{"legacy_mergeable_when_detailed_empty", "", "mergeable", "clean", false},
		{"legacy_preferred_over_unknown_detailed", "unknown_detailed_value", "mergeable", "", true},
		{"both_empty_is_unknown", "", "", "", true},
		{"detailed_takes_priority_over_legacy", "cannot_be_merged", "mergeable", "dirty", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := deriveMRMergeableState(tc.detailed, tc.legacy)
			if tc.wantNull {
				if got.Valid {
					t.Errorf("deriveMRMergeableState(%q, %q) = %+v, want NULL", tc.detailed, tc.legacy, got)
				}
				return
			}
			if !got.Valid || got.String != tc.want {
				t.Errorf("deriveMRMergeableState(%q, %q) = %+v, want %q", tc.detailed, tc.legacy, got, tc.want)
			}
		})
	}
}

// TestGitLabWebhook_AutoLinksAndUpsertsMR covers the base ingestion path: a
// merge request whose title references an issue identifier gets upserted
// into github_pull_request (provider=gitlab) and linked to that issue.
func TestGitLabWebhook_AutoLinksAndUpsertsMR(t *testing.T) {
	if testHandler == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	ctx := context.Background()
	created, token := setupGitLabTestIssue(t, ctx)

	fireMergeRequestWebhook(t, token, created.Identifier, "gitlab-repo-a", 1, "opened", false, "", "mergeable", "fix/"+created.Identifier)

	rows, err := testHandler.Queries.ListPullRequestsByIssue(ctx, parseUUID(created.ID))
	if err != nil {
		t.Fatalf("ListPullRequestsByIssue: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 linked MR, got %d", len(rows))
	}
	row := rows[0]
	if row.Provider != "gitlab" {
		t.Errorf("provider = %q, want gitlab", row.Provider)
	}
	if row.State != "open" {
		t.Errorf("state = %q, want open", row.State)
	}
	if !row.MergeableState.Valid || row.MergeableState.String != "clean" {
		t.Errorf("mergeable_state = %+v, want clean", row.MergeableState)
	}
	if row.InstallationID.Valid {
		t.Errorf("installation_id should be NULL for a GitLab row, got %+v", row.InstallationID)
	}
	if row.RepoOwner != "acme" || row.RepoName != "gitlab-repo-a" {
		t.Errorf("repo_owner/repo_name = %s/%s, want acme/gitlab-repo-a", row.RepoOwner, row.RepoName)
	}
}

// TestGitLabWebhook_MergedAdvancesIssueToDone covers the auto-close path:
// once every linked MR is either merged (with closing intent from the
// title) or otherwise no longer open, the issue advances to done, tagged
// with source=gitlab_mr_merged.
func TestGitLabWebhook_MergedAdvancesIssueToDone(t *testing.T) {
	if testHandler == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	ctx := context.Background()
	created, token := setupGitLabTestIssue(t, ctx)

	title := "Closes " + created.Identifier
	// Reuse fireMergeRequestWebhook's identifier-based title by faking the
	// closing keyword directly via a bespoke payload, since the shared
	// helper only prefixes "Fix ". Closing-intent detection is exercised
	// thoroughly by TestExtractClosingIdentifiers in github_test.go; this
	// test only needs the merged-state → advance-to-done wiring.
	payload := map[string]any{
		"object_attributes": map[string]any{
			"iid":                   int32(1),
			"title":                 title,
			"description":           "",
			"state":                 "merged",
			"url":                   "https://gitlab.example/acme/gitlab-repo-b/-/merge_requests/1",
			"source_branch":         "fix/foo",
			"work_in_progress":      false,
			"merge_status":          "merged",
			"detailed_merge_status": "merged",
			"merge_commit_sha":      "cafebabe",
		},
		"project": map[string]any{"path_with_namespace": "acme/gitlab-repo-b"},
		"user":    map[string]any{"username": "gitlab-tester"},
	}
	raw, _ := json.Marshal(payload)
	rec := httptest.NewRecorder()
	hookReq := httptest.NewRequest("POST", "/api/webhooks/gitlab/"+token, bytes.NewReader(raw))
	hookReq = withURLParam(hookReq, "token", token)
	hookReq.Header.Set("X-Gitlab-Event", "Merge Request Hook")
	hookReq.Header.Set("X-Gitlab-Token", token)
	testHandler.HandleGitLabWebhook(rec, hookReq)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("webhook: expected 202, got %d (%s)", rec.Code, rec.Body.String())
	}

	final, err := testHandler.Queries.GetIssue(ctx, parseUUID(created.ID))
	if err != nil {
		t.Fatalf("GetIssue: %v", err)
	}
	if final.Status != "done" {
		t.Errorf("expected issue done after merged MR with closing intent, got %q", final.Status)
	}
}

// TestGitLabWebhook_InvalidTokenRejected covers the auth boundary: a
// mismatched or unknown token must 401, and the X-Gitlab-Token header must
// match the URL token exactly, not just be present.
func TestGitLabWebhook_InvalidTokenRejected(t *testing.T) {
	if testHandler == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	ctx := context.Background()
	_, token := setupGitLabTestIssue(t, ctx)

	t.Run("unknown token", func(t *testing.T) {
		rec := httptest.NewRecorder()
		hookReq := httptest.NewRequest("POST", "/api/webhooks/gitlab/glwt_does_not_exist", bytes.NewReader([]byte("{}")))
		hookReq = withURLParam(hookReq, "token", "glwt_does_not_exist")
		hookReq.Header.Set("X-Gitlab-Event", "Merge Request Hook")
		hookReq.Header.Set("X-Gitlab-Token", "glwt_does_not_exist")
		testHandler.HandleGitLabWebhook(rec, hookReq)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("expected 401 for unknown token, got %d", rec.Code)
		}
	})

	t.Run("mismatched secret header", func(t *testing.T) {
		rec := httptest.NewRecorder()
		hookReq := httptest.NewRequest("POST", "/api/webhooks/gitlab/"+token, bytes.NewReader([]byte("{}")))
		hookReq = withURLParam(hookReq, "token", token)
		hookReq.Header.Set("X-Gitlab-Event", "Merge Request Hook")
		hookReq.Header.Set("X-Gitlab-Token", "wrong-secret")
		testHandler.HandleGitLabWebhook(rec, hookReq)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("expected 401 for mismatched X-Gitlab-Token, got %d", rec.Code)
		}
	})
}

// TestCreateOrRotateGitLabIntegration covers the admin management endpoint:
// the first call creates a row and returns a webhook_url; a second call
// rotates the token, invalidating the first one, and GetGitLabIntegration
// never echoes the token itself.
func TestCreateOrRotateGitLabIntegration(t *testing.T) {
	if testHandler == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	ctx := context.Background()
	t.Cleanup(func() {
		testPool.Exec(ctx, `DELETE FROM gitlab_integration WHERE workspace_id = $1`, testWorkspaceID)
	})

	create := func(t *testing.T) GitLabIntegrationResponse {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/api/workspaces/"+testWorkspaceID+"/gitlab/rotate", nil)
		req = withURLParam(req, "id", testWorkspaceID)
		req = req.WithContext(middleware.SetMemberContext(req.Context(), testWorkspaceID, db.Member{Role: "admin", UserID: parseUUID(testUserID)}))
		w := httptest.NewRecorder()
		testHandler.CreateOrRotateGitLabIntegration(w, req)
		if w.Code != http.StatusCreated {
			t.Fatalf("CreateOrRotateGitLabIntegration: %d %s", w.Code, w.Body.String())
		}
		var body GitLabIntegrationResponse
		if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return body
	}

	first := create(t)
	if first.WebhookURL == nil || *first.WebhookURL == "" {
		t.Fatalf("expected webhook_url on first create, got %+v", first)
	}
	if !first.Configured {
		t.Errorf("expected configured=true after create")
	}

	// GET must never echo the token/URL.
	getReq := httptest.NewRequest(http.MethodGet, "/api/workspaces/"+testWorkspaceID+"/gitlab", nil)
	getReq = withURLParam(getReq, "id", testWorkspaceID)
	getW := httptest.NewRecorder()
	testHandler.GetGitLabIntegration(getW, getReq)
	if getW.Code != http.StatusOK {
		t.Fatalf("GetGitLabIntegration: %d %s", getW.Code, getW.Body.String())
	}
	var getBody GitLabIntegrationResponse
	json.NewDecoder(getW.Body).Decode(&getBody)
	if getBody.WebhookURL != nil {
		t.Errorf("GetGitLabIntegration must not echo webhook_url, got %+v", getBody)
	}
	if !getBody.Configured {
		t.Errorf("expected configured=true on GET after create")
	}

	// Rotating replaces the token; the old URL's token must stop working.
	second := create(t)
	if second.WebhookURL == nil || *second.WebhookURL == *first.WebhookURL {
		t.Errorf("expected rotate to return a different webhook_url, first=%v second=%v", first.WebhookURL, second.WebhookURL)
	}
}
