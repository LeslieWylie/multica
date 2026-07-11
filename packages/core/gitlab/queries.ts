import { queryOptions } from "@tanstack/react-query";
import { api } from "../api";

export const gitlabKeys = {
  all: (wsId: string) => ["gitlab", wsId] as const,
  integration: (wsId: string) => [...gitlabKeys.all(wsId), "integration"] as const,
};

export const gitlabIntegrationOptions = (wsId: string) =>
  queryOptions({
    queryKey: gitlabKeys.integration(wsId),
    queryFn: () => api.getGitLabIntegration(wsId),
    enabled: !!wsId,
  });
