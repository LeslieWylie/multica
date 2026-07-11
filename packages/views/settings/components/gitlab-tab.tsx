"use client";

import { useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import { Copy, RefreshCw } from "lucide-react";
import { Button } from "@multica/ui/components/ui/button";
import { Card, CardContent } from "@multica/ui/components/ui/card";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@multica/ui/components/ui/alert-dialog";
import { useAuthStore } from "@multica/core/auth";
import { useWorkspaceId } from "@multica/core/hooks";
import { memberListOptions } from "@multica/core/workspace/queries";
import { gitlabIntegrationOptions, gitlabKeys } from "@multica/core/gitlab";
import { api, ApiError } from "@multica/core/api";
import { useT } from "../../i18n";
import { GitLabMark } from "./gitlab-mark";

// GitLabTab is the workspace settings panel for the GitLab MR ↔ issue
// integration (see server/internal/handler/gitlab.go). Unlike GitHub's App
// installation flow, GitLab has no OAuth/discovery step: the admin copies a
// webhook URL from here and pastes it into the GitLab project's
// Settings → Webhooks page, with "Merge request events" checked. There is
// deliberately no repository list or connection status beyond
// configured/not-configured — GitLab has no equivalent of an App
// installation to enumerate.
export function GitLabTab() {
  const { t } = useT("settings");
  const wsId = useWorkspaceId();
  const qc = useQueryClient();
  const user = useAuthStore((s) => s.user);

  const { data: members = [] } = useQuery({
    ...memberListOptions(wsId),
    enabled: !!wsId,
  });
  const canManage = members.some(
    (m) => m.user_id === user?.id && (m.role === "owner" || m.role === "admin"),
  );

  const { data, isLoading } = useQuery({
    ...gitlabIntegrationOptions(wsId),
    enabled: !!wsId,
  });
  const configured = data?.configured === true;

  const [rotating, setRotating] = useState(false);
  const [confirmRotateOpen, setConfirmRotateOpen] = useState(false);
  // The webhook URL is only ever returned by the rotate call, the instant the
  // plaintext token is known — same "shown once" pattern as webhook
  // subscriptions' signing secret (see webhook-dialogs.tsx). Held in local
  // state rather than query cache so it disappears once the dialog closes.
  const [webhookUrl, setWebhookUrl] = useState<string | null>(null);

  async function copyUrl(url: string) {
    try {
      await navigator.clipboard.writeText(url);
      toast.success(t(($) => $.gitlab.toast_copied));
    } catch {
      toast.error(t(($) => $.gitlab.toast_copy_failed));
    }
  }

  async function handleRotate() {
    if (!wsId || rotating) return;
    setRotating(true);
    try {
      const resp = await api.rotateGitLabIntegration(wsId);
      setWebhookUrl(resp.webhook_url ?? null);
      qc.setQueryData(gitlabKeys.integration(wsId), resp);
      toast.success(t(($) => $.gitlab.toast_rotated));
    } catch (e) {
      toast.error(e instanceof ApiError ? e.message : t(($) => $.gitlab.toast_rotate_failed));
    } finally {
      setRotating(false);
      setConfirmRotateOpen(false);
    }
  }

  if (!wsId) return null;

  return (
    <div className="space-y-4">
      <div>
        <h3 className="text-sm font-medium">{t(($) => $.gitlab.section_title)}</h3>
        <p className="text-sm text-muted-foreground">{t(($) => $.gitlab.description)}</p>
      </div>

      <Card>
        <CardContent className="space-y-4">
          <div className="flex items-start justify-between gap-4">
            <div className="flex items-start gap-3">
              <GitLabMark className="h-6 w-6 mt-0.5 shrink-0 text-muted-foreground" />
              <div className="space-y-1">
                <p className="text-sm font-medium">{t(($) => $.gitlab.connection_title)}</p>
                {isLoading ? (
                  <p className="text-xs text-muted-foreground">{t(($) => $.gitlab.loading)}</p>
                ) : configured ? (
                  <p className="text-xs text-muted-foreground">
                    {t(($) => $.gitlab.configured_description)}
                  </p>
                ) : canManage ? (
                  <p className="text-xs text-muted-foreground">
                    {t(($) => $.gitlab.not_configured_description)}
                  </p>
                ) : (
                  <p className="text-xs text-muted-foreground">
                    {t(($) => $.gitlab.contact_admin_to_connect)}
                  </p>
                )}
              </div>
            </div>
            {canManage && (
              <Button
                variant={configured ? "outline" : "default"}
                size="sm"
                onClick={() => (configured ? setConfirmRotateOpen(true) : handleRotate())}
                disabled={rotating}
              >
                <RefreshCw className="h-3.5 w-3.5" />
                {rotating
                  ? t(($) => $.gitlab.rotating)
                  : configured
                    ? t(($) => $.gitlab.rotate_button)
                    : t(($) => $.gitlab.generate_button)}
              </Button>
            )}
          </div>

          {!canManage && !configured && (
            <p className="text-xs text-muted-foreground">
              {t(($) => $.gitlab.member_not_configured_hint)}
            </p>
          )}
        </CardContent>
      </Card>

      {/* Webhook-URL-shown-once dialog */}
      <AlertDialog
        open={!!webhookUrl}
        onOpenChange={(v) => {
          if (!v) setWebhookUrl(null);
        }}
      >
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>{t(($) => $.gitlab.webhook_dialog_title)}</AlertDialogTitle>
            <AlertDialogDescription>
              {t(($) => $.gitlab.webhook_dialog_description)}
            </AlertDialogDescription>
          </AlertDialogHeader>
          {webhookUrl && (
            <div className="flex items-center gap-2">
              <code className="min-w-0 flex-1 truncate rounded bg-muted px-2 py-1.5 text-xs">
                {webhookUrl}
              </code>
              <Button variant="outline" size="icon" onClick={() => copyUrl(webhookUrl)}>
                <Copy className="h-4 w-4" />
              </Button>
            </div>
          )}
          <p className="text-xs text-muted-foreground">
            {t(($) => $.gitlab.webhook_dialog_secret_hint)}
          </p>
          <AlertDialogFooter>
            <AlertDialogAction onClick={() => setWebhookUrl(null)}>
              {t(($) => $.gitlab.webhook_dialog_done)}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>

      {/* Rotate confirmation — rotating invalidates the previous URL
          immediately, so an admin who forgot to update the GitLab project's
          webhook config would silently stop receiving events. */}
      <AlertDialog open={confirmRotateOpen} onOpenChange={setConfirmRotateOpen}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>{t(($) => $.gitlab.rotate_confirm_title)}</AlertDialogTitle>
            <AlertDialogDescription>
              {t(($) => $.gitlab.rotate_confirm_description)}
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel disabled={rotating}>
              {t(($) => $.gitlab.rotate_confirm_cancel)}
            </AlertDialogCancel>
            <AlertDialogAction onClick={handleRotate} disabled={rotating}>
              {t(($) => $.gitlab.rotate_confirm_action)}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}
