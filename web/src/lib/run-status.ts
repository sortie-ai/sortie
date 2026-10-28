import type { RunResponse } from "@/api/types.gen";

export type Mark =
  | "running"
  | "current"
  | "waiting"
  | "approval"
  | "retry"
  | "held"
  | "needs"
  | "stopped"
  | "handed"
  | "succeeded"
  | "failed"
  | "degraded"
  | "clear"
  | "stalled";

/** The marks that state where work stands; `stalled` is a separate marker beside them, never a state. */
export type StateMark = Exclude<Mark, "stalled">;

const STATE_WORDS: Record<StateMark, string> = {
  running: "Running",
  current: "Current",
  waiting: "Waiting",
  approval: "Approval",
  retry: "Retry",
  held: "Held",
  needs: "Attention",
  stopped: "Stopped",
  handed: "Handed",
  succeeded: "Succeeded",
  failed: "Failed",
  degraded: "Degraded",
  clear: "Clear",
};

export function stateWord(mark: StateMark): string {
  return STATE_WORDS[mark];
}

const RUN_STATUSES: Record<string, { word: string; mark: Mark | null }> = {
  succeeded: { word: "Succeeded", mark: "succeeded" },
  failed: { word: "Failed", mark: "failed" },
  ci_failed: { word: "CI failed", mark: "failed" },
  needs_person: { word: "Needed input", mark: "stopped" },
  budget_stopped: { word: "Stopped at a budget", mark: "stopped" },
  usage_limited: { word: "Usage limit reached", mark: "stopped" },
  cancelled: { word: "Cancelled", mark: null },
};

export const RUN_STATUS_NAMES: readonly string[] = Object.keys(RUN_STATUSES);

/** A run row serves its status and no wording for it, so the client words the persisted statuses and shows any other value as served with no mark. */
export function runStatus(status: string): { word: string; mark: Mark | null } {
  return Object.hasOwn(RUN_STATUSES, status) ? RUN_STATUSES[status] : { word: status, mark: null };
}

const DECLARED_OUTCOMES: Record<string, { word: string; mark: Mark }> = {
  blocked: { word: "Action required", mark: "stopped" },
  "needs-human-review": { word: "Ready for human review", mark: "handed" },
  "no-change-needed": { word: "No change needed", mark: "succeeded" },
};

/** A succeeded run reads as the outcome its agent declared; any other status wins over a declaration, so a run whose handoff was withheld still reads as failed. */
export function runOutcome(run: Pick<RunResponse, "status" | "declared_outcome">): {
  word: string;
  mark: Mark | null;
} {
  const declared = run.declared_outcome;
  if (
    run.status === "succeeded" &&
    declared !== null &&
    Object.hasOwn(DECLARED_OUTCOMES, declared)
  ) {
    return DECLARED_OUTCOMES[declared];
  }
  return runStatus(run.status);
}

export function agentStatement(run: Pick<RunResponse, "agent_text">): string | null {
  const text = run.agent_text;
  return text === null || text.trim() === "" ? null : text;
}
