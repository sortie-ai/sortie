import type { StateResponse } from "@/api/types.gen";

export type Condition = "degraded" | "needs" | "held" | "clear";

export const ACKNOWLEDGED_WORD = "acknowledged";

const SCM_PROVIDER_NAMES: Record<string, string> = {
  github: "GitHub",
  gitlab: "GitLab",
  gitea: "Gitea",
};

export function scmProviderName(provider: string): string {
  return SCM_PROVIDER_NAMES[provider] ?? provider;
}

export function failingChecks(state: StateResponse): string[] {
  const { health } = state;
  if (health === undefined) {
    return [];
  }
  const checks = [
    { name: "Database", check: health.database },
    { name: "Dispatch preflight", check: health.preflight },
    { name: health.workflow.file ?? "Workflow file", check: health.workflow },
    { name: "Tracker poll", check: health.poll },
    { name: health.scm === null ? "" : scmProviderName(health.scm.provider), check: health.scm },
  ];
  return checks
    .filter(({ check }) => check !== null && check.status === "fail")
    .map(({ name }) => name);
}

export function acknowledgedCount(state: StateResponse): number {
  return (state.attention ?? []).filter((item) => item.acknowledged_at !== null).length;
}

/** A failing check outranks unacknowledged attention items, which outrank acknowledged ones that still stand, which outrank all clear. */
export function condition(state: StateResponse): Condition {
  if (failingChecks(state).length > 0) {
    return "degraded";
  }
  if (state.counts.attention > 0) {
    return "needs";
  }
  return acknowledgedCount(state) > 0 ? "held" : "clear";
}

export function conditionWord(c: Condition): string {
  switch (c) {
    case "degraded":
      return "Degraded";
    case "needs":
      return "Needs attention";
    case "held":
      return "Waiting on a person";
    case "clear":
      return "All clear";
  }
}
