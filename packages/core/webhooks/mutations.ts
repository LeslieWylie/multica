import { useMutation, useQueryClient } from "@tanstack/react-query";
import { api } from "../api";
import { webhookKeys } from "./queries";
import { useWorkspaceId } from "../hooks";
import type {
  CreateWebhookSubscriptionRequest,
  ListWebhookSubscriptionsResponse,
  UpdateWebhookSubscriptionRequest,
} from "../types";

// projectId scopes invalidation to the right list (workspace vs project). It is
// the same value passed to webhookSubscriptionsOptions on the calling surface.
export function useCreateWebhookSubscription(projectId?: string) {
  const qc = useQueryClient();
  const wsId = useWorkspaceId();
  return useMutation({
    mutationFn: (data: CreateWebhookSubscriptionRequest) =>
      api.createWebhookSubscription(data),
    onSettled: () => {
      qc.invalidateQueries({ queryKey: webhookKeys.list(wsId, projectId) });
    },
  });
}

export function useUpdateWebhookSubscription(projectId?: string) {
  const qc = useQueryClient();
  const wsId = useWorkspaceId();
  const key = webhookKeys.list(wsId, projectId);
  return useMutation({
    mutationFn: ({ id, ...data }: { id: string } & UpdateWebhookSubscriptionRequest) =>
      api.updateWebhookSubscription(id, data),
    // Optimistic patch (CLAUDE.md: "patch locally, send request, roll back on
    // failure, invalidate on settle"). Without this, toggling two event
    // checkboxes back-to-back computes each PATCH's `events` array from the
    // subscription object still in the query cache — which for the second
    // click is a stale pre-first-click snapshot while the first PATCH is still
    // in flight, so the second request can silently overwrite the first
    // click's change. Patching the cache synchronously in onMutate means the
    // component re-renders with the toggled state before the next click can
    // read stale data.
    onMutate: async ({ id, ...data }) => {
      await qc.cancelQueries({ queryKey: key });
      const previous = qc.getQueryData<ListWebhookSubscriptionsResponse>(key);
      if (previous) {
        qc.setQueryData<ListWebhookSubscriptionsResponse>(key, {
          ...previous,
          subscriptions: previous.subscriptions.map((s) =>
            s.id === id ? { ...s, ...data } : s,
          ),
        });
      }
      return { previous };
    },
    onError: (_err, _vars, context) => {
      if (context?.previous) {
        qc.setQueryData(key, context.previous);
      }
    },
    onSettled: () => {
      qc.invalidateQueries({ queryKey: key });
    },
  });
}

export function useDeleteWebhookSubscription(projectId?: string) {
  const qc = useQueryClient();
  const wsId = useWorkspaceId();
  return useMutation({
    mutationFn: (id: string) => api.deleteWebhookSubscription(id),
    onSettled: () => {
      qc.invalidateQueries({ queryKey: webhookKeys.list(wsId, projectId) });
    },
  });
}

// useRedeliverWebhookSubscriptionDelivery re-POSTs a stored payload. The server
// enqueues it async (202) and records the new row only once the worker
// finishes, so invalidating once on settle can race ahead of that write. We
// invalidate immediately and again after a short delay so the new delivery row
// surfaces without a manual reload.
export function useRedeliverWebhookSubscriptionDelivery(subscriptionId: string) {
  const qc = useQueryClient();
  const wsId = useWorkspaceId();
  return useMutation({
    mutationFn: (deliveryId: string) =>
      api.redeliverWebhookSubscriptionDelivery(subscriptionId, deliveryId),
    onSettled: () => {
      const key = webhookKeys.deliveries(wsId, subscriptionId);
      qc.invalidateQueries({ queryKey: key });
      // The row is written after the 202 (and a failing endpoint retries for a
      // few seconds), so refetch again shortly to catch the recorded row.
      setTimeout(() => {
        void qc.invalidateQueries({ queryKey: key });
      }, 3000);
    },
  });
}
