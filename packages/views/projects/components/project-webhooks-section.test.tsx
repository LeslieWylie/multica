import type { ReactNode } from "react";
import { useState } from "react";
import { describe, it, expect, beforeEach, vi } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { I18nProvider } from "@multica/core/i18n/react";
import enCommon from "../../locales/en/common.json";
import enSettings from "../../locales/en/settings.json";
import enProjects from "../../locales/en/projects.json";
import type { WebhookSubscription } from "@multica/core/types";

const mockCreate = vi.hoisted(() => vi.fn());
const mockUpdate = vi.hoisted(() => vi.fn());
const mockDelete = vi.hoisted(() => vi.fn());

type MemberRole = "owner" | "admin" | "member";
const membersRef = vi.hoisted(() => ({
  current: [{ user_id: "user-1", role: "owner" as MemberRole }],
}));
const subsRef = vi.hoisted(() => ({
  current: [] as WebhookSubscription[],
}));
// Captures the projectId passed to webhookSubscriptionsOptions so we can assert
// the section scopes its query to the project.
const optionsProjectId = vi.hoisted(() => ({ current: undefined as string | undefined }));

vi.mock("@tanstack/react-query", () => ({
  useQuery: (opts: { queryKey: unknown[] }) => {
    const key = JSON.stringify(opts.queryKey);
    if (key.includes("members")) return { data: membersRef.current };
    if (key.includes("webhook")) return { data: subsRef.current };
    return { data: undefined };
  },
  queryOptions: <T,>(opts: T) => opts,
}));

vi.mock("@multica/core/hooks", () => ({
  useWorkspaceId: () => "workspace-1",
}));

vi.mock("@multica/core/workspace/queries", () => ({
  memberListOptions: () => ({ queryKey: ["members"], queryFn: vi.fn() }),
}));

vi.mock("@multica/core/webhooks/queries", () => ({
  webhookSubscriptionsOptions: (_wsId: string, projectId?: string) => {
    optionsProjectId.current = projectId;
    return { queryKey: ["webhook-subscriptions", projectId], queryFn: vi.fn() };
  },
}));

vi.mock("@multica/core/webhooks/mutations", () => ({
  useCreateWebhookSubscription: () => ({ mutateAsync: mockCreate, isPending: false }),
  // Real useState-backed implementation (not a static {isPending: false}
  // stub) — needed to test the checkbox-race fix (isEventUpdatePending),
  // which reads isPending/variables off this hook while a PATCH is in
  // flight. Mirrors TanStack Query's own semantics: both flip synchronously
  // with the mutation call and reset once mutateAsync's promise settles.
  useUpdateWebhookSubscription: () => {
    const [state, setState] = useState<{
      isPending: boolean;
      variables?: { id: string };
    }>({ isPending: false });
    const mutateAsync = async (vars: { id: string } & Record<string, unknown>) => {
      setState({ isPending: true, variables: vars });
      try {
        return await mockUpdate(vars);
      } finally {
        setState({ isPending: false, variables: vars });
      }
    };
    return { mutateAsync, isPending: state.isPending, variables: state.variables };
  },
  useDeleteWebhookSubscription: () => ({ mutateAsync: mockDelete, isPending: false }),
}));

vi.mock("@multica/core/auth", () => {
  const useAuthStore = Object.assign(
    (sel?: (s: { user: { id: string } }) => unknown) =>
      sel ? sel({ user: { id: "user-1" } }) : { user: { id: "user-1" } },
    { getState: () => ({ user: { id: "user-1" } }) },
  );
  return { useAuthStore };
});

vi.mock("sonner", () => ({
  toast: { success: vi.fn(), error: vi.fn() },
}));

// The real dropdown-menu is a Base UI popup (Portal + pointer-based open),
// which jsdom + userEvent.click doesn't reliably drive. Every other test
// in this repo that touches DropdownMenu mocks the module the same way —
// see projects-page.test.tsx / create-project.test.tsx / create-issue.test.tsx
// — rendering content unconditionally and wiring onCheckedChange straight
// to onClick, so a plain click exercises the same callback the real
// component would fire.
vi.mock("@multica/ui/components/ui/dropdown-menu", () => ({
  DropdownMenu: ({ children }: { children: React.ReactNode }) => (
    <>{children}</>
  ),
  DropdownMenuTrigger: ({ render }: { render: React.ReactNode }) => (
    <>{render}</>
  ),
  DropdownMenuContent: ({ children }: { children: React.ReactNode }) => (
    <div>{children}</div>
  ),
  DropdownMenuCheckboxItem: ({
    children,
    onCheckedChange,
    checked,
    disabled,
  }: {
    children: React.ReactNode;
    onCheckedChange?: (checked: boolean) => void;
    checked?: boolean;
    disabled?: boolean;
  }) => (
    <button
      type="button"
      disabled={disabled}
      onClick={() => onCheckedChange?.(!checked)}
    >
      {children}
    </button>
  ),
}));

import { ProjectWebhooksSection } from "./project-webhooks-section";

const TEST_RESOURCES = {
  en: { common: enCommon, settings: enSettings, projects: enProjects },
};

function I18nWrapper({ children }: { children: ReactNode }) {
  return (
    <I18nProvider locale="en" resources={TEST_RESOURCES}>
      {children}
    </I18nProvider>
  );
}

function makeSub(over: Partial<WebhookSubscription> = {}): WebhookSubscription {
  return {
    id: "sub-1",
    workspace_id: "workspace-1",
    project_id: "proj-1",
    url: "https://example.com/hook",
    events: ["issue.status_changed"],
    enabled: true,
    secret_hint: "ab12",
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-01-01T00:00:00Z",
    ...over,
  };
}

const PROJECT_ID = "proj-1";

describe("ProjectWebhooksSection", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    membersRef.current = [{ user_id: "user-1", role: "owner" }];
    subsRef.current = [];
    optionsProjectId.current = undefined;
  });

  it("renders nothing for non-admin members", () => {
    membersRef.current = [{ user_id: "user-1", role: "member" }];
    const { container } = render(
      <ProjectWebhooksSection projectId={PROJECT_ID} />,
      { wrapper: I18nWrapper },
    );
    expect(container).toBeEmptyDOMElement();
  });

  it("is collapsed by default and expands on click", async () => {
    render(<ProjectWebhooksSection projectId={PROJECT_ID} />, {
      wrapper: I18nWrapper,
    });
    // Header is always present; body (empty-state copy) only after expand.
    expect(screen.queryByText(/No webhooks yet/i)).toBeNull();
    await userEvent.click(screen.getByRole("button", { name: /Webhooks/i }));
    expect(screen.getByText(/No webhooks yet/i)).toBeTruthy();
  });

  it("scopes the subscriptions query to the project", async () => {
    render(<ProjectWebhooksSection projectId={PROJECT_ID} />, {
      wrapper: I18nWrapper,
    });
    await userEvent.click(screen.getByRole("button", { name: /Webhooks/i }));
    expect(optionsProjectId.current).toBe(PROJECT_ID);
  });

  it("creates a project-scoped subscription with project_id and only issue.status_changed by default", async () => {
    mockCreate.mockResolvedValue(makeSub({ secret: "whsec_revealed" }));
    render(<ProjectWebhooksSection projectId={PROJECT_ID} />, {
      wrapper: I18nWrapper,
    });
    await userEvent.click(screen.getByRole("button", { name: /Webhooks/i }));
    // Reveal the inline add input, then type + submit.
    await userEvent.click(screen.getByRole("button", { name: /^Add$/i }));
    await userEvent.type(
      screen.getByPlaceholderText(/example\.com/i),
      "https://p.example.com/hook",
    );
    await userEvent.click(screen.getByRole("button", { name: /^Add$/i }));

    await waitFor(() =>
      expect(mockCreate).toHaveBeenCalledWith({
        url: "https://p.example.com/hook",
        project_id: PROJECT_ID,
        events: ["issue.status_changed"],
      }),
    );
    expect(await screen.findByText("whsec_revealed")).toBeTruthy();
  });

  it("widens events via the compact events dropdown before creating", async () => {
    mockCreate.mockResolvedValue(makeSub({ secret: "whsec_revealed" }));
    render(<ProjectWebhooksSection projectId={PROJECT_ID} />, {
      wrapper: I18nWrapper,
    });
    await userEvent.click(screen.getByRole("button", { name: /Webhooks/i }));
    await userEvent.click(screen.getByRole("button", { name: /^Add$/i }));
    await userEvent.type(
      screen.getByPlaceholderText(/example\.com/i),
      "https://p.example.com/hook",
    );

    // Content renders unconditionally under the dropdown-menu mock (see the
    // module mock's comment) — no "open" step needed. Check issue.assignee_changed to
    // widen the default single-event selection.
    await userEvent.click(screen.getByText("issue.assignee_changed"));
    await userEvent.click(screen.getByRole("button", { name: /^Add$/i }));

    await waitFor(() =>
      expect(mockCreate).toHaveBeenCalledWith({
        url: "https://p.example.com/hook",
        project_id: PROJECT_ID,
        events: ["issue.status_changed", "issue.assignee_changed"],
      }),
    );
  });

  it("lists existing project subscriptions when expanded", async () => {
    subsRef.current = [makeSub({ url: "https://hooks.acme.dev/proj" })];
    render(<ProjectWebhooksSection projectId={PROJECT_ID} />, {
      wrapper: I18nWrapper,
    });
    await userEvent.click(screen.getByRole("button", { name: /Webhooks/i }));
    expect(screen.getByText("https://hooks.acme.dev/proj")).toBeTruthy();
  });

  it("disables a subscription's other event checkboxes while its own PATCH is in flight", async () => {
    subsRef.current = [
      makeSub({
        url: "https://hooks.acme.dev/proj",
        events: ["issue.status_changed", "issue.assignee_changed"],
      }),
    ];
    let resolveUpdate!: (v: unknown) => void;
    mockUpdate.mockImplementation(
      () => new Promise((resolve) => { resolveUpdate = resolve; }),
    );
    render(<ProjectWebhooksSection projectId={PROJECT_ID} />, {
      wrapper: I18nWrapper,
    });
    await userEvent.click(screen.getByRole("button", { name: /Webhooks/i }));

    const row = screen.getByText("https://hooks.acme.dev/proj").closest(".group") as HTMLElement;
    const rowScope = within(row);

    // Toggle comment.created (unchecked) for the row's subscription — its
    // PATCH never resolves during this test.
    await userEvent.click(rowScope.getByText("comment.created"));

    // A second click on a DIFFERENT event checkbox for the SAME subscription
    // must not fire a second concurrent PATCH while the first is still in
    // flight — same out-of-order-completion race as webhooks-section.tsx's
    // Checkbox, just via DropdownMenuCheckboxItem here.
    const statusButton = rowScope.getByText("issue.status_changed").closest("button")!;
    await waitFor(() => expect(statusButton).toBeDisabled());
    expect(mockUpdate).toHaveBeenCalledTimes(1);

    resolveUpdate(makeSub({ events: ["issue.status_changed", "issue.assignee_changed", "comment.created"] }));
    await waitFor(() => expect(statusButton).not.toBeDisabled());
  });
});
