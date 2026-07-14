/**
 * @vitest-environment jsdom
 */
import { describe, expect, it, vi, beforeEach } from "vitest";
import { renderHook, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { ReactNode } from "react";

import { setApiInstance } from "../api";
import type { ApiClient } from "../api/client";
import { useUpdateWebhookSubscription } from "./mutations";
import { webhookKeys } from "./queries";
import type {
  ListWebhookSubscriptionsResponse,
  WebhookSubscription,
} from "../types";

vi.mock("../hooks", () => ({
  useWorkspaceId: () => "ws-1",
}));

const WS_ID = "ws-1";

function makeSub(overrides: Partial<WebhookSubscription> = {}): WebhookSubscription {
  return {
    id: "sub-1",
    workspace_id: WS_ID,
    project_id: null,
    url: "https://example.com/hook",
    events: ["issue.status_changed"],
    enabled: true,
    secret_hint: "ab12",
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-01-01T00:00:00Z",
    ...overrides,
  };
}

function createWrapper(qc: QueryClient) {
  return function Wrapper({ children }: { children: ReactNode }) {
    return <QueryClientProvider client={qc}>{children}</QueryClientProvider>;
  };
}

// Guards the fix for the checkbox-race the /code-review-style audit flagged:
// toggling two event checkboxes back-to-back must not have the second PATCH
// silently overwrite the first because it was built from a stale pre-first-
// click subscription snapshot. onMutate must patch the cache synchronously
// before the network call resolves, so a second mutation started while the
// first is still in flight sees the first's optimistic result.
describe("useUpdateWebhookSubscription optimistic cache patch", () => {
  let qc: QueryClient;
  let updateWebhookSubscription: ReturnType<
    typeof vi.fn<(id: string, data: unknown) => Promise<WebhookSubscription>>
  >;

  beforeEach(() => {
    qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    updateWebhookSubscription = vi.fn();
    setApiInstance({ updateWebhookSubscription } as unknown as ApiClient);
  });

  function seed(subs: WebhookSubscription[]) {
    qc.setQueryData<ListWebhookSubscriptionsResponse>(webhookKeys.list(WS_ID), {
      subscriptions: subs,
    });
  }

  function cached(): WebhookSubscription[] {
    return (
      qc.getQueryData<ListWebhookSubscriptionsResponse>(webhookKeys.list(WS_ID))
        ?.subscriptions ?? []
    );
  }

  it("patches the cache synchronously before the network call resolves", async () => {
    seed([makeSub({ events: ["issue.status_changed"] })]);
    // Never resolves within this test — proves the cache patch happens in
    // onMutate, not after the promise settles.
    updateWebhookSubscription.mockReturnValue(new Promise(() => {}));

    const { result } = renderHook(() => useUpdateWebhookSubscription(), {
      wrapper: createWrapper(qc),
    });

    result.current.mutate({
      id: "sub-1",
      events: ["issue.status_changed", "issue.assignee_changed"],
    });

    await waitFor(() =>
      expect(cached()[0]?.events).toEqual([
        "issue.status_changed",
        "issue.assignee_changed",
      ]),
    );
  });

  it("a second toggle started while the first PATCH is still in flight builds on the first's optimistic result, not a stale snapshot", async () => {
    seed([makeSub({ events: ["issue.status_changed"] })]);
    // Both calls hang — simulates two rapid clicks where neither PATCH has
    // resolved yet when the second one fires.
    updateWebhookSubscription.mockReturnValue(new Promise(() => {}));

    const { result } = renderHook(() => useUpdateWebhookSubscription(), {
      wrapper: createWrapper(qc),
    });

    // First click: check issue.assignee_changed.
    result.current.mutate({
      id: "sub-1",
      events: ["issue.status_changed", "issue.assignee_changed"],
    });
    await waitFor(() =>
      expect(cached()[0]?.events).toContain("issue.assignee_changed"),
    );

    // Second click reads the CURRENT cache (post-first-optimistic-patch) to
    // build its own next array — exactly what the UI's handleToggleEvent
    // does by reading `sub.events` off the rendered (query-backed) object.
    const nextEvents = [...cached()[0]!.events, "comment.created"];
    result.current.mutate({ id: "sub-1", events: nextEvents });

    await waitFor(() =>
      expect(cached()[0]?.events).toEqual([
        "issue.status_changed",
        "issue.assignee_changed",
        "comment.created",
      ]),
    );
  });

  it("rolls back to the pre-mutation snapshot on error", async () => {
    seed([makeSub({ events: ["issue.status_changed"] })]);
    updateWebhookSubscription.mockRejectedValue(new Error("network error"));

    const { result } = renderHook(() => useUpdateWebhookSubscription(), {
      wrapper: createWrapper(qc),
    });

    result.current.mutate({
      id: "sub-1",
      events: ["issue.status_changed", "issue.assignee_changed"],
    });

    await waitFor(() => expect(result.current.isError).toBe(true));
    expect(cached()[0]?.events).toEqual(["issue.status_changed"]);
  });
});
