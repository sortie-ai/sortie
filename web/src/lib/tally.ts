import { NO_COST, statsCostFigure, statsTokensFigure } from "@/lib/figures";
import { fmtCount, plural } from "@/lib/format";

import type { Summary } from "@/api/types.gen";
import type { Figure } from "@/lib/figures";

/** A part of the tally: words around an optional figure that may carry a note. */
export interface TallyPart {
  lead: string;
  figure: Figure | undefined;
  tail: string;
}

/** The cost is apart so the screen can place it after the counts. */
export interface TallyLine {
  parts: TallyPart[];
  cost: TallyPart | undefined;
}

// The tally reads a report that records cost, so the base tier never applies.
const RECORDED_TIER = "full";

function words(text: string): TallyPart {
  return { lead: text, figure: undefined, tail: "" };
}

export function tallyLine(summary: Summary, endedCount: number, hasCost: boolean): TallyLine {
  if (summary.runs === 0) {
    return { parts: [words("No run finished today.")], cost: undefined };
  }
  const parts = [
    words(`${plural(endedCount, "issue", "issues")} ended`),
    words(`${plural(summary.runs, "run", "runs")} finished`),
    words(`${fmtCount(summary.succeeded)} succeeded`),
    words(`${fmtCount(summary.failed)} failed`),
  ];
  if (summary.stopped > 0) {
    parts.push(words(`${fmtCount(summary.stopped)} stopped`));
  }
  if (summary.cancelled > 0) {
    parts.push(words(`${fmtCount(summary.cancelled)} cancelled`));
  }
  parts.push(tokensPart(summary));
  return { parts, cost: hasCost ? costPart(summary) : undefined };
}

function tokensPart(summary: Summary): TallyPart {
  const figure = statsTokensFigure(summary, RECORDED_TIER);
  if (summary.tokens === null) {
    return { lead: "tokens ", figure, tail: "" };
  }
  return { lead: "", figure, tail: summary.tokens.uncached === 1 ? " token" : " tokens" };
}

function costPart(summary: Summary): TallyPart | undefined {
  const figure = statsCostFigure(summary, RECORDED_TIER);
  if (figure.text === NO_COST) {
    return undefined;
  }
  return summary.cost_usd === null
    ? { lead: "cost ", figure, tail: "" }
    : { lead: "", figure, tail: " estimated" };
}
