"use client";

import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { toast } from "sonner";
import { useAuthStore } from "@multica/core/auth";
import { useWorkspaceId } from "@multica/core/hooks";
import { memberListOptions } from "@multica/core/workspace/queries";
import { webhookSubscriptionsOptions } from "@multica/core/webhooks/queries";
import {
  useCreateWebhookSubscription,
  useDeleteWebhookSubscription,
  useUpdateWebhookSubscription,
} from "@multica/core/webhooks/mutations";
import type {
  WebhookSubscription,
  WebhookSubscriptionEvent,
} from "@multica/core/types";
import { useT } from "../../i18n";

// useWebhookSection owns all state + actions shared by the workspace-level
// settings tab and the project-level sidebar section. The two surfaces differ
// only in layout (and the workspace section fetches eagerly while the project
// section gates on `enabled`), so the logic lives here once and each component
// renders its own chrome. Toast copy uses the `settings` i18n namespace.
export interface UseWebhookSectionResult {
  canManage: boolean;
  subscriptions: WebhookSubscription[];
  newUrl: string;
  setNewUrl: (v: string) => void;
  // Events checked in the create form. Defaults to just issue.status_changed
  // — matching the server's own default when `events` is omitted entirely
  // (see webhook_subscription.go's `if len(req.Events) == 0`) — so a
  // subscription created without touching the checklist behaves exactly like
  // the old URL-only form did. Upgrading must not silently widen what a
  // freshly created subscription receives (e.g. opting every new webhook
  // into full comment bodies by default); widening requires the user to
  // explicitly check the additional boxes.
  newEvents: WebhookSubscriptionEvent[];
  toggleNewEvent: (event: WebhookSubscriptionEvent, checked: boolean) => void;
  createdSecret: string | null;
  setCreatedSecret: (v: string | null) => void;
  deleteTarget: WebhookSubscription | null;
  setDeleteTarget: (v: WebhookSubscription | null) => void;
  isCreating: boolean;
  isDeleting: boolean;
  handleCreate: () => Promise<void>;
  handleToggle: (sub: WebhookSubscription, enabled: boolean) => Promise<void>;
  // Flips one event on/off for an existing subscription. Refuses to submit an
  // empty event list (mirrors the server's reject-on-empty validation) by
  // silently ignoring the toggle that would empty it — the checkbox itself
  // stays checked so the user sees why nothing happened rather than landing
  // in a subscription that receives nothing.
  handleToggleEvent: (
    sub: WebhookSubscription,
    event: WebhookSubscriptionEvent,
    checked: boolean,
  ) => Promise<void>;
  // True while an events PATCH for this specific subscription is in flight.
  // Callers disable that subscription's event checkboxes while true — the
  // full events array is replaced wholesale on each PATCH, so two in-flight
  // requests for the same subscription could complete out of send-order and
  // have the earlier-sent (smaller) array clobber the later one at the DB.
  // Blocking a second toggle until the first settles means at most one
  // request per subscription is ever in flight, so there is nothing left to
  // race.
  isEventUpdatePending: (subId: string) => boolean;
  handleDelete: () => Promise<void>;
  copySecret: (secret: string) => Promise<void>;
}

export function useWebhookSection(
  // projectId scopes the subscription list + create to one project (project-
  // level webhook). Omit for workspace-level.
  projectId?: string,
  // enabled lets a surface defer the query until visible (the sidebar section
  // gates on its open/collapsed state). Defaults to always-on.
  enabled = true,
): UseWebhookSectionResult {
  const { t } = useT("settings");
  const wsId = useWorkspaceId();
  const user = useAuthStore((s) => s.user);

  const { data: members = [] } = useQuery(memberListOptions(wsId));
  const currentMember = members.find((m) => m.user_id === user?.id) ?? null;
  const canManage =
    currentMember?.role === "owner" || currentMember?.role === "admin";

  const { data: subscriptions = [] } = useQuery({
    ...webhookSubscriptionsOptions(wsId, projectId),
    enabled: !!wsId && canManage && enabled,
  });

  const createMutation = useCreateWebhookSubscription(projectId);
  const updateMutation = useUpdateWebhookSubscription(projectId);
  const deleteMutation = useDeleteWebhookSubscription(projectId);

  const [newUrl, setNewUrl] = useState("");
  // Defaults to just issue.status_changed, matching the server's own default
  // for an omitted `events` field — see the interface doc comment above for
  // why this must not default to "select everything."
  const [newEvents, setNewEvents] = useState<WebhookSubscriptionEvent[]>([
    "issue.status_changed",
  ]);
  // The signing secret is returned once on create; surfaced in a dialog so the
  // operator can copy it before it becomes unreachable.
  const [createdSecret, setCreatedSecret] = useState<string | null>(null);
  const [deleteTarget, setDeleteTarget] = useState<WebhookSubscription | null>(
    null,
  );

  function toggleNewEvent(event: WebhookSubscriptionEvent, checked: boolean) {
    setNewEvents((prev) =>
      checked ? [...prev, event] : prev.filter((e) => e !== event),
    );
  }

  async function handleCreate() {
    const url = newUrl.trim();
    if (!url) return;
    // Belt-and-suspenders: the Add button is already disabled when
    // newEvents is empty (see webhooks-section.tsx), but guard here too so
    // a future caller of this hook can't submit a request the server would
    // 400 on anyway.
    if (newEvents.length === 0) return;
    try {
      const created = await createMutation.mutateAsync({
        url,
        project_id: projectId ?? null,
        events: newEvents,
      });
      setNewUrl("");
      setNewEvents(["issue.status_changed"]);
      if (created.secret) setCreatedSecret(created.secret);
      toast.success(t(($) => $.webhooks.toast_created));
    } catch (e) {
      toast.error(
        e instanceof Error ? e.message : t(($) => $.webhooks.toast_create_failed),
      );
    }
  }

  async function handleToggle(sub: WebhookSubscription, enabled_: boolean) {
    try {
      await updateMutation.mutateAsync({ id: sub.id, enabled: enabled_ });
    } catch (e) {
      toast.error(
        e instanceof Error ? e.message : t(($) => $.webhooks.toast_update_failed),
      );
    }
  }

  async function handleToggleEvent(
    sub: WebhookSubscription,
    event: WebhookSubscriptionEvent,
    checked: boolean,
  ) {
    const next = checked
      ? [...sub.events, event]
      : sub.events.filter((e) => e !== event);
    // See the interface doc comment: an empty result is refused client-side
    // (matches the server's own reject-on-empty-events validation) so the UI
    // never sends a request that would 400, and the checkbox visually snaps
    // back rather than appearing to succeed.
    if (next.length === 0) return;
    try {
      await updateMutation.mutateAsync({ id: sub.id, events: next });
    } catch (e) {
      toast.error(
        e instanceof Error ? e.message : t(($) => $.webhooks.toast_update_failed),
      );
    }
  }

  function isEventUpdatePending(subId: string): boolean {
    return updateMutation.isPending && updateMutation.variables?.id === subId;
  }

  async function handleDelete() {
    if (!deleteTarget) return;
    try {
      await deleteMutation.mutateAsync(deleteTarget.id);
      setDeleteTarget(null);
      toast.success(t(($) => $.webhooks.toast_deleted));
    } catch (e) {
      toast.error(
        e instanceof Error ? e.message : t(($) => $.webhooks.toast_delete_failed),
      );
    }
  }

  async function copySecret(secret: string) {
    try {
      await navigator.clipboard.writeText(secret);
      toast.success(t(($) => $.webhooks.toast_secret_copied));
    } catch {
      toast.error(t(($) => $.webhooks.toast_copy_failed));
    }
  }

  return {
    canManage,
    subscriptions,
    newUrl,
    setNewUrl,
    newEvents,
    toggleNewEvent,
    createdSecret,
    setCreatedSecret,
    deleteTarget,
    setDeleteTarget,
    isCreating: createMutation.isPending,
    isDeleting: deleteMutation.isPending,
    handleCreate,
    handleToggle,
    handleToggleEvent,
    isEventUpdatePending,
    handleDelete,
    copySecret,
  };
}
