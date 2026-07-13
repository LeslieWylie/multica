"use client";

import { useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import { Copy, Trash2 } from "lucide-react";
import { Button } from "@multica/ui/components/ui/button";
import { Card, CardContent } from "@multica/ui/components/ui/card";
import { Input } from "@multica/ui/components/ui/input";
import { Label } from "@multica/ui/components/ui/label";
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
import {
  Dialog,
  DialogContent,
  DialogHeader,
  DialogTitle,
  DialogDescription,
  DialogFooter,
} from "@multica/ui/components/ui/dialog";
import { useAuthStore } from "@multica/core/auth";
import { useWorkspaceId } from "@multica/core/hooks";
import { memberListOptions } from "@multica/core/workspace/queries";
import { gitlabIntegrationsOptions, gitlabKeys } from "@multica/core/gitlab";
import { api, ApiError } from "@multica/core/api";
import type { GitLabIntegrationResponse } from "@multica/core/types";
import { useT } from "../../i18n";
import { GitLabMark } from "./gitlab-mark";

// GitLabTab is the workspace settings panel for GitLab MR ↔ issue
// integrations. Structured like OctoTab: a workspace registers one
// integration PER GitLab project it wants MR sync for (there's no App/
// installation concept on GitLab's side to discover projects automatically),
// each with its own webhook URL + secret. Listing is member-visible;
// register/remove are admin-only (the backend enforces it via
// RequireWorkspaceRole; the UI hides the actions to match).
export function GitLabTab() {
  const { t } = useT("settings");
  const wsId = useWorkspaceId();
  const qc = useQueryClient();
  const user = useAuthStore((s) => s.user);

  const { data: listing, isLoading } = useQuery({
    ...gitlabIntegrationsOptions(wsId),
    enabled: !!wsId,
  });
  const { data: members = [] } = useQuery({
    ...memberListOptions(wsId),
    enabled: !!wsId,
  });

  const isAdmin = members.some(
    (m) => m.user_id === user?.id && (m.role === "owner" || m.role === "admin"),
  );

  const integrations = listing?.integrations ?? [];

  const [configureOpen, setConfigureOpen] = useState(false);
  const [deleteTarget, setDeleteTarget] = useState<GitLabIntegrationResponse | null>(null);
  const [deleting, setDeleting] = useState(false);
  // The webhook URL + secret are only ever returned by the create call, the
  // instant the plaintext secret is known — same "shown once" pattern as
  // webhook subscriptions' signing secret (see webhook-dialogs.tsx).
  const [created, setCreated] = useState<{ url: string; secret: string } | null>(null);

  const refresh = () => {
    if (wsId) qc.invalidateQueries({ queryKey: gitlabKeys.integrations(wsId) });
  };

  async function copyText(text: string) {
    try {
      await navigator.clipboard.writeText(text);
      toast.success(t(($) => $.gitlab.toast_copied));
    } catch {
      toast.error(t(($) => $.gitlab.toast_copy_failed));
    }
  }

  return (
    <div className="space-y-4">
      <div className="flex items-center justify-between">
        <div>
          <h3 className="text-sm font-medium">{t(($) => $.gitlab.section_title)}</h3>
          <p className="text-sm text-muted-foreground">{t(($) => $.gitlab.description)}</p>
        </div>
        {isAdmin && (
          <Button size="sm" onClick={() => setConfigureOpen(true)}>
            {t(($) => $.gitlab.register_button)}
          </Button>
        )}
      </div>

      {isLoading && (
        <Card>
          <CardContent className="py-6 text-sm text-muted-foreground">
            {t(($) => $.gitlab.loading)}
          </CardContent>
        </Card>
      )}

      {!isLoading && integrations.length === 0 && (
        <Card>
          <CardContent className="py-6 text-sm text-muted-foreground">
            {isAdmin
              ? t(($) => $.gitlab.empty)
              : t(($) => $.gitlab.member_not_configured_hint)}
          </CardContent>
        </Card>
      )}

      {integrations.map((integ) => (
        <Card key={integ.id}>
          <CardContent className="flex items-center justify-between gap-4 py-4">
            <div className="flex min-w-0 items-center gap-3">
              <GitLabMark className="h-5 w-5 shrink-0 text-muted-foreground" />
              <div className="min-w-0">
                <div className="truncate text-sm font-medium">{integ.gitlab_project_path}</div>
                <div className="truncate text-xs text-muted-foreground">{integ.gitlab_host}</div>
              </div>
            </div>
            {isAdmin && (
              <Button
                size="icon"
                variant="ghost"
                aria-label={t(($) => $.gitlab.remove)}
                onClick={() => setDeleteTarget(integ)}
              >
                <Trash2 className="h-4 w-4" />
              </Button>
            )}
          </CardContent>
        </Card>
      ))}

      <ConfigureDialog
        open={configureOpen}
        onOpenChange={setConfigureOpen}
        wsId={wsId}
        onConfigured={(url, secret) => {
          setConfigureOpen(false);
          setCreated({ url, secret });
          refresh();
        }}
      />

      {/* Webhook-URL-and-secret-shown-once dialog */}
      <AlertDialog
        open={!!created}
        onOpenChange={(v) => {
          if (!v) setCreated(null);
        }}
      >
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>{t(($) => $.gitlab.webhook_dialog_title)}</AlertDialogTitle>
            <AlertDialogDescription>
              {t(($) => $.gitlab.webhook_dialog_description)}
            </AlertDialogDescription>
          </AlertDialogHeader>
          {created && (
            <div className="space-y-3">
              <div className="space-y-1">
                <Label className="text-xs">{t(($) => $.gitlab.webhook_dialog_url_label)}</Label>
                <div className="flex items-center gap-2">
                  <code className="min-w-0 flex-1 truncate rounded bg-muted px-2 py-1.5 text-xs">
                    {created.url}
                  </code>
                  <Button
                    variant="outline"
                    size="icon"
                    onClick={() => copyText(created.url)}
                  >
                    <Copy className="h-4 w-4" />
                  </Button>
                </div>
              </div>
              <div className="space-y-1">
                <Label className="text-xs">{t(($) => $.gitlab.webhook_dialog_secret_label)}</Label>
                <div className="flex items-center gap-2">
                  <code className="min-w-0 flex-1 truncate rounded bg-muted px-2 py-1.5 text-xs">
                    {created.secret}
                  </code>
                  <Button
                    variant="outline"
                    size="icon"
                    onClick={() => copyText(created.secret)}
                  >
                    <Copy className="h-4 w-4" />
                  </Button>
                </div>
              </div>
              <p className="text-xs text-muted-foreground">
                {t(($) => $.gitlab.webhook_dialog_secret_hint)}
              </p>
            </div>
          )}
          <AlertDialogFooter>
            <AlertDialogAction onClick={() => setCreated(null)}>
              {t(($) => $.gitlab.webhook_dialog_done)}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>

      {/* Remove confirmation */}
      <AlertDialog open={!!deleteTarget} onOpenChange={(o) => !o && setDeleteTarget(null)}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>{t(($) => $.gitlab.remove_confirm_title)}</AlertDialogTitle>
            <AlertDialogDescription>
              {t(($) => $.gitlab.remove_confirm_description)}
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel disabled={deleting}>
              {t(($) => $.gitlab.remove_confirm_cancel)}
            </AlertDialogCancel>
            <AlertDialogAction
              disabled={deleting}
              onClick={async () => {
                const target = deleteTarget;
                if (!target || !wsId) return;
                setDeleting(true);
                try {
                  await api.deleteGitLabIntegration(wsId, target.id);
                  toast.success(t(($) => $.gitlab.removed));
                  setDeleteTarget(null);
                  refresh();
                } catch (err) {
                  toast.error(
                    err instanceof ApiError ? err.message : t(($) => $.gitlab.remove_failed),
                  );
                } finally {
                  setDeleting(false);
                }
              }}
            >
              {deleting ? t(($) => $.gitlab.removing) : t(($) => $.gitlab.remove_confirm_action)}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}

function ConfigureDialog({
  open,
  onOpenChange,
  wsId,
  onConfigured,
}: {
  open: boolean;
  onOpenChange: (o: boolean) => void;
  wsId: string;
  onConfigured: (webhookUrl: string, webhookSecret: string) => void;
}) {
  const { t } = useT("settings");
  const [host, setHost] = useState("");
  const [projectPath, setProjectPath] = useState("");
  const [projectId, setProjectId] = useState("");
  const [submitting, setSubmitting] = useState(false);

  const parsedProjectId = Number(projectId);
  const canSubmit =
    host.trim() !== "" &&
    projectPath.trim() !== "" &&
    projectId.trim() !== "" &&
    Number.isInteger(parsedProjectId) &&
    parsedProjectId > 0;

  const submit = async () => {
    if (!canSubmit) return;
    setSubmitting(true);
    try {
      const resp = await api.createGitLabIntegration(wsId, {
        gitlab_host: host.trim(),
        gitlab_project_path: projectPath.trim(),
        gitlab_project_id: parsedProjectId,
      });
      toast.success(t(($) => $.gitlab.registered));
      setHost("");
      setProjectPath("");
      setProjectId("");
      if (resp.webhook_url && resp.webhook_secret) {
        onConfigured(resp.webhook_url, resp.webhook_secret);
      }
    } catch (err) {
      toast.error(err instanceof ApiError ? err.message : t(($) => $.gitlab.register_failed));
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>{t(($) => $.gitlab.register_button)}</DialogTitle>
          <DialogDescription>{t(($) => $.gitlab.register_desc)}</DialogDescription>
        </DialogHeader>
        <div className="space-y-3">
          <div className="space-y-1">
            <label className="text-xs font-medium">{t(($) => $.gitlab.host_label)}</label>
            <Input
              value={host}
              onChange={(e) => setHost(e.target.value)}
              placeholder={t(($) => $.gitlab.host_placeholder)}
            />
          </div>
          <div className="space-y-1">
            <label className="text-xs font-medium">{t(($) => $.gitlab.project_path_label)}</label>
            <Input
              value={projectPath}
              onChange={(e) => setProjectPath(e.target.value)}
              placeholder={t(($) => $.gitlab.project_path_placeholder)}
            />
          </div>
          <div className="space-y-1">
            <label className="text-xs font-medium">{t(($) => $.gitlab.project_id_label)}</label>
            <Input
              type="number"
              min={1}
              value={projectId}
              onChange={(e) => setProjectId(e.target.value)}
              placeholder={t(($) => $.gitlab.project_id_placeholder)}
            />
            <p className="text-xs text-muted-foreground">
              {t(($) => $.gitlab.project_id_hint)}
            </p>
          </div>
        </div>
        <DialogFooter>
          <Button variant="ghost" onClick={() => onOpenChange(false)}>
            {t(($) => $.gitlab.cancel)}
          </Button>
          <Button disabled={submitting || !canSubmit} onClick={submit}>
            {submitting ? t(($) => $.gitlab.registering) : t(($) => $.gitlab.register_button)}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
