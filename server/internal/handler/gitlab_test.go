package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/middleware"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// setupGitLabTestIssue creates an issue plus a per-project gitlab_integration
// row for testWorkspaceID and returns the issue, the integration, and the
// webhook secret to sign requests against. Mirrors setupPRTestIssue in
// github_test.go.
func setupGitLabTestIssue(t *testing.T, ctx context.Context) (IssueResponse, db.GitlabIntegration, string) {
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

	secret, err := generateGitLabWebhookSecret()
	if err != nil {
		t.Fatalf("generateGitLabWebhookSecret: %v", err)
	}
	integ, err := testHandler.Queries.CreateGitLabIntegration(ctx, db.CreateGitLabIntegrationParams{
		WorkspaceID:       parseUUID(testWorkspaceID),
		GitlabHost:        "gitlab.example",
		GitlabProjectID:   1001,
		GitlabProjectPath: "acme/gitlab-repo-a",
		WebhookSecret:     secret,
	})
	if err != nil {
		t.Fatalf("CreateGitLabIntegration: %v", err)
	}

	t.Cleanup(func() {
		testPool.Exec(ctx, `DELETE FROM issue_pull_request WHERE issue_id = $1`, created.ID)
		testPool.Exec(ctx, `DELETE FROM github_pull_request WHERE workspace_id = $1`, testWorkspaceID)
		testPool.Exec(ctx, `DELETE FROM gitlab_integration WHERE workspace_id = $1`, testWorkspaceID)
		testPool.Exec(ctx, `DELETE FROM activity_log WHERE issue_id = $1`, created.ID)
		testPool.Exec(ctx, `DELETE FROM issue WHERE id = $1`, created.ID)
	})
	return created, integ, secret
}

// mrPayloadOpts lets each test override only the fields it cares about;
// fireMergeRequestWebhook fills in reasonable defaults for the rest.
// mergeCommitSha and lastCommitID are deliberately separate fields (not one
// "the current sha" field) — GitLab only populates merge_commit_sha once
// the MR is actually merged (null on an open MR); last_commit.id is present
// on every state and is what head_sha is meant to track. Defaulting
// mergeCommitSha to "" here means a test that doesn't explicitly set it
// exercises the real "open MR" shape rather than accidentally masking the
// bug this was written to catch.
type mrPayloadOpts struct {
	projectID           int64
	projectPath         string
	iid                 int32
	title               string
	description         string
	state               string
	action              string
	draft               bool
	mergeStatus         string
	detailedMergeStatus string
	sourceBranch        string
	createdAt           string
	updatedAt           string
	mergedAt            string
	lastCommitID        string
	mergeCommitSha      string
}

// fireMergeRequestWebhook posts a "Merge Request Hook" payload through
// HandleGitLabWebhook, signing it with the given secret as the
// X-Gitlab-Token header (the real-world setup: an operator pastes the
// secret shown at registration time into the project's Secret token field).
func fireMergeRequestWebhook(t *testing.T, integrationID, secret string, opts mrPayloadOpts) {
	t.Helper()
	payload := map[string]any{
		"object_attributes": map[string]any{
			"iid":                   opts.iid,
			"title":                 opts.title,
			"description":           opts.description,
			"state":                 opts.state,
			"action":                opts.action,
			"url":                   "https://gitlab.example/" + opts.projectPath + "/-/merge_requests/1",
			"source_branch":         opts.sourceBranch,
			"draft":                 opts.draft,
			"merge_status":          opts.mergeStatus,
			"detailed_merge_status": opts.detailedMergeStatus,
			"merge_commit_sha":      opts.mergeCommitSha,
			"last_commit":           map[string]any{"id": opts.lastCommitID},
			"created_at":            opts.createdAt,
			"updated_at":            opts.updatedAt,
			"merged_at":             opts.mergedAt,
		},
		"project": map[string]any{
			"id":                  opts.projectID,
			"path_with_namespace": opts.projectPath,
		},
		"user": map[string]any{
			"username": "gitlab-tester",
		},
	}
	raw, _ := json.Marshal(payload)

	rec := httptest.NewRecorder()
	hookReq := httptest.NewRequest("POST", "/api/webhooks/gitlab/"+integrationID, bytes.NewReader(raw))
	hookReq = withURLParam(hookReq, "integrationId", integrationID)
	hookReq.Header.Set("X-Gitlab-Event", "Merge Request Hook")
	hookReq.Header.Set("X-Gitlab-Token", secret)
	testHandler.HandleGitLabWebhook(rec, hookReq)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("gitlab webhook %s iid=%d state=%s: expected 202, got %d (%s)",
			opts.projectPath, opts.iid, opts.state, rec.Code, rec.Body.String())
	}
}

func TestDeriveMRState(t *testing.T) {
	cases := []struct {
		name  string
		state string
		draft bool
		want  string
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
			got := deriveMRState(tc.state, tc.draft)
			if got != tc.want {
				t.Errorf("deriveMRState(%q, %v) = %q, want %q", tc.state, tc.draft, got, tc.want)
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

func TestDeriveMRPreserveCloseIntent(t *testing.T) {
	cases := []struct {
		name   string
		action string
		state  string
		want   bool
	}{
		{"merge_action_is_terminal_does_not_preserve", "merge", "merged", false},
		{"merged_but_action_not_yet_merge_still_preserves", "update", "merged", true},
		{"closed_via_close_action_does_not_preserve", "close", "closed", false},
		{"closed_state_but_different_action_preserves", "update", "closed", true},
		{"open_state_never_preserves", "update", "open", false},
		{"draft_state_never_preserves", "update", "draft", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := deriveMRPreserveCloseIntent(tc.action, tc.state)
			if got != tc.want {
				t.Errorf("deriveMRPreserveCloseIntent(%q, %q) = %v, want %v", tc.action, tc.state, got, tc.want)
			}
		})
	}
}

func TestParseGitLabTime(t *testing.T) {
	cases := []struct {
		name      string
		in        string
		wantValid bool
		wantYear  int
	}{
		{"empty_is_invalid", "", false, 0},
		{"rfc3339", "2026-01-16T05:56:22Z", true, 2026},
		{"gitlab_legacy_space_format", "2026-01-16 05:56:22 UTC", true, 2026},
		{"unrecognized_format_falls_back_invalid", "not-a-timestamp", false, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseGitLabTime(tc.in)
			if got.Valid != tc.wantValid {
				t.Fatalf("parseGitLabTime(%q).Valid = %v, want %v", tc.in, got.Valid, tc.wantValid)
			}
			if tc.wantValid && got.Time.Year() != tc.wantYear {
				t.Errorf("parseGitLabTime(%q).Time.Year() = %d, want %d", tc.in, got.Time.Year(), tc.wantYear)
			}
		})
	}
}

// TestGitLabWebhook_AutoLinksAndUpsertsMR covers the base ingestion path: a
// merge request whose title references an issue identifier gets upserted
// into github_pull_request (provider=gitlab, provider_host=the registered
// gitlab_host) and linked to that issue.
func TestGitLabWebhook_AutoLinksAndUpsertsMR(t *testing.T) {
	if testHandler == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	ctx := context.Background()
	created, integ, secret := setupGitLabTestIssue(t, ctx)

	fireMergeRequestWebhook(t, uuidToString(integ.ID), secret, mrPayloadOpts{
		projectID:           integ.GitlabProjectID,
		projectPath:         integ.GitlabProjectPath,
		iid:                 1,
		title:               "Fix " + created.Identifier,
		state:               "opened",
		action:              "open",
		sourceBranch:        "fix/" + created.Identifier,
		detailedMergeStatus: "mergeable",
		createdAt:           "2026-01-16 05:56:22 UTC",
		updatedAt:           "2026-01-16 05:56:22 UTC",
		// mergeCommitSha intentionally left empty — GitLab never populates
		// it on an open MR. lastCommitID is the source branch's current
		// HEAD, which is what head_sha below must come from.
		lastCommitID: "abc1112223334445556667778889990001112223",
	})

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
	if row.ProviderHost != integ.GitlabHost {
		t.Errorf("provider_host = %q, want %q", row.ProviderHost, integ.GitlabHost)
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
	// head_sha must come from last_commit.id (the source branch's current
	// HEAD), not merge_commit_sha (null on an open MR per GitLab's
	// documented payload — using it would silently store an empty string
	// here and break review-task dedup for every open MR).
	if row.HeadSha != "abc1112223334445556667778889990001112223" {
		t.Errorf("head_sha = %q, want the open MR's last_commit.id", row.HeadSha)
	}
	if row.PrCreatedAt.Time.Year() != 2026 {
		t.Errorf("pr_created_at should come from the payload's created_at, got %v", row.PrCreatedAt.Time)
	}
}

// TestGitLabWebhook_PushUpdatesHeadSha covers the other half of the
// head_sha fix: when a new commit lands on an open MR's source branch (a
// "push"/"update" delivery, not open or merge), last_commit.id changes and
// head_sha must track it — this is what lets review-task dedup key off the
// MR's CURRENT head rather than getting stuck on the id it had when first
// opened.
func TestGitLabWebhook_PushUpdatesHeadSha(t *testing.T) {
	if testHandler == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	ctx := context.Background()
	created, integ, secret := setupGitLabTestIssue(t, ctx)

	fireMergeRequestWebhook(t, uuidToString(integ.ID), secret, mrPayloadOpts{
		projectID:    integ.GitlabProjectID,
		projectPath:  integ.GitlabProjectPath,
		iid:          1,
		title:        "Fix " + created.Identifier,
		state:        "opened",
		action:       "open",
		sourceBranch: "fix/" + created.Identifier,
		lastCommitID: "firstcommit1112223334445556667778889990",
	})
	fireMergeRequestWebhook(t, uuidToString(integ.ID), secret, mrPayloadOpts{
		projectID:    integ.GitlabProjectID,
		projectPath:  integ.GitlabProjectPath,
		iid:          1,
		title:        "Fix " + created.Identifier,
		state:        "opened",
		action:       "update",
		sourceBranch: "fix/" + created.Identifier,
		lastCommitID: "secondcommit22233344455566677788899900011",
	})

	rows, err := testHandler.Queries.ListPullRequestsByIssue(ctx, parseUUID(created.ID))
	if err != nil {
		t.Fatalf("ListPullRequestsByIssue: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 linked MR, got %d", len(rows))
	}
	if got := rows[0].HeadSha; got != "secondcommit22233344455566677788899900011" {
		t.Errorf("head_sha = %q, want the pushed update's new last_commit.id", got)
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
	created, integ, secret := setupGitLabTestIssue(t, ctx)

	fireMergeRequestWebhook(t, uuidToString(integ.ID), secret, mrPayloadOpts{
		projectID:    integ.GitlabProjectID,
		projectPath:  integ.GitlabProjectPath,
		iid:          1,
		title:        "Closes " + created.Identifier,
		state:        "merged",
		action:       "merge",
		sourceBranch: "fix/foo",
		createdAt:    "2026-01-16 05:56:22 UTC",
		updatedAt:    "2026-01-16 06:00:00 UTC",
		mergedAt:     "2026-01-16 06:00:00 UTC",
		// A merged MR carries both: last_commit is still the reviewed
		// source-branch HEAD (what head_sha must store), merge_commit_sha
		// is the newly created merge commit (not the same value, and not
		// what head_sha tracks — see TestGitLabWebhook_AutoLinksAndUpsertsMR).
		lastCommitID:   "sourcecommit111222333444555666777888999",
		mergeCommitSha: "mergecommit999888777666555444333222111",
	})

	rows, err := testHandler.Queries.ListPullRequestsByIssue(ctx, parseUUID(created.ID))
	if err != nil {
		t.Fatalf("ListPullRequestsByIssue: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 linked MR, got %d", len(rows))
	}
	if got := rows[0].HeadSha; got != "sourcecommit111222333444555666777888999" {
		t.Errorf("head_sha = %q, want the merged MR's last_commit.id (the reviewed source HEAD), not merge_commit_sha", got)
	}

	final, err := testHandler.Queries.GetIssue(ctx, parseUUID(created.ID))
	if err != nil {
		t.Fatalf("GetIssue: %v", err)
	}
	if final.Status != "done" {
		t.Errorf("expected issue done after merged MR with closing intent, got %q", final.Status)
	}
	if !rows[0].MergedAt.Valid {
		t.Fatalf("expected merged_at set from the payload, got %+v", rows[0])
	}
}

// TestGitLabWebhook_PostMergeEditDoesNotUndoCloseIntent guards
// deriveMRPreserveCloseIntent's wiring: once a merge action has been
// delivered with closing intent declared, a later non-merge update to the
// same MR (e.g. a title edit) must not flip close_intent back to false —
// mirroring GitHub's preserveCloseIntent test coverage.
func TestGitLabWebhook_PostMergeEditDoesNotUndoCloseIntent(t *testing.T) {
	if testHandler == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	ctx := context.Background()
	created, integ, secret := setupGitLabTestIssue(t, ctx)

	fireMergeRequestWebhook(t, uuidToString(integ.ID), secret, mrPayloadOpts{
		projectID:    integ.GitlabProjectID,
		projectPath:  integ.GitlabProjectPath,
		iid:          1,
		title:        "Closes " + created.Identifier,
		state:        "merged",
		action:       "merge",
		sourceBranch: "fix/foo",
	})
	// Simulate a later webhook (e.g. a label change) whose title no longer
	// carries the closing keyword and whose action is not "merge"/"close".
	fireMergeRequestWebhook(t, uuidToString(integ.ID), secret, mrPayloadOpts{
		projectID:    integ.GitlabProjectID,
		projectPath:  integ.GitlabProjectPath,
		iid:          1,
		title:        created.Identifier + " (edited)",
		state:        "merged",
		action:       "update",
		sourceBranch: "fix/foo",
	})

	final, err := testHandler.Queries.GetIssue(ctx, parseUUID(created.ID))
	if err != nil {
		t.Fatalf("GetIssue: %v", err)
	}
	if final.Status != "done" {
		t.Errorf("expected issue to remain done after post-merge edit stripped the closing keyword, got %q", final.Status)
	}
}

// TestGitLabWebhook_InvalidCredentialsRejected covers the auth boundary: an
// unknown integration id or a mismatched X-Gitlab-Token header must 401.
func TestGitLabWebhook_InvalidCredentialsRejected(t *testing.T) {
	if testHandler == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	ctx := context.Background()
	_, integ, secret := setupGitLabTestIssue(t, ctx)

	t.Run("unknown integration id", func(t *testing.T) {
		rec := httptest.NewRecorder()
		fakeID := "00000000-0000-0000-0000-000000000000"
		hookReq := httptest.NewRequest("POST", "/api/webhooks/gitlab/"+fakeID, bytes.NewReader([]byte("{}")))
		hookReq = withURLParam(hookReq, "integrationId", fakeID)
		hookReq.Header.Set("X-Gitlab-Event", "Merge Request Hook")
		hookReq.Header.Set("X-Gitlab-Token", secret)
		testHandler.HandleGitLabWebhook(rec, hookReq)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("expected 401 for unknown integration id, got %d", rec.Code)
		}
	})

	t.Run("mismatched secret header", func(t *testing.T) {
		rec := httptest.NewRecorder()
		id := uuidToString(integ.ID)
		hookReq := httptest.NewRequest("POST", "/api/webhooks/gitlab/"+id, bytes.NewReader([]byte("{}")))
		hookReq = withURLParam(hookReq, "integrationId", id)
		hookReq.Header.Set("X-Gitlab-Event", "Merge Request Hook")
		hookReq.Header.Set("X-Gitlab-Token", "wrong-secret")
		testHandler.HandleGitLabWebhook(rec, hookReq)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("expected 401 for mismatched X-Gitlab-Token, got %d", rec.Code)
		}
	})
}

// TestGitLabWebhook_ProjectIDMismatchRejected covers the trust-boundary fix:
// a valid secret for integration A does not authorize a payload describing
// a DIFFERENT GitLab project (e.g. an admin pasted integration A's URL/
// secret into project B's webhook settings, or someone is trying to forge
// events for a project they were never issued credentials for).
func TestGitLabWebhook_ProjectIDMismatchRejected(t *testing.T) {
	if testHandler == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	ctx := context.Background()
	created, integ, secret := setupGitLabTestIssue(t, ctx)

	rec := httptest.NewRecorder()
	id := uuidToString(integ.ID)
	payload := map[string]any{
		"object_attributes": map[string]any{
			"iid":    1,
			"title":  "Fix " + created.Identifier,
			"state":  "opened",
			"action": "open",
		},
		"project": map[string]any{
			"id":                  integ.GitlabProjectID + 999, // different project
			"path_with_namespace": "someone-else/other-project",
		},
	}
	raw, _ := json.Marshal(payload)
	hookReq := httptest.NewRequest("POST", "/api/webhooks/gitlab/"+id, bytes.NewReader(raw))
	hookReq = withURLParam(hookReq, "integrationId", id)
	hookReq.Header.Set("X-Gitlab-Event", "Merge Request Hook")
	hookReq.Header.Set("X-Gitlab-Token", secret)
	testHandler.HandleGitLabWebhook(rec, hookReq)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for project id mismatch, got %d (%s)", rec.Code, rec.Body.String())
	}

	rows, err := testHandler.Queries.ListPullRequestsByIssue(ctx, parseUUID(created.ID))
	if err != nil {
		t.Fatalf("ListPullRequestsByIssue: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("expected no rows written for a rejected project-id-mismatch payload, got %d", len(rows))
	}
}

// TestGitLabWebhook_MalformedPayloadRejected covers the other half of the
// 202-swallows-everything fix: a body that isn't valid JSON can never
// succeed on retry either, so it must 400 rather than 202 — a 202 here
// would tell GitLab the (unparseable, never-persisted) event was delivered.
func TestGitLabWebhook_MalformedPayloadRejected(t *testing.T) {
	if testHandler == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	ctx := context.Background()
	_, integ, secret := setupGitLabTestIssue(t, ctx)

	rec := httptest.NewRecorder()
	id := uuidToString(integ.ID)
	hookReq := httptest.NewRequest("POST", "/api/webhooks/gitlab/"+id, bytes.NewReader([]byte("not valid json{{{")))
	hookReq = withURLParam(hookReq, "integrationId", id)
	hookReq.Header.Set("X-Gitlab-Event", "Merge Request Hook")
	hookReq.Header.Set("X-Gitlab-Token", secret)
	testHandler.HandleGitLabWebhook(rec, hookReq)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for a malformed payload, got %d (%s)", rec.Code, rec.Body.String())
	}
}

// TestHandleMergeRequestEvent_UpsertFailurePropagatesError covers the core
// of the 202-swallows-everything fix: when the MR upsert itself fails,
// handleMergeRequestEvent must return a non-nil error (so the HTTP layer
// can turn it into a 5xx and let GitLab retry) instead of logging and
// returning as if nothing went wrong. Exercised directly against
// handleMergeRequestEvent (bypassing the HTTP/integration-lookup layer)
// because a real integration row can never point at a workspace_id that
// doesn't exist — gitlab_integration.workspace_id has a FOREIGN KEY to
// workspace(id), so that combination is unreachable through the public
// create-integration API. Pointing a payload at a workspace_id with no
// matching row is the simplest way to force a real, deterministic
// constraint violation on the upsert without any fault-injection
// machinery.
func TestHandleMergeRequestEvent_UpsertFailurePropagatesError(t *testing.T) {
	if testHandler == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	ctx := context.Background()

	fakeWorkspaceID := parseUUID("00000000-0000-0000-0000-000000000abc")
	integ := db.GitlabIntegration{
		WorkspaceID:       fakeWorkspaceID,
		GitlabHost:        "gitlab.example",
		GitlabProjectID:   4242,
		GitlabProjectPath: "acme/ghost-workspace",
	}
	payload := glMergeRequestPayload{}
	payload.ObjectAttributes.IID = 1
	payload.ObjectAttributes.Title = "Fix SOMETHING-1"
	payload.ObjectAttributes.State = "opened"
	payload.ObjectAttributes.Action = "open"
	payload.Project.ID = integ.GitlabProjectID
	payload.Project.PathWithNamespace = integ.GitlabProjectPath

	err := testHandler.handleMergeRequestEvent(ctx, integ, payload)
	if err == nil {
		t.Fatal("expected handleMergeRequestEvent to return an error when the upsert violates the workspace_id foreign key, got nil")
	}

	var count int
	if scanErr := testPool.QueryRow(ctx, `
		SELECT count(*) FROM github_pull_request WHERE provider = 'gitlab' AND repo_owner = 'acme' AND repo_name = 'ghost-workspace'
	`).Scan(&count); scanErr != nil {
		t.Fatalf("count rows: %v", scanErr)
	}
	if count != 0 {
		t.Errorf("expected no row persisted after a failed upsert, got %d", count)
	}
}

// TestGitHubAndGitLabSamePathDoNotCollide guards the identity-key widening
// fix (migration 155): a GitHub PR and a GitLab MR sharing the same
// repo_owner/repo_name/pr_number string within one workspace must persist
// as two independent rows, not silently overwrite each other via the
// upserts' ON CONFLICT targets.
func TestGitHubAndGitLabSamePathDoNotCollide(t *testing.T) {
	if testHandler == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	ctx := context.Background()
	wsUUID := parseUUID(testWorkspaceID)
	t.Cleanup(func() {
		testPool.Exec(ctx, `DELETE FROM github_pull_request WHERE workspace_id = $1 AND repo_owner = 'acme' AND repo_name = 'shared-name'`, testWorkspaceID)
	})

	githubPR, err := testHandler.Queries.UpsertGitHubPullRequest(ctx, db.UpsertGitHubPullRequestParams{
		WorkspaceID:    wsUUID,
		InstallationID: pgtype.Int8{Int64: 424242, Valid: true},
		RepoOwner:      "acme",
		RepoName:       "shared-name",
		PrNumber:       12,
		Title:          "github pr",
		State:          "open",
		HtmlUrl:        "https://github.com/acme/shared-name/pull/12",
		PrCreatedAt:    parseGitLabTime("2026-01-01T00:00:00Z"),
		PrUpdatedAt:    parseGitLabTime("2026-01-01T00:00:00Z"),
		HeadSha:        "abc123",
	})
	if err != nil {
		t.Fatalf("UpsertGitHubPullRequest: %v", err)
	}

	gitlabMR, err := testHandler.Queries.UpsertGitLabMergeRequest(ctx, db.UpsertGitLabMergeRequestParams{
		WorkspaceID:  wsUUID,
		ProviderHost: "gitlab.example",
		RepoOwner:    "acme",
		RepoName:     "shared-name",
		PrNumber:     12,
		Title:        "gitlab mr",
		State:        "open",
		HtmlUrl:      "https://gitlab.example/acme/shared-name/-/merge_requests/12",
		PrCreatedAt:  parseGitLabTime("2026-01-01T00:00:00Z"),
		PrUpdatedAt:  parseGitLabTime("2026-01-01T00:00:00Z"),
		HeadSha:      "def456",
	})
	if err != nil {
		t.Fatalf("UpsertGitLabMergeRequest: %v", err)
	}

	if githubPR.ID == gitlabMR.ID {
		t.Fatalf("expected two distinct rows, got the same id for both")
	}

	// GetGitHubPullRequest (used by GitHub's own check_suite webhook path)
	// must resolve to the GitHub row specifically, not nondeterministically
	// whichever of the two same-path rows the planner picks — this is what
	// github.sql's added `provider = 'github'` filter guarantees.
	reloaded, err := testHandler.Queries.GetGitHubPullRequest(ctx, db.GetGitHubPullRequestParams{
		WorkspaceID: wsUUID,
		RepoOwner:   "acme",
		RepoName:    "shared-name",
		PrNumber:    12,
	})
	if err != nil {
		t.Fatalf("GetGitHubPullRequest: %v", err)
	}
	if reloaded.ID != githubPR.ID {
		t.Errorf("GetGitHubPullRequest returned the wrong row (id=%s), want the github row (id=%s)",
			uuidToString(reloaded.ID), uuidToString(githubPR.ID))
	}

	var count int
	if err := testPool.QueryRow(ctx, `
		SELECT count(*) FROM github_pull_request
		WHERE workspace_id = $1 AND repo_owner = 'acme' AND repo_name = 'shared-name' AND pr_number = 12
	`, testWorkspaceID).Scan(&count); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if count != 2 {
		t.Fatalf("expected 2 independent rows (github + gitlab) sharing the same repo path, got %d — the identity key collided", count)
	}
}

// TestGitLabIntegrationCRUD covers registering, listing, and removing a
// GitLab project integration. GET never echoes webhook_secret; only the
// create response does, exactly once.
func TestGitLabIntegrationCRUD(t *testing.T) {
	if testHandler == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	ctx := context.Background()
	t.Cleanup(func() {
		testPool.Exec(ctx, `DELETE FROM gitlab_integration WHERE workspace_id = $1`, testWorkspaceID)
	})

	createReq := httptest.NewRequest(http.MethodPost, "/api/workspaces/"+testWorkspaceID+"/gitlab/integrations", bytes.NewReader([]byte(`{
		"gitlab_host": "gitlab.example",
		"gitlab_project_id": 555,
		"gitlab_project_path": "acme/crud-test"
	}`)))
	createReq = withURLParam(createReq, "id", testWorkspaceID)
	createReq = createReq.WithContext(middleware.SetMemberContext(createReq.Context(), testWorkspaceID, db.Member{Role: "admin", UserID: parseUUID(testUserID)}))
	createW := httptest.NewRecorder()
	testHandler.CreateGitLabIntegration(createW, createReq)
	if createW.Code != http.StatusCreated {
		t.Fatalf("CreateGitLabIntegration: %d %s", createW.Code, createW.Body.String())
	}
	var created GitLabIntegrationResponse
	if err := json.NewDecoder(createW.Body).Decode(&created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	if created.WebhookURL == nil || *created.WebhookURL == "" {
		t.Fatalf("expected webhook_url on create, got %+v", created)
	}
	if created.WebhookSecret == nil || *created.WebhookSecret == "" {
		t.Fatalf("expected webhook_secret on create, got %+v", created)
	}

	listReq := httptest.NewRequest(http.MethodGet, "/api/workspaces/"+testWorkspaceID+"/gitlab/integrations", nil)
	listReq = withURLParam(listReq, "id", testWorkspaceID)
	listW := httptest.NewRecorder()
	testHandler.ListGitLabIntegrations(listW, listReq)
	if listW.Code != http.StatusOK {
		t.Fatalf("ListGitLabIntegrations: %d %s", listW.Code, listW.Body.String())
	}
	var listResp ListGitLabIntegrationsResponse
	json.NewDecoder(listW.Body).Decode(&listResp)
	if len(listResp.Integrations) != 1 {
		t.Fatalf("expected 1 integration listed, got %d", len(listResp.Integrations))
	}
	if listResp.Integrations[0].WebhookSecret != nil {
		t.Errorf("ListGitLabIntegrations must never echo webhook_secret, got %+v", listResp.Integrations[0])
	}
	if listResp.Integrations[0].WebhookURL != nil {
		t.Errorf("ListGitLabIntegrations must never echo webhook_url either, got %+v", listResp.Integrations[0])
	}

	deleteReq := httptest.NewRequest(http.MethodDelete, "/api/workspaces/"+testWorkspaceID+"/gitlab/integrations/"+created.ID, nil)
	deleteReq = withURLParams(deleteReq, "id", testWorkspaceID, "integrationId", created.ID)
	deleteW := httptest.NewRecorder()
	testHandler.DeleteGitLabIntegration(deleteW, deleteReq)
	if deleteW.Code != http.StatusNoContent {
		t.Fatalf("DeleteGitLabIntegration: %d %s", deleteW.Code, deleteW.Body.String())
	}

	listW2 := httptest.NewRecorder()
	testHandler.ListGitLabIntegrations(listW2, listReq)
	var listResp2 ListGitLabIntegrationsResponse
	json.NewDecoder(listW2.Body).Decode(&listResp2)
	if len(listResp2.Integrations) != 0 {
		t.Errorf("expected 0 integrations after delete, got %d", len(listResp2.Integrations))
	}
}

// TestGitLabIntegrationCreate_RotatesSecretOnReregister covers the
// re-register-to-rotate flow: registering the same (workspace, host,
// project id) again replaces the secret rather than erroring, and the OLD
// secret stops verifying against the (unchanged) integration id.
func TestGitLabIntegrationCreate_RotatesSecretOnReregister(t *testing.T) {
	if testHandler == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	ctx := context.Background()
	t.Cleanup(func() {
		testPool.Exec(ctx, `DELETE FROM gitlab_integration WHERE workspace_id = $1`, testWorkspaceID)
	})

	register := func(t *testing.T) GitLabIntegrationResponse {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/api/workspaces/"+testWorkspaceID+"/gitlab/integrations", bytes.NewReader([]byte(`{
			"gitlab_host": "gitlab.example",
			"gitlab_project_id": 777,
			"gitlab_project_path": "acme/rotate-test"
		}`)))
		req = withURLParam(req, "id", testWorkspaceID)
		req = req.WithContext(middleware.SetMemberContext(req.Context(), testWorkspaceID, db.Member{Role: "admin", UserID: parseUUID(testUserID)}))
		w := httptest.NewRecorder()
		testHandler.CreateGitLabIntegration(w, req)
		if w.Code != http.StatusCreated {
			t.Fatalf("CreateGitLabIntegration: %d %s", w.Code, w.Body.String())
		}
		var resp GitLabIntegrationResponse
		json.NewDecoder(w.Body).Decode(&resp)
		return resp
	}

	first := register(t)
	second := register(t)
	if first.ID != second.ID {
		t.Fatalf("expected re-registering the same project to reuse the same integration id, got %s vs %s", first.ID, second.ID)
	}
	if *first.WebhookSecret == *second.WebhookSecret {
		t.Errorf("expected re-registering to rotate the secret, got the same value twice")
	}

	// The OLD secret must no longer verify.
	rec := httptest.NewRecorder()
	hookReq := httptest.NewRequest("POST", "/api/webhooks/gitlab/"+first.ID, bytes.NewReader([]byte("{}")))
	hookReq = withURLParam(hookReq, "integrationId", first.ID)
	hookReq.Header.Set("X-Gitlab-Event", "Merge Request Hook")
	hookReq.Header.Set("X-Gitlab-Token", *first.WebhookSecret)
	testHandler.HandleGitLabWebhook(rec, hookReq)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected the pre-rotation secret to be rejected, got %d", rec.Code)
	}
}
