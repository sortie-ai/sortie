import { statsCostText, statsCostTitle } from "@/lib/figures";
import { fmtCount, fmtTokens, plural } from "@/lib/format";

import type { Summary } from "@/api/types.gen";

/** The cost is apart so it can carry its own hover. */
export interface TallyLine {
  parts: string[];
  cost: string | undefined;
}

// The tally reads a report that records cost, so the base tier never applies.
const RECORDED_TIER = "full";

export function tallyLine(summary: Summary, endedCount: number, hasCost: boolean): TallyLine {
  if (summary.runs === 0) {
    return { parts: ["No run finished today."], cost: undefined };
  }
  const parts = [
    `${plural(endedCount, "issue", "issues")} ended`,
    `${plural(summary.runs, "run", "runs")} finished`,
    `${fmtCount(summary.succeeded)} succeeded`,
    `${fmtCount(summary.failed)} failed`,
  ];
  if (summary.stopped > 0) {
    parts.push(`${fmtCount(summary.stopped)} stopped`);
  }
  if (summary.cancelled > 0) {
    parts.push(`${fmtCount(summary.cancelled)} cancelled`);
  }
  parts.push(tokensPart(summary));
  return { parts, cost: hasCost ? costPart(summary) : undefined };
}

export function tallyCostTitle(summary: Summary, hasCost: boolean): string | undefined {
  return hasCost && summary.cost_usd !== null ? statsCostTitle(summary, RECORDED_TIER) : undefined;
}

function tokensPart(summary: Summary): string {
  if (summary.tokens_unmeasured_runs === summary.runs) {
    return "tokens not measured";
  }
  if (summary.tokens === null) {
    return "tokens not recorded";
  }
  const { uncached } = summary.tokens;
  const text = `${fmtTokens(uncached)} ${uncached === 1 ? "token" : "tokens"}`;
  return summary.tokens_unmeasured_runs > 0
    ? `${text} (${plural(summary.tokens_unmeasured_runs, "run", "runs")} not measured)`
    : text;
}

function costPart(summary: Summary): string {
  if (summary.cost_usd === null) {
    return "cost unpriced";
  }
  const text = `${statsCostText(summary, RECORDED_TIER)} estimated`;
  return summary.cost_unpriced_runs > 0
    ? `${text} (${plural(summary.cost_unpriced_runs, "run", "runs")} unpriced)`
    : text;
}
