import {
  fmtMoney,
  fmtMoneyAtLeast,
  fmtMoneyCut,
  fmtMoneyExact,
  fmtTokens,
  plural,
} from "@/lib/format";

import type {
  Group,
  HistoryTotalsResponse,
  RunningEntryResponse,
  RunResponse,
} from "@/api/types.gen";

export type CostFigures = Pick<
  Group,
  "cost_usd" | "tokens_unmeasured_runs" | "cost_unpriced_runs" | "cost_partial_runs"
>;

export type StatsFigures = CostFigures &
  Pick<Group, "tokens" | "succeeded" | "cost_per_succeeded_run_usd">;

export const NOT_MEASURED = "not measured";
export const UNPRICED = "unpriced";
const NOT_RECORDED = "not recorded";
const BASE_TIER = "base";

const LOWER_BOUND_NOTE =
  "At least this much. It leaves out runs Sortie could not price or measure.";

const LEAVES_OUT = "Leaves out spend Sortie could not price or measure";

/** A `partial` cost is a lower bound and reads "at least"; a status the client does not know reads "unknown". */
export function costText(status: string, usd: number | null): string {
  switch (status) {
    case "estimated":
      return usd === null ? "unknown" : fmtMoney(usd);
    case "partial":
      return usd === null ? "unknown" : fmtMoneyAtLeast(usd);
    case "unpriced":
      return UNPRICED;
    case "unmeasured":
      return NOT_MEASURED;
    default:
      return "unknown";
  }
}

/** For a `partial` cost, the note that it leaves out spend; undefined for a cost without an amount. */
export function costTitle(status: string, usd: number | null): string | undefined {
  if (usd === null) {
    return undefined;
  }
  switch (status) {
    case "estimated":
      return fmtMoneyExact(usd);
    case "partial":
      return `At least ${fmtMoneyExact(usd)}. ${LEAVES_OUT}.`;
    default:
      return undefined;
  }
}

export function runTokensText(run: RunResponse): string {
  if (!run.tokens_measured || run.uncached_tokens === null) {
    return NOT_MEASURED;
  }
  return fmtTokens(run.uncached_tokens);
}

export function runningTokensText(running: RunningEntryResponse): string {
  const figure = running.uncached_tokens;
  if (!running.tokens_measured || figure === null) {
    return NOT_MEASURED;
  }
  return fmtTokens(figure);
}

export function runCostText(run: RunResponse): string {
  return costText(run.cost_status, run.estimated_cost_usd);
}

export function totalsTokensText(totals: HistoryTotalsResponse): string {
  if (totals.uncached_tokens === null) {
    return NOT_MEASURED;
  }
  return fmtTokens(totals.uncached_tokens);
}

export function totalsCostText(totals: HistoryTotalsResponse): string {
  return costText(totals.cost_status, totals.estimated_cost_usd);
}

/** Words the token figure of a report group or summary; a `base` tier records none. */
export function statsTokensText(f: StatsFigures, tier: string): string {
  if (tier === BASE_TIER) {
    return NOT_RECORDED;
  }
  if (f.tokens === null) {
    return f.tokens_unmeasured_runs > 0 ? NOT_MEASURED : NOT_RECORDED;
  }
  return fmtTokens(f.tokens.uncached);
}

function cacheReadsText(cacheReads: number | null, tokensFigure: number | null): string | null {
  // A zero also means an agent that reports no cache data, and a zero is a claim.
  if (tokensFigure === null || cacheReads === null || cacheReads === 0) {
    return null;
  }
  return fmtTokens(cacheReads);
}

export function runCacheReadsText(run: RunResponse): string | null {
  return cacheReadsText(run.tokens.cache_read_tokens, run.uncached_tokens);
}

export function totalsCacheReadsText(totals: HistoryTotalsResponse): string | null {
  return cacheReadsText(totals.tokens.cache_read_tokens, totals.uncached_tokens);
}

export function statsCacheReadsText(f: StatsFigures, tier: string): string | null {
  if (tier === BASE_TIER || f.tokens === null) {
    return null;
  }
  return cacheReadsText(f.tokens.cache_read, f.tokens.uncached);
}

function lowerBound(f: CostFigures): boolean {
  return (
    f.cost_usd !== null &&
    (f.cost_unpriced_runs > 0 || f.cost_partial_runs > 0 || f.tokens_unmeasured_runs > 0)
  );
}

export function statsCostText(f: CostFigures, tier: string): string {
  if (tier === BASE_TIER) {
    return NOT_RECORDED;
  }
  if (f.cost_usd !== null) {
    return lowerBound(f) ? fmtMoneyAtLeast(f.cost_usd) : fmtMoney(f.cost_usd);
  }
  if (f.cost_unpriced_runs > 0) {
    return UNPRICED;
  }
  // A group of recorded CI results holds no measurable run, so it has no cost to state.
  return f.tokens_unmeasured_runs > 0 ? NOT_MEASURED : "none";
}

/** A lower bound reads as its cut amount alone, because the screen explains the bound on hover. */
export function statsCostFigure(f: CostFigures, tier: string): string {
  return tier !== BASE_TIER && f.cost_usd !== null && lowerBound(f)
    ? fmtMoneyCut(f.cost_usd)
    : statsCostText(f, tier);
}

export function statsTokensShortfall(f: CostFigures): string | null {
  return f.tokens_unmeasured_runs > 0
    ? plural(f.tokens_unmeasured_runs, "run not measured", "runs not measured")
    : null;
}

export function statsShortfall(f: CostFigures): string | null {
  const parts = [
    statsTokensShortfall(f),
    f.cost_unpriced_runs > 0 ? plural(f.cost_unpriced_runs, "run unpriced", "runs unpriced") : null,
    f.cost_partial_runs > 0
      ? plural(f.cost_partial_runs, "run with a partial cost", "runs with a partial cost")
      : null,
  ].filter((part) => part !== null);
  return parts.length === 0 ? null : parts.join(", ");
}

/** Its exact amount, with the runs it leaves out when it is a lower bound, or those runs alone while it has no amount; undefined on a `base` tier and when nothing is left out of a missing amount. */
export function statsCostTitle(f: CostFigures, tier: string): string | undefined {
  if (tier === BASE_TIER) {
    return undefined;
  }
  if (f.cost_usd === null) {
    return statsShortfall(f) ?? undefined;
  }
  const exact = fmtMoneyExact(f.cost_usd);
  const shortfall = statsShortfall(f);
  return shortfall === null ? exact : `At least ${exact}. ${LEAVES_OUT}: ${shortfall}.`;
}

/** A figure over a lower-bound cost reads as its cut amount alone; null while no run succeeded. */
export function statsCostPerSucceededText(f: StatsFigures, tier: string): string | null {
  if (tier === BASE_TIER) {
    return NOT_RECORDED;
  }
  if (f.succeeded === 0) {
    return null;
  }
  const value = f.cost_per_succeeded_run_usd;
  if (value !== null) {
    return lowerBound(f) ? fmtMoneyCut(value) : fmtMoney(value);
  }
  return f.cost_usd !== null ? NOT_MEASURED : statsCostText(f, tier);
}

export function statsTokensNote(f: CostFigures): string | undefined {
  return f.tokens_unmeasured_runs > 0
    ? "Leaves out runs whose tokens were not measured."
    : undefined;
}

export function statsCostNote(f: CostFigures, tier: string): string | undefined {
  if (tier === BASE_TIER) {
    return undefined;
  }
  if (f.cost_usd !== null) {
    return lowerBound(f) ? LOWER_BOUND_NOTE : undefined;
  }
  if (f.cost_unpriced_runs > 0) {
    return "The configured rates leave these runs unpriced.";
  }
  return f.tokens_unmeasured_runs > 0 ? "The agent reported no tokens for these runs." : undefined;
}

export function statsCostPerSucceededNote(f: StatsFigures, tier: string): string | undefined {
  const isLowerBound = tier !== BASE_TIER && f.cost_per_succeeded_run_usd !== null && lowerBound(f);
  return isLowerBound ? LOWER_BOUND_NOTE : undefined;
}
