package main

import (
	"log/slog"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/handler"
	"github.com/multica-ai/multica/server/internal/integrations/outwebhook"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// registerWebhookListeners wires the outbound webhook dispatcher to the event
// bus. It listens for issue status changes, issue assignment changes, and new
// comments, POSTing a signed payload to every matching webhook_subscription
// (workspace-level or project-level).
//
// This mirrors registerAutopilotListeners: it filters issue:updated events on
// status_changed / assignee_changed and reads the typed handler.IssueResponse
// out of the payload. Delivery itself is async (the dispatcher detaches each
// POST), so this listener never blocks the synchronous bus dispatch.
//
// Scope of issue.status_changed (v1): it fires for status transitions published
// as issue:updated with status_changed=true. That covers user/API/PR-merge
// changes (single update, batch update, PR-merged) AND the system-internal
// agent task-failure / stuck-issue auto-reset (in_progress → todo), which now
// publishes a status_changed event from TaskService.HandleFailedTasks. It does
// NOT fire for status mutations that never publish issue:updated with
// status_changed. The issue payload arrives in one of two shapes — the typed
// handler.IssueResponse (handler paths) or a map[string]any (service paths) —
// and both are handled below.
//
// Scope of issue.assignee_changed: it fires for assignment changes published as
// issue:updated with assignee_changed=true (single update and batch update).
// It does NOT fire for the github.go PR-merge path or task.go's
// broadcastIssueUpdated, neither of which touches assignee — those never set
// assignee_changed=true, so this handler is a no-op for them.
//
// Scope of comment.created: it fires for every comment:created event
// (human CreateComment, agent-authored comments from task.go, and the
// MUL-2538 system child-done comment) — there is no filtering on author_type
// here; a subscription that wants to exclude system comments must filter on
// the receiving end. Project-level subscriptions only receive comments on
// issues in their own project (issue_project_id, resolved by each publish
// site from the issue's project_id, same as issue.status_changed/
// issue.assignee_changed). The comment payload arrives in one of two shapes — the
// typed handler.CommentResponse (handler paths) or a map[string]any
// (task.go's agent-comment path) — both handled by webhookCommentPayload.
func registerWebhookListeners(bus *events.Bus, d *outwebhook.Dispatcher) {
	bus.Subscribe(protocol.EventIssueUpdated, func(e events.Event) {
		payload, ok := e.Payload.(map[string]any)
		if !ok {
			return
		}
		fields, ok := webhookIssuePayload(payload["issue"])
		if !ok {
			slog.Debug("webhook listener: unrecognized issue payload shape")
			return
		}

		if statusChanged, _ := payload["status_changed"].(bool); statusChanged {
			prevStatus, _ := payload["prev_status"].(string)
			d.DispatchIssueStatusChanged(outwebhook.IssueStatusChanged{
				WorkspaceID:    e.WorkspaceID,
				ProjectID:      fields.projectID,
				ActorType:      e.ActorType,
				ActorID:        e.ActorID,
				PreviousStatus: prevStatus,
				Issue:          fields.issue,
				Identifier:     fields.identifier,
				AssigneeType:   fields.assigneeType,
				AssigneeID:     fields.assigneeID,
			})
		}

		if assigneeChanged, _ := payload["assignee_changed"].(bool); assigneeChanged {
			prevAssigneeType := stringFromMap(payload["prev_assignee_type"])
			prevAssigneeID := stringFromMap(payload["prev_assignee_id"])
			d.DispatchIssueAssigneeChanged(outwebhook.IssueAssigneeChanged{
				WorkspaceID:          e.WorkspaceID,
				ProjectID:            fields.projectID,
				ActorType:            e.ActorType,
				ActorID:              e.ActorID,
				Issue:                fields.issue,
				Identifier:           fields.identifier,
				AssigneeType:         fields.assigneeType,
				AssigneeID:           fields.assigneeID,
				PreviousAssigneeType: prevAssigneeType,
				PreviousAssigneeID:   prevAssigneeID,
			})
		}
	})

	bus.Subscribe(protocol.EventCommentCreated, func(e events.Event) {
		payload, ok := e.Payload.(map[string]any)
		if !ok {
			return
		}
		fields, ok := webhookCommentPayload(payload["comment"])
		if !ok {
			slog.Debug("webhook listener: unrecognized comment payload shape")
			return
		}
		issueTitle, _ := payload["issue_title"].(string)
		issueStatus, _ := payload["issue_status"].(string)
		issueProjectID := stringFromMap(payload["issue_project_id"])

		d.DispatchCommentCreated(outwebhook.CommentCreated{
			WorkspaceID: e.WorkspaceID,
			ProjectID:   issueProjectID,
			ActorType:   e.ActorType,
			ActorID:     e.ActorID,
			Comment:     fields.comment,
			IssueID:     fields.issueID,
			IssueTitle:  issueTitle,
			IssueStatus: issueStatus,
		})
	})
}

// webhookIssueFields is everything the listener extracts from an issue:updated
// payload in one pass: the project id (project-level routing), the raw issue
// body to embed verbatim, and the identifier + polymorphic assignee the
// dispatcher needs to build issue_url + resolve the assignee name. The extraction
// lives here (not in outwebhook) because reading the typed handler.IssueResponse
// from outwebhook would be an import cycle (handler imports outwebhook).
type webhookIssueFields struct {
	projectID    string
	issue        any
	identifier   string
	assigneeType string
	assigneeID   string
}

// webhookIssuePayload extracts webhookIssueFields from either shape of the
// issue:updated payload: the typed handler.IssueResponse (handler paths) or the
// map[string]any emitted by service-layer status changes (e.g. issueToMap).
// Returns ok=false when neither shape is present. Missing fields come back as "".
func webhookIssuePayload(raw any) (webhookIssueFields, bool) {
	switch v := raw.(type) {
	case handler.IssueResponse:
		f := webhookIssueFields{issue: v, identifier: v.Identifier}
		if v.ProjectID != nil {
			f.projectID = *v.ProjectID
		}
		if v.AssigneeType != nil {
			f.assigneeType = *v.AssigneeType
		}
		if v.AssigneeID != nil {
			f.assigneeID = *v.AssigneeID
		}
		return f, true
	case map[string]any:
		f := webhookIssueFields{issue: v}
		f.projectID = stringFromMap(v["project_id"])
		f.identifier, _ = v["identifier"].(string)
		f.assigneeType = stringFromMap(v["assignee_type"])
		f.assigneeID = stringFromMap(v["assignee_id"])
		return f, true
	default:
		return webhookIssueFields{}, false
	}
}

// stringFromMap reads a string value from an issue map field that may be a
// string, a *string (nullable columns serialized by the service layer), or
// absent/nil.
func stringFromMap(raw any) string {
	switch s := raw.(type) {
	case string:
		return s
	case *string:
		if s != nil {
			return *s
		}
	}
	return ""
}

// webhookCommentFields is everything the listener extracts from a
// comment:created payload's "comment" field: the raw comment body to embed
// verbatim, and the issue id it belongs to (there is no clickable issue_url
// for comments — see CommentCreated's doc comment).
type webhookCommentFields struct {
	comment any
	issueID string
}

// webhookCommentPayload extracts webhookCommentFields from either shape of
// the comment:created payload's "comment" field: the typed
// handler.CommentResponse (human CreateComment / system child-done comment,
// both published via handler.publish) or the map[string]any emitted by
// task.go's agent-comment path. Mirrors webhookIssuePayload's dual-shape
// handling. Returns ok=false when neither shape is present.
func webhookCommentPayload(raw any) (webhookCommentFields, bool) {
	switch v := raw.(type) {
	case handler.CommentResponse:
		return webhookCommentFields{comment: v, issueID: v.IssueID}, true
	case map[string]any:
		return webhookCommentFields{comment: v, issueID: stringFromMap(v["issue_id"])}, true
	default:
		return webhookCommentFields{}, false
	}
}
