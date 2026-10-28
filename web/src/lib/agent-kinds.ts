import type { ConfigResponse } from "@/api/types.gen";

export function usageByKind(config: ConfigResponse): [string, string][] {
  const byKind = new Map<string, string>([[config.agent.kind, config.agent.usage_arrival]]);
  const { dispatch } = config;
  if (dispatch.default !== null) {
    byKind.set(dispatch.default.agent_kind, dispatch.default.usage_arrival);
  }
  for (const rule of dispatch.rules ?? []) {
    byKind.set(rule.agent_kind, rule.usage_arrival);
  }
  return [...byKind].sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0));
}
