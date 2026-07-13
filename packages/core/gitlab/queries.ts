import { queryOptions } from "@tanstack/react-query";
import { api } from "../api";

export const gitlabKeys = {
  all: (wsId: string) => ["gitlab", wsId] as const,
  integrations: (wsId: string) => [...gitlabKeys.all(wsId), "integrations"] as const,
};

export const gitlabIntegrationsOptions = (wsId: string) =>
  queryOptions({
    queryKey: gitlabKeys.integrations(wsId),
    queryFn: () => api.listGitLabIntegrations(wsId),
    enabled: !!wsId,
  });
