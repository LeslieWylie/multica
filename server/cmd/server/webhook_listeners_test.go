package main

import (
	"testing"

	"github.com/multica-ai/multica/server/internal/handler"
)

func strptr(s string) *string { return &s }

func TestWebhookIssuePayload(t *testing.T) {
	t.Run("typed IssueResponse with project + assignee", func(t *testing.T) {
		issue := handler.IssueResponse{
			ID:           "i1",
			Identifier:   "MUL-1",
			ProjectID:    strptr("proj-1"),
			AssigneeType: strptr("member"),
			AssigneeID:   strptr("mem-1"),
		}
		f, ok := webhookIssuePayload(issue)
		if !ok {
			t.Fatal("expected ok for IssueResponse")
		}
		if f.projectID != "proj-1" {
			t.Errorf("projectID = %q, want proj-1", f.projectID)
		}
		if f.identifier != "MUL-1" {
			t.Errorf("identifier = %q, want MUL-1", f.identifier)
		}
		if f.assigneeType != "member" || f.assigneeID != "mem-1" {
			t.Errorf("assignee = %q/%q, want member/mem-1", f.assigneeType, f.assigneeID)
		}
		if _, isResp := f.issue.(handler.IssueResponse); !isResp {
			t.Errorf("issue body should pass through as IssueResponse")
		}
	})

	t.Run("typed IssueResponse without project or assignee", func(t *testing.T) {
		f, ok := webhookIssuePayload(handler.IssueResponse{ID: "i1", Identifier: "MUL-2"})
		if !ok || f.projectID != "" || f.assigneeType != "" || f.assigneeID != "" {
			t.Errorf("got %+v ok=%v, want ok + empty project/assignee", f, ok)
		}
		if f.identifier != "MUL-2" {
			t.Errorf("identifier = %q, want MUL-2", f.identifier)
		}
	})

	t.Run("map shape with *string fields (issueToMap)", func(t *testing.T) {
		m := map[string]any{
			"id":            "i1",
			"identifier":    "MUL-3",
			"project_id":    strptr("proj-2"),
			"assignee_type": strptr("agent"),
			"assignee_id":   strptr("agent-9"),
			"status":        "todo",
		}
		f, ok := webhookIssuePayload(m)
		if !ok {
			t.Fatal("expected ok for map")
		}
		if f.projectID != "proj-2" {
			t.Errorf("projectID = %q, want proj-2", f.projectID)
		}
		if f.identifier != "MUL-3" {
			t.Errorf("identifier = %q, want MUL-3", f.identifier)
		}
		if f.assigneeType != "agent" || f.assigneeID != "agent-9" {
			t.Errorf("assignee = %q/%q, want agent/agent-9", f.assigneeType, f.assigneeID)
		}
		if _, isMap := f.issue.(map[string]any); !isMap {
			t.Errorf("issue body should pass through as map")
		}
	})

	t.Run("map shape with nil *string project_id (workspace-level)", func(t *testing.T) {
		var nilp *string
		m := map[string]any{"id": "i1", "project_id": nilp}
		f, ok := webhookIssuePayload(m)
		if !ok || f.projectID != "" {
			t.Errorf("ok=%v projectID=%q, want ok + empty", ok, f.projectID)
		}
	})

	t.Run("map shape with plain string fields", func(t *testing.T) {
		m := map[string]any{"id": "i1", "project_id": "proj-3", "assignee_type": "squad", "assignee_id": "sq-1"}
		f, ok := webhookIssuePayload(m)
		if !ok || f.projectID != "proj-3" {
			t.Errorf("ok=%v projectID=%q, want proj-3", ok, f.projectID)
		}
		if f.assigneeType != "squad" || f.assigneeID != "sq-1" {
			t.Errorf("assignee = %q/%q, want squad/sq-1", f.assigneeType, f.assigneeID)
		}
	})

	t.Run("unknown shape", func(t *testing.T) {
		if _, ok := webhookIssuePayload(42); ok {
			t.Error("expected ok=false for unknown shape")
		}
		if _, ok := webhookIssuePayload(nil); ok {
			t.Error("expected ok=false for nil")
		}
	})
}

// TestAssigneeChangedPrevFieldsExtraction covers stringFromMap against the
// exact prev_assignee_type/prev_assignee_id shapes issue.go's publish calls
// emit: textToPtr/uuidToPtr produce *string, nil when the issue was
// previously unassigned. registerWebhookListeners reads these two fields
// straight off the issue:updated payload (not through webhookIssuePayload,
// which only extracts the CURRENT assignee) to build IssueAssigned's
// PreviousAssigneeType/PreviousAssigneeID.
func TestAssigneeChangedPrevFieldsExtraction(t *testing.T) {
	t.Run("previously assigned to a member", func(t *testing.T) {
		payload := map[string]any{
			"prev_assignee_type": strptr("member"),
			"prev_assignee_id":   strptr("mem-1"),
		}
		if got := stringFromMap(payload["prev_assignee_type"]); got != "member" {
			t.Errorf("prev_assignee_type = %q, want member", got)
		}
		if got := stringFromMap(payload["prev_assignee_id"]); got != "mem-1" {
			t.Errorf("prev_assignee_id = %q, want mem-1", got)
		}
	})

	t.Run("previously unassigned (nil *string)", func(t *testing.T) {
		var nilType, nilID *string
		payload := map[string]any{
			"prev_assignee_type": nilType,
			"prev_assignee_id":   nilID,
		}
		if got := stringFromMap(payload["prev_assignee_type"]); got != "" {
			t.Errorf("prev_assignee_type = %q, want empty", got)
		}
		if got := stringFromMap(payload["prev_assignee_id"]); got != "" {
			t.Errorf("prev_assignee_id = %q, want empty", got)
		}
	})

	t.Run("field absent from payload entirely", func(t *testing.T) {
		payload := map[string]any{}
		if got := stringFromMap(payload["prev_assignee_type"]); got != "" {
			t.Errorf("prev_assignee_type = %q, want empty", got)
		}
	})
}

func TestWebhookCommentPayload(t *testing.T) {
	t.Run("typed CommentResponse (human/system comment path)", func(t *testing.T) {
		comment := handler.CommentResponse{
			ID:         "c1",
			IssueID:    "issue-1",
			AuthorType: "member",
			AuthorID:   "mem-1",
			Content:    "hello",
		}
		f, ok := webhookCommentPayload(comment)
		if !ok {
			t.Fatal("expected ok for CommentResponse")
		}
		if f.issueID != "issue-1" {
			t.Errorf("issueID = %q, want issue-1", f.issueID)
		}
		if _, isResp := f.comment.(handler.CommentResponse); !isResp {
			t.Errorf("comment body should pass through as CommentResponse")
		}
	})

	t.Run("map shape (agent-authored comment path, task.go)", func(t *testing.T) {
		m := map[string]any{
			"id":          "c2",
			"issue_id":    "issue-2",
			"author_type": "agent",
			"author_id":   "agent-1",
			"content":     "done",
		}
		f, ok := webhookCommentPayload(m)
		if !ok {
			t.Fatal("expected ok for map")
		}
		if f.issueID != "issue-2" {
			t.Errorf("issueID = %q, want issue-2", f.issueID)
		}
		if _, isMap := f.comment.(map[string]any); !isMap {
			t.Errorf("comment body should pass through as map")
		}
	})

	t.Run("map shape missing issue_id", func(t *testing.T) {
		m := map[string]any{"id": "c3"}
		f, ok := webhookCommentPayload(m)
		if !ok || f.issueID != "" {
			t.Errorf("ok=%v issueID=%q, want ok + empty", ok, f.issueID)
		}
	})

	t.Run("unknown shape", func(t *testing.T) {
		if _, ok := webhookCommentPayload(42); ok {
			t.Error("expected ok=false for unknown shape")
		}
		if _, ok := webhookCommentPayload(nil); ok {
			t.Error("expected ok=false for nil")
		}
	})
}
