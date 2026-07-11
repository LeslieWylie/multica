package service

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// TestClaimTaskRunOnlySerializedPerAgent guards the gap fixed alongside this
// test: run_only autopilot tasks (issue_id AND chat_session_id both NULL,
// autopilot_run_id SET) matched none of ClaimAgentTask's three original
// mutual-exclusion branches — not the issue branch (no issue_id), not the
// chat branch (no chat_session_id), and not the quick-create branch (which
// requires autopilot_run_id IS NULL). Two run_only tasks for the same agent
// were therefore claimable and dispatchable concurrently with no
// serialization at all, bounded only by max_concurrent_tasks — e.g. the same
// webhook-triggered autopilot firing twice in quick succession. This test
// enqueues two run_only tasks for one agent and claims both concurrently;
// with the fix in place, exactly one should be claimed per round (mirrors
// TestClaimTaskConcurrentCapacityRespected's shape, but for the run_only FK
// combination instead of the issue-linked one).
func TestClaimTaskRunOnlySerializedPerAgent(t *testing.T) {
	ctx := context.Background()
	pool := newTaskClaimRacePool(t)
	queries := db.New(pool)

	agentID, runtimeID, runID1, runID2 := createRunOnlyClaimFixture(t, ctx, pool)
	agentUUID := util.MustParseUUID(agentID)

	triggerName := fmt.Sprintf("claim_run_only_sleep_%d", time.Now().UnixNano())
	functionName := triggerName + "_fn"
	createSleepTrigger(t, ctx, pool, triggerName, functionName, agentID)
	svc := NewTaskService(queries, pool, nil, events.New())

	if _, err := pool.Exec(ctx, `
		INSERT INTO agent_task_queue (agent_id, runtime_id, autopilot_run_id, status, priority, context)
		VALUES ($1, $2, $3, 'queued', 0, '{}'::jsonb)
	`, agentID, runtimeID, runID1); err != nil {
		t.Fatalf("enqueue run_only task 1: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO agent_task_queue (agent_id, runtime_id, autopilot_run_id, status, priority, context)
		VALUES ($1, $2, $3, 'queued', 0, '{}'::jsonb)
	`, agentID, runtimeID, runID2); err != nil {
		t.Fatalf("enqueue run_only task 2: %v", err)
	}

	const workers = 2
	start := make(chan struct{})
	claimed := make(chan string, workers)
	errs := make(chan error, workers)

	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			task, err := svc.ClaimTask(ctx, agentUUID)
			if err != nil {
				errs <- err
				return
			}
			if task != nil {
				claimed <- util.UUIDToString(task.ID)
			}
		}()
	}

	close(start)
	wg.Wait()
	close(claimed)
	close(errs)

	for err := range errs {
		if err != nil {
			t.Fatalf("claim task: %v", err)
		}
	}

	var claimedIDs []string
	for id := range claimed {
		claimedIDs = append(claimedIDs, id)
	}
	// Before the fix, both run_only tasks matched none of ClaimAgentTask's
	// exclusion branches, so both concurrent claims would succeed here
	// (claimedIDs would have length 2). With the fix, only one is claimable
	// while the other run_only task for the same agent is already active.
	if len(claimedIDs) != 1 {
		t.Fatalf("expected exactly 1 claimed run_only task while the other is active, got %d (%v)", len(claimedIDs), claimedIDs)
	}

	var active int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM agent_task_queue
		WHERE agent_id = $1 AND status IN ('dispatched', 'running', 'waiting_local_directory')
	`, agentID).Scan(&active); err != nil {
		t.Fatalf("count active tasks: %v", err)
	}
	if active != 1 {
		t.Fatalf("expected 1 active run_only task after concurrent claims, got %d", active)
	}

	var stillQueued int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM agent_task_queue
		WHERE agent_id = $1 AND status = 'queued'
	`, agentID).Scan(&stillQueued); err != nil {
		t.Fatalf("count queued tasks: %v", err)
	}
	if stillQueued != 1 {
		t.Fatalf("expected the second run_only task to remain queued (serialized, not lost), got %d still queued", stillQueued)
	}
}

// createRunOnlyClaimFixture mirrors createClaimCapacityFixture's shape (user
// / workspace / member / runtime / agent) but additionally creates the
// autopilot + autopilot_run rows a run_only task actually links to via
// autopilot_run_id, and sets max_concurrent_tasks=2 (not 1) so a false-pass
// from capacity alone can't hide a missing per-agent run_only exclusion —
// the two concurrent claims in the test above must be blocked by the NOT
// EXISTS branch this fix adds, not by simply running out of concurrency
// slots. Returns the agent id and two autopilot_run ids from two DIFFERENT
// autopilots on that agent, matching the "coarse-grained: any run_only task
// for the same agent" tradeoff the fix makes (see agent.sql comment) — this
// specifically proves that even unrelated autopilots serialize against each
// other, not just repeated runs of the same one.
func createRunOnlyClaimFixture(t *testing.T, ctx context.Context, pool *pgxpool.Pool) (agentID, runtimeID, runID1, runID2 string) {
	t.Helper()

	suffix := time.Now().UnixNano()
	email := fmt.Sprintf("claim-run-only-%d@multica.ai", suffix)
	slug := fmt.Sprintf("claim-run-only-%d", suffix)

	var userID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO "user" (name, email)
		VALUES ($1, $2)
		RETURNING id
	`, "Claim Run Only Test", email).Scan(&userID); err != nil {
		t.Fatalf("create user: %v", err)
	}

	var workspaceID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO workspace (name, slug, description, issue_prefix)
		VALUES ($1, $2, $3, $4)
		RETURNING id
	`, "Claim Run Only Test", slug, "temporary run_only claim race test workspace", "CRO").Scan(&workspaceID); err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO member (workspace_id, user_id, role)
		VALUES ($1, $2, 'owner')
	`, workspaceID, userID); err != nil {
		t.Fatalf("create member: %v", err)
	}

	if err := pool.QueryRow(ctx, `
		INSERT INTO agent_runtime (
			workspace_id, daemon_id, name, runtime_mode, provider,
			status, device_info, metadata, last_seen_at, visibility, owner_id
		)
		VALUES ($1, NULL, $2, 'cloud', 'claim_run_only_test', 'online', 'test runtime', '{}'::jsonb, now(), 'private', $3)
		RETURNING id
	`, workspaceID, "Claim Run Only Runtime", userID).Scan(&runtimeID); err != nil {
		t.Fatalf("create runtime: %v", err)
	}

	if err := pool.QueryRow(ctx, `
		INSERT INTO agent (
			workspace_id, name, description, runtime_mode, runtime_config,
			runtime_id, visibility, max_concurrent_tasks, owner_id
		)
		VALUES ($1, $2, '', 'cloud', '{}'::jsonb, $3, 'private', 2, $4)
		RETURNING id
	`, workspaceID, "Claim Run Only Agent", runtimeID, userID).Scan(&agentID); err != nil {
		t.Fatalf("create agent: %v", err)
	}

	// Two DIFFERENT autopilots on the same agent, each with one run — proves
	// the fix's coarse-grained "any run_only task for this agent" exclusion
	// applies across distinct autopilots, not just repeated runs of one.
	var autopilotID1, autopilotID2 string
	if err := pool.QueryRow(ctx, `
		INSERT INTO autopilot (workspace_id, title, assignee_id, execution_mode, created_by_type, created_by_id)
		VALUES ($1, $2, $3, 'run_only', 'member', $4)
		RETURNING id
	`, workspaceID, "Claim Run Only Autopilot 1", agentID, userID).Scan(&autopilotID1); err != nil {
		t.Fatalf("create autopilot 1: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO autopilot (workspace_id, title, assignee_id, execution_mode, created_by_type, created_by_id)
		VALUES ($1, $2, $3, 'run_only', 'member', $4)
		RETURNING id
	`, workspaceID, "Claim Run Only Autopilot 2", agentID, userID).Scan(&autopilotID2); err != nil {
		t.Fatalf("create autopilot 2: %v", err)
	}

	if err := pool.QueryRow(ctx, `
		INSERT INTO autopilot_run (autopilot_id, source, status)
		VALUES ($1, 'webhook', 'running')
		RETURNING id
	`, autopilotID1).Scan(&runID1); err != nil {
		t.Fatalf("create autopilot_run 1: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO autopilot_run (autopilot_id, source, status)
		VALUES ($1, 'webhook', 'running')
		RETURNING id
	`, autopilotID2).Scan(&runID2); err != nil {
		t.Fatalf("create autopilot_run 2: %v", err)
	}

	t.Cleanup(func() {
		cleanupCtx := context.Background()
		pool.Exec(cleanupCtx, `DELETE FROM agent_task_queue WHERE agent_id = $1`, agentID)
		pool.Exec(cleanupCtx, `DELETE FROM autopilot_run WHERE id IN ($1, $2)`, runID1, runID2)
		pool.Exec(cleanupCtx, `DELETE FROM autopilot WHERE id IN ($1, $2)`, autopilotID1, autopilotID2)
		pool.Exec(cleanupCtx, `DELETE FROM agent WHERE id = $1`, agentID)
		pool.Exec(cleanupCtx, `DELETE FROM agent_runtime WHERE id = $1`, runtimeID)
		pool.Exec(cleanupCtx, `DELETE FROM member WHERE workspace_id = $1 AND user_id = $2`, workspaceID, userID)
		pool.Exec(cleanupCtx, `DELETE FROM workspace WHERE id = $1`, workspaceID)
		pool.Exec(cleanupCtx, `DELETE FROM "user" WHERE id = $1`, userID)
	})

	return agentID, runtimeID, runID1, runID2
}
