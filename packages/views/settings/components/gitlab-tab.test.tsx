import { type ReactNode } from "react";
import { describe, it, expect, beforeEach, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { I18nProvider } from "@multica/core/i18n/react";
import enCommon from "../../locales/en/common.json";
import enSettings from "../../locales/en/settings.json";
import { GitLabTab } from "./gitlab-tab";

type MemberRole = "owner" | "admin" | "member" | "guest";

const membersRef = vi.hoisted(() => ({
  current: [{ user_id: "user-1", role: "owner" as MemberRole }],
}));
const listingRef = vi.hoisted(() => ({
  current: {
    integrations: [] as Array<{
      id: string;
      workspace_id: string;
      gitlab_host: string;
      gitlab_project_id: number;
      gitlab_project_path: string;
      created_at: string;
    }>,
  },
}));

const mockCreate = vi.hoisted(() => vi.fn());
const mockDelete = vi.hoisted(() => vi.fn());
const mockInvalidate = vi.hoisted(() => vi.fn());

vi.mock("@tanstack/react-query", () => ({
  useQuery: (opts: { queryKey: unknown[]; enabled?: boolean }) => {
    if (opts.enabled === false) return { data: undefined, isLoading: false };
    const key = JSON.stringify(opts.queryKey);
    if (key.includes("members")) return { data: membersRef.current, isLoading: false };
    if (key.includes("gitlab")) return { data: listingRef.current, isLoading: false };
    return { data: undefined, isLoading: false };
  },
  useQueryClient: () => ({ invalidateQueries: mockInvalidate }),
  queryOptions: <T,>(opts: T) => opts,
}));

vi.mock("@multica/core/hooks", () => ({
  useWorkspaceId: () => "workspace-1",
}));

vi.mock("@multica/core/workspace/queries", () => ({
  memberListOptions: () => ({ queryKey: ["members"], queryFn: vi.fn() }),
}));

vi.mock("@multica/core/gitlab", () => ({
  gitlabIntegrationsOptions: () => ({ queryKey: ["gitlab", "ws", "integrations"], queryFn: vi.fn() }),
  gitlabKeys: { integrations: (wsId: string) => ["gitlab", wsId, "integrations"] },
}));

vi.mock("@multica/core/auth", () => ({
  useAuthStore: Object.assign(
    (selector?: (s: unknown) => unknown) => {
      const state = { user: { id: "user-1" } };
      return selector ? selector(state) : state;
    },
    { getState: () => ({ user: { id: "user-1" } }) },
  ),
}));

vi.mock("@multica/core/api", () => ({
  api: {
    createGitLabIntegration: (...args: unknown[]) => mockCreate(...args),
    deleteGitLabIntegration: (...args: unknown[]) => mockDelete(...args),
  },
  ApiError: class ApiError extends Error {},
}));

vi.mock("sonner", () => ({
  toast: { success: vi.fn(), error: vi.fn() },
}));

const TEST_RESOURCES = { en: { common: enCommon, settings: enSettings } };

function renderTab() {
  function Wrapper({ children }: { children: ReactNode }) {
    return (
      <I18nProvider locale="en" resources={TEST_RESOURCES}>
        {children}
      </I18nProvider>
    );
  }
  return render(<GitLabTab />, { wrapper: Wrapper });
}

describe("GitLabTab", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    membersRef.current = [{ user_id: "user-1", role: "owner" }];
    listingRef.current = { integrations: [] };
  });

  it("shows the empty state when no projects are registered", () => {
    renderTab();
    expect(screen.getByText(enSettings.gitlab.empty)).toBeInTheDocument();
  });

  it("lists registered projects with their host and path", () => {
    listingRef.current = {
      integrations: [
        {
          id: "integ-1",
          workspace_id: "workspace-1",
          gitlab_host: "gitlab.example",
          gitlab_project_id: 42,
          gitlab_project_path: "acme/widget",
          created_at: "2026-01-01T00:00:00Z",
        },
      ],
    };
    renderTab();
    expect(screen.getByText("acme/widget")).toBeInTheDocument();
    expect(screen.getByText("gitlab.example")).toBeInTheDocument();
  });

  it("hides register/remove actions for non-admin members", () => {
    membersRef.current = [{ user_id: "user-1", role: "member" }];
    listingRef.current = {
      integrations: [
        {
          id: "integ-1",
          workspace_id: "workspace-1",
          gitlab_host: "gitlab.example",
          gitlab_project_id: 42,
          gitlab_project_path: "acme/widget",
          created_at: "2026-01-01T00:00:00Z",
        },
      ],
    };
    renderTab();
    expect(screen.queryByRole("button", { name: enSettings.gitlab.register_button })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: enSettings.gitlab.remove })).not.toBeInTheDocument();
    // The list itself still renders for a non-admin member.
    expect(screen.getByText("acme/widget")).toBeInTheDocument();
  });

  it("shows member-visible copy when nothing is registered and the caller can't manage it", () => {
    membersRef.current = [{ user_id: "user-1", role: "member" }];
    listingRef.current = { integrations: [] };
    renderTab();
    expect(screen.getByText(enSettings.gitlab.member_not_configured_hint)).toBeInTheDocument();
    expect(screen.getByText(enSettings.gitlab.member_not_configured_hint)).toBeInTheDocument();
  });

  it("registers a project and shows the webhook URL + secret exactly once", async () => {
    mockCreate.mockResolvedValue({
      id: "integ-2",
      workspace_id: "workspace-1",
      gitlab_host: "gitlab.example",
      gitlab_project_id: 99,
      gitlab_project_path: "acme/new-project",
      created_at: "2026-01-01T00:00:00Z",
      webhook_url: "https://multica.test/api/webhooks/gitlab/integ-2",
      webhook_secret: "glws_revealed",
    });
    renderTab();

    await userEvent.click(screen.getByRole("button", { name: enSettings.gitlab.register_button }));
    await userEvent.type(screen.getByPlaceholderText("gitlab.com"), "gitlab.example");
    await userEvent.type(screen.getByPlaceholderText("group/subproject"), "acme/new-project");
    await userEvent.type(screen.getByPlaceholderText("12345678"), "99");

    // Two buttons share the "Register a project" label (header CTA + dialog
    // submit) — the dialog's own submit button is the last one rendered.
    const submitButtons = screen.getAllByRole("button", { name: enSettings.gitlab.register_button });
    await userEvent.click(submitButtons[submitButtons.length - 1]!);

    await waitFor(() =>
      expect(mockCreate).toHaveBeenCalledWith("workspace-1", {
        gitlab_host: "gitlab.example",
        gitlab_project_path: "acme/new-project",
        gitlab_project_id: 99,
      }),
    );
    expect(await screen.findByText("https://multica.test/api/webhooks/gitlab/integ-2")).toBeInTheDocument();
    expect(await screen.findByText("glws_revealed")).toBeInTheDocument();
  });

  it("removes a project after confirmation", async () => {
    listingRef.current = {
      integrations: [
        {
          id: "integ-1",
          workspace_id: "workspace-1",
          gitlab_host: "gitlab.example",
          gitlab_project_id: 42,
          gitlab_project_path: "acme/widget",
          created_at: "2026-01-01T00:00:00Z",
        },
      ],
    };
    mockDelete.mockResolvedValue(undefined);
    renderTab();

    await userEvent.click(screen.getByRole("button", { name: enSettings.gitlab.remove }));
    await userEvent.click(screen.getByRole("button", { name: enSettings.gitlab.remove_confirm_action }));

    await waitFor(() => expect(mockDelete).toHaveBeenCalledWith("workspace-1", "integ-1"));
  });
});
