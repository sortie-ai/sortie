import { fmtClock, parseInstant, upperFirst } from "@/lib/format";

import type {
  IssueDetailResponse,
  IssueOutcomeResponse,
  PrLoopStationResponse,
  RunResponse,
  RunsResponse,
  StateResponse,
} from "@/api/types.gen";

/** The tracker state the issue reached, as the operator configured it with a capital first letter, or Merged. */
export function endingWord(outcome: IssueOutcomeResponse): string {
  switch (outcome.kind) {
    case "merged":
      return "Merged";
    case "handed_off":
    case "terminal":
      return outcome.state === undefined || outcome.state === ""
        ? "Ended"
        : upperFirst(outcome.state);
    default:
      return "Ended";
  }
}

export function hasSeveralTerminalStates(
  terminalStates: readonly string[] | null | undefined,
): boolean {
  return (terminalStates ?? []).length >= 2;
}

/** Gives the clock time an issue's closing line carries: when its ending was observed once it ended, else now. */
export function closingClock(
  detail: Pick<IssueDetailResponse, "status" | "history">,
  now: number,
): string {
  const ending = detail.status === "history" ? detail.history?.issue_outcome : null;
  return fmtClock(ending ? ending.observed_at : new Date(now));
}

/** An issue whose newest run today ended with an outcome and which Sortie no longer holds. */
export interface EndedIssue {
  issueId: string;
  identifier: string;
  displayKey: string;
  title: string | null;
  rule: string | null;
  agentKind: string | null;
  outcome: IssueOutcomeResponse;
  /** What each pull request reaction recorded in the pass of the issue's newest run. */
  prLoop: Record<string, PrLoopStationResponse> | null;
}

function activeIssueIds(state: StateResponse): Set<string> {
  const lists = [
    state.running,
    state.retrying,
    state.budget_exhausted,
    state.parked,
    state.pending_reactions,
    state.between_stages,
    state.held,
  ];
  return new Set(lists.flatMap((list) => (list ?? []).map((entry) => entry.issue_id)));
}

/** Runs are served newest first; the newest run of each issue that has an outcome and is in no row list of the state, newest ending first. */
export function buildEnded(pages: RunsResponse[], state: StateResponse): EndedIssue[] {
  const active = activeIssueIds(state);
  const newest = new Map<string, RunResponse>();
  for (const run of pages.flatMap((page) => page.runs ?? [])) {
    if (!newest.has(run.issue_id)) {
      newest.set(run.issue_id, run);
    }
  }
  const ended: EndedIssue[] = [];
  for (const run of newest.values()) {
    if (run.issue_outcome !== null && !active.has(run.issue_id)) {
      ended.push({
        issueId: run.issue_id,
        identifier: run.issue_identifier,
        displayKey: run.display_identifier || run.issue_identifier,
        title: run.issue_title || null,
        rule: run.rule_name && run.rule_name !== "default" ? run.rule_name : null,
        agentKind: run.agent_kind || null,
        outcome: run.issue_outcome,
        prLoop: run.pr_loop,
      });
    }
  }
  const observed = (issue: EndedIssue): number => parseInstant(issue.outcome.observed_at) ?? 0;
  return ended.sort((a, b) => observed(b) - observed(a));
}
