"use client";

import { useState } from "react";
import { History, Plus, Trash2, Webhook } from "lucide-react";
import { Button } from "@multica/ui/components/ui/button";
import { Card, CardContent } from "@multica/ui/components/ui/card";
import { Input } from "@multica/ui/components/ui/input";
import { Label } from "@multica/ui/components/ui/label";
import { Switch } from "@multica/ui/components/ui/switch";
import { Badge } from "@multica/ui/components/ui/badge";
import { Checkbox } from "@multica/ui/components/ui/checkbox";
import type { TFunction } from "i18next";
import type { WebhookSubscriptionEvent } from "@multica/core/types";
import { WEBHOOK_SUBSCRIPTION_EVENTS } from "@multica/core/types";
import { useT } from "../../i18n";
import { useWebhookSection } from "./use-webhook-section";
import { WebhookDialogs } from "./webhook-dialogs";
import { WebhookSubscriptionDeliveriesDialog } from "../../webhooks/components/webhook-subscription-deliveries-dialog";

// Resolves a per-event description string. A plain switch (rather than
// indexing $.webhooks.event_descriptions[event]) so every branch stays a
// static selector the i18next typegen/extraction tooling can see — dynamic
// bracket access on the selector API isn't a pattern used elsewhere in this
// codebase and isn't worth being the first.
function eventDescription(
  t: TFunction<"settings">,
  event: WebhookSubscriptionEvent,
): string {
  switch (event) {
    case "issue.status_changed":
      return t(($) => $.webhooks.event_descriptions.issue_status_changed);
    case "issue.assignee_changed":
      return t(($) => $.webhooks.event_descriptions.issue_assignee_changed);
    case "comment.created":
      return t(($) => $.webhooks.event_descriptions.comment_created);
  }
}

// Workspace-level outbound webhooks (Settings → Webhooks tab). Logic lives in
// useWebhookSection; this component owns only the settings-card layout.
export function WebhooksSection() {
  const { t } = useT("settings");
  const wh = useWebhookSection();
  // Subscription id whose delivery history dialog is open, or null.
  const [historyTarget, setHistoryTarget] = useState<string | null>(null);
  // Subscription id whose event checklist is expanded for editing, or null.
  // Collapsed by default — the badge row already shows what's subscribed;
  // this is only for changing it.
  const [editingEventsFor, setEditingEventsFor] = useState<string | null>(
    null,
  );

  if (!wh.canManage) {
    return (
      <p className="text-sm text-muted-foreground">
        {t(($) => $.webhooks.admin_only)}
      </p>
    );
  }

  return (
    <div className="space-y-6">
      <section className="space-y-1">
        <p className="text-sm text-muted-foreground">
          {t(($) => $.webhooks.description_workspace)}
        </p>
      </section>

      {/* Add form */}
      <Card>
        <CardContent className="space-y-3">
          <Label htmlFor="webhook-url" className="text-sm font-medium">
            {t(($) => $.webhooks.add_label)}
          </Label>
          <div className="flex items-center gap-2">
            <Input
              id="webhook-url"
              type="url"
              placeholder="https://example.com/webhooks/multica"
              value={wh.newUrl}
              onChange={(e) => wh.setNewUrl(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === "Enter") wh.handleCreate();
              }}
            />
            <Button
              onClick={wh.handleCreate}
              disabled={
                !wh.newUrl.trim() || wh.newEvents.length === 0 || wh.isCreating
              }
            >
              <Plus className="h-4 w-4" />
              {t(($) => $.webhooks.add_button)}
            </Button>
          </div>

          {/* Event checklist — every subscription must send at least one
              event, so the Add button above is disabled while this is
              empty rather than letting the request 400. */}
          <div className="space-y-1.5">
            <p className="text-xs font-medium text-muted-foreground">
              {t(($) => $.webhooks.events_label)}
            </p>
            <div className="flex flex-col gap-1.5">
              {WEBHOOK_SUBSCRIPTION_EVENTS.map((event) => (
                <label
                  key={event}
                  className="flex items-center gap-2 text-sm"
                >
                  <Checkbox
                    checked={wh.newEvents.includes(event)}
                    onCheckedChange={(checked) =>
                      wh.toggleNewEvent(event, checked === true)
                    }
                  />
                  <code className="text-xs">{event}</code>
                  <span className="text-xs text-muted-foreground">
                    {eventDescription(t, event)}
                  </span>
                </label>
              ))}
            </div>
          </div>

          <p className="text-xs text-muted-foreground">
            {t(($) => $.webhooks.signature_hint)}
          </p>
        </CardContent>
      </Card>

      {/* List */}
      {wh.subscriptions.length === 0 ? (
        <div className="flex flex-col items-center gap-2 rounded-md border border-dashed p-8 text-center">
          <Webhook className="h-6 w-6 text-muted-foreground" />
          <p className="text-sm text-muted-foreground">
            {t(($) => $.webhooks.empty)}
          </p>
        </div>
      ) : (
        <div className="space-y-2">
          {wh.subscriptions.map((sub) => (
            <Card key={sub.id}>
              <CardContent className="space-y-2 py-3">
                <div className="flex items-center justify-between gap-4">
                  <div className="min-w-0 space-y-1">
                    <p className="truncate text-sm font-medium" title={sub.url}>
                      {sub.url}
                    </p>
                    <div className="flex flex-wrap items-center gap-1.5">
                      {sub.events.map((ev) => (
                        <Badge
                          key={ev}
                          variant="secondary"
                          className="text-[10px]"
                        >
                          {ev}
                        </Badge>
                      ))}
                      {sub.secret_hint && (
                        <span className="text-xs text-muted-foreground">
                          {t(($) => $.webhooks.secret_hint, {
                            hint: sub.secret_hint,
                          })}
                        </span>
                      )}
                    </div>
                  </div>
                  <div className="flex shrink-0 items-center gap-3">
                    <Button
                      variant="ghost"
                      size="sm"
                      className="text-xs"
                      onClick={() =>
                        setEditingEventsFor(
                          editingEventsFor === sub.id ? null : sub.id,
                        )
                      }
                    >
                      {t(($) => $.webhooks.edit_events_button)}
                    </Button>
                    <Switch
                      checked={sub.enabled}
                      onCheckedChange={(v) => wh.handleToggle(sub, v)}
                      aria-label={t(($) => $.webhooks.toggle_label)}
                    />
                    <Button
                      variant="ghost"
                      size="icon"
                      onClick={() => setHistoryTarget(sub.id)}
                      aria-label={t(($) => $.webhooks.deliveries.history_button)}
                    >
                      <History className="h-4 w-4" />
                    </Button>
                    <Button
                      variant="ghost"
                      size="icon"
                      onClick={() => wh.setDeleteTarget(sub)}
                      aria-label={t(($) => $.webhooks.delete_label)}
                    >
                      <Trash2 className="h-4 w-4" />
                    </Button>
                  </div>
                </div>

                {editingEventsFor === sub.id && (
                  <div className="flex flex-col gap-1.5 border-t pt-2">
                    {WEBHOOK_SUBSCRIPTION_EVENTS.map((event) => {
                      const checked = sub.events.includes(event);
                      // Disable unchecking the last remaining event — the
                      // server rejects an empty list, and handleToggleEvent
                      // already no-ops on it, but disabling here gives the
                      // user a visual reason instead of a silent no-op.
                      const isLastChecked = checked && sub.events.length === 1;
                      // Also disable while an events PATCH for this
                      // subscription is already in flight — see
                      // isEventUpdatePending's doc comment in
                      // use-webhook-section.ts for why this closes the
                      // out-of-order-completion race between two rapid
                      // toggles.
                      const disabled = isLastChecked || wh.isEventUpdatePending(sub.id);
                      return (
                        <label
                          key={event}
                          className="flex items-center gap-2 text-sm"
                        >
                          <Checkbox
                            checked={checked}
                            disabled={disabled}
                            onCheckedChange={(v) =>
                              wh.handleToggleEvent(sub, event, v === true)
                            }
                          />
                          <code className="text-xs">{event}</code>
                        </label>
                      );
                    })}
                  </div>
                )}
              </CardContent>
            </Card>
          ))}
        </div>
      )}

      <WebhookDialogs
        createdSecret={wh.createdSecret}
        setCreatedSecret={wh.setCreatedSecret}
        copySecret={wh.copySecret}
        deleteTarget={wh.deleteTarget}
        setDeleteTarget={wh.setDeleteTarget}
        handleDelete={wh.handleDelete}
        isDeleting={wh.isDeleting}
      />

      {historyTarget && (
        <WebhookSubscriptionDeliveriesDialog
          subscriptionId={historyTarget}
          open={historyTarget !== null}
          onOpenChange={(open) => {
            if (!open) setHistoryTarget(null);
          }}
        />
      )}
    </div>
  );
}
