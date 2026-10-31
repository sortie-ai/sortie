import {
  fmtMoney,
  fmtMoneyCut,
  fmtMoneyExact,
  fmtTokens,
  fmtTokensCut,
  fmtVendorAmount,
  upperFirst,
} from "@/lib/format";

import type {
  Group,
  HistoryTotalsResponse,
  IssueDetailResponse,
  ModelUsageResponse,
  RunningEntryResponse,
  RunResponse,
  UnitAmountResponse,
  VendorAmountResponse,
} from "@/api/types.gen";
import type { CostFigure } from "@/lib/budget";

/** A spend text and the note a reader can open on it. */
export interface Figure {
  text: string;
  note?: string;
}

export type CostFigures = Pick<
  Group,
  | "cost_usd"
  | "tokens_unmeasured_runs"
  | "cost_unpriced_runs"
  | "cost_partial_runs"
  | "cost_lower_bound"
  | "tokens_lower_bound"
  | "unmeasured_cause"
  | "unmeasured_units"
>;

export type StatsFigures = CostFigures &
  Pick<Group, "tokens" | "succeeded" | "cost_per_succeeded_run_usd">;

export const NOT_MEASURED = "not measured";
export const UNPRICED_TEXT = "unpriced";
export const NOT_AVAILABLE = "N/A";
export const NOT_RECORDED = "not recorded";
export const NO_COST = "none";
const UNKNOWN_FIGURE = "unknown";
const BASE_TIER = "base";
const UNKNOWN_MODEL = "";

export const TOKENS_TILE_CAPTION = "every run measured";
export const COST_PER_RUN_TILE_CAPTION = "Estimated cost divided by the runs that succeeded.";

export const LABEL_TOKENS = "Tokens";
export const LABEL_EST_COST = "Est. cost";
export const LABEL_MEASURED_FROM = "Measured from";
export const LABEL_AGENT_REPORTED = "Agent reported";
export const LABEL_PRICED_FROM = "Priced from";
export const LABEL_MODEL_NOT_REPORTED = "Model not reported";

export const UNMEASURED_ENDED_EARLY =
  "The run ended before the agent reported any usage. It will not be measured later, and nothing needs doing on your side.";
export const UNMEASURED_NO_USAGE =
  "The agent sent no usage for this run. It will not be measured later, and nothing needs doing on your side.";
export const UNMEASURED_MIXED =
  "These runs have no token figure, for more than one reason. Each session says why.";
export const PROVENANCE_UNKNOWN =
  "Sortie recorded this run before it kept where a figure came from.";
export const RUN_TOKENS_LOWER_BOUND =
  "Some turns were not fully counted, so the real figure is higher. It will not be corrected later.";
export const RUN_COST_LOWER_BOUND =
  "Some turns were not fully counted, so the real cost is higher. It will not be corrected later.";
export const SESSION_COST_LOWER_BOUND =
  "Some turns were not fully counted, so the real cost is higher.";
export const TOKENS_LOWER_BOUND =
  "Some runs were not measured or not fully counted, so the real figure is higher.";
export const COST_LOWER_BOUND =
  "Some runs could not be fully priced or measured, so the real cost is higher.";
export const STATS_UNPRICED = "The configured rates leave these runs unpriced.";
export const VENDOR_LOWER_BOUND =
  "Some turns were not fully counted, so the real amount is higher. It will not be corrected later.";
export const CHART_ESTIMATED = "Estimated cost of the runs that finished in this period.";
export const CHART_UNKNOWN = "Cost unknown. The runs could not be priced or measured.";

const SOURCE_WORDS = new Map([
  ["protocol", "protocol"],
  ["output", "output"],
  ["journal", "session files"],
]);
const STRATEGY_WORDS = new Map([
  ["live", "live"],
  ["records", "saved"],
]);
const PRICE_SOURCE_LABELS = new Map([
  ["operator", "Custom prices"],
  ["fetched", "Downloaded prices"],
  ["embedded", "Built-in prices"],
  ["agent", "Agent-reported"],
]);
const CLASS_WORDS = new Map([
  ["input", "input"],
  ["output", "output"],
  ["cache_read", "cache reads"],
  ["cache_write", "cache writes"],
]);
const SHORT_UNITS = new Map([
  ["credits", "cr"],
  ["AI credits", "AI cr"],
  ["premium requests", "req"],
]);

function listed(items: readonly string[]): string {
  return items.join(" and ");
}

function worded(map: ReadonlyMap<string, string>, key: string): string {
  return map.get(key) ?? key;
}

function joinNotes(...notes: readonly (string | undefined)[]): string | undefined {
  const present = notes.filter((note) => note !== undefined);
  return present.length === 0 ? undefined : present.join(" ");
}

export function UNMEASURED_AMOUNTS_ONLY(units: readonly string[]): string {
  return `The agent reports its spend only in ${listed(units)}, not in tokens. Nothing to do on your side.`;
}

export function MEASURED_FROM(phrases: readonly string[]): string {
  return `Measured from ${listed(phrases)}.`;
}

export function CACHE_READS(tokens: string): string {
  return `${tokens} cache reads.`;
}

export function EXACT(usd: number): string {
  return `${fmtMoneyExact(usd)}.`;
}

export function PARTIAL_PRICE(model: string, classes: readonly string[]): string {
  const words = classes.map((name) => worded(CLASS_WORDS, name));
  return `${model} has no price for ${listed(words)}, so the real cost is higher. A price you add counts for future runs only.`;
}

export function UNPRICED(model: string): string {
  return `No price table lists ${model}, so this run has no cost. A price you add counts for future runs only.`;
}

export function AGENT_PART(share: number, models: readonly string[]): string {
  return `${fmtMoneyExact(share)} of it is the amount the agent reported for ${listed(models)}, which no price table lists.`;
}

export function AGENT_SHARE(total: number, share: number, models: readonly string[]): string {
  return `${fmtMoneyExact(total)}. ${AGENT_PART(share, models)}`;
}

export function REPORTED_BY(amounts: string, agent: string): string {
  return `${amounts} reported by ${agent}.`;
}

function isNameable(models: readonly string[]): boolean {
  return models.length > 0 && !models.includes(UNKNOWN_MODEL);
}

function unnamedAgentPart(
  share: number,
  models: readonly string[],
  agent?: string,
): string | undefined {
  return models.includes(UNKNOWN_MODEL) && agent !== undefined
    ? REPORTED_BY(fmtMoneyExact(share), agent)
    : undefined;
}

function agentPartNote(
  share: number,
  models: readonly string[],
  agent?: string,
): string | undefined {
  return isNameable(models) ? AGENT_PART(share, models) : unnamedAgentPart(share, models, agent);
}

function agentShareNote(
  total: number,
  share: number,
  models: readonly string[],
  agent?: string,
): string {
  return isNameable(models)
    ? AGENT_SHARE(total, share, models)
    : (joinNotes(EXACT(total), unnamedAgentPart(share, models, agent)) ?? EXACT(total));
}

function causeOf(cause: string | null | undefined, units: readonly string[] | null | undefined) {
  return cause === "amounts_only" ? UNMEASURED_AMOUNTS_ONLY(units ?? []) : undefined;
}

/** The note on a figure that sums several runs; a cause that names one run reads nothing. */
function aggregateCause(
  cause: string | null | undefined,
  units: readonly string[] | null | undefined,
): string | undefined {
  return cause === "mixed" ? UNMEASURED_MIXED : causeOf(cause, units);
}

function runCause(run: RunResponse): string | undefined {
  switch (run.unmeasured_cause) {
    case "ended_early":
      return UNMEASURED_ENDED_EARLY;
    case "no_usage":
      return UNMEASURED_NO_USAGE;
    case "unknown":
      return PROVENANCE_UNKNOWN;
    default:
      return causeOf(run.unmeasured_cause, run.unmeasured_units);
  }
}

function sourcePhrases(run: RunResponse): string[] | null {
  return run.usage_provenance === null
    ? null
    : run.usage_provenance.map(
        (pair) => `${worded(STRATEGY_WORDS, pair.strategy)} ${worded(SOURCE_WORDS, pair.source)}`,
      );
}

function runSource(run: RunResponse): string | undefined {
  const phrases = sourcePhrases(run);
  if (phrases === null) {
    return PROVENANCE_UNKNOWN;
  }
  return phrases.length === 0 ? undefined : MEASURED_FROM(phrases);
}

interface AgentShare {
  usd: number;
  models: string[];
}

function agentShareOf(parts: readonly ModelUsageResponse[]): AgentShare | undefined {
  const priced = parts.filter((part) => part.price_source === "agent");
  if (priced.length === 0) {
    return undefined;
  }
  return {
    usd: priced.reduce((sum, part) => sum + (part.estimated_cost_usd ?? 0), 0),
    models: [...new Set(priced.map((part) => part.model))],
  };
}

function nonEmpty(value: string | undefined): string | undefined {
  return value === undefined || value === "" ? undefined : value;
}

function runModel(run: RunResponse): string | undefined {
  return nonEmpty(run.model_name) ?? nonEmpty(run.configured_model);
}

function partialPriceNote(model: string, classes: readonly string[] | null): string | undefined {
  return model !== UNKNOWN_MODEL && classes !== null && classes.length > 0
    ? PARTIAL_PRICE(model, classes)
    : undefined;
}

/** Words the token figure of a run; a missing figure reads N/A with its cause. */
export function runTokensFigure(run: RunResponse): Figure {
  const tokens = run.uncached_tokens;
  if (run.usage_completeness === "unmeasured" || tokens === null) {
    return { text: NOT_AVAILABLE, note: runCause(run) };
  }
  if (run.usage_completeness === "lower_bound") {
    return {
      text: fmtTokensCut(tokens),
      note: joinNotes(RUN_TOKENS_LOWER_BOUND, runSource(run)),
    };
  }
  const cacheReads = runCacheReadsText(run);
  return {
    text: fmtTokens(tokens),
    note: joinNotes(runSource(run), cacheReads === null ? undefined : CACHE_READS(cacheReads)),
  };
}

/** Words the cost of a run in US dollars alone; vendor amounts are not folded in. */
export function runBaseCostFigure(run: RunResponse): Figure {
  const usd = run.estimated_cost_usd;
  const parts = run.model_usage ?? [];
  const share = agentShareOf(parts);
  switch (run.cost_status) {
    case "estimated":
      if (usd === null) {
        return { text: UNKNOWN_FIGURE };
      }
      return {
        text: fmtMoney(usd),
        note:
          share === undefined
            ? undefined
            : agentShareNote(usd, share.usd, share.models, run.agent_kind),
      };
    case "partial": {
      if (usd === null) {
        return { text: UNKNOWN_FIGURE };
      }
      const priceGap =
        run.usage_completeness === "lower_bound"
          ? undefined
          : parts
              .map((part) => partialPriceNote(part.model, part.unrated_classes))
              .find((note) => note !== undefined);
      return {
        text: fmtMoneyCut(usd),
        note: joinNotes(
          priceGap ?? RUN_COST_LOWER_BOUND,
          share === undefined ? undefined : agentPartNote(share.usd, share.models, run.agent_kind),
        ),
      };
    }
    case "unpriced": {
      const model = runModel(run);
      return { text: UNPRICED_TEXT, note: model === undefined ? undefined : UNPRICED(model) };
    }
    case "unmeasured":
      return { text: NOT_AVAILABLE, note: runCause(run) };
    default:
      return { text: UNKNOWN_FIGURE };
  }
}

interface FoldedAmount {
  unit: string;
  amount: number;
  lowerBound: boolean;
}

function foldByUnit(amounts: readonly VendorAmountResponse[]): FoldedAmount[] {
  const folded = new Map<string, FoldedAmount>();
  for (const entry of amounts) {
    const current = folded.get(entry.unit);
    folded.set(entry.unit, {
      unit: entry.unit,
      amount: (current?.amount ?? 0) + entry.amount,
      lowerBound: (current?.lowerBound ?? false) || entry.lower_bound,
    });
  }
  return [...folded.values()];
}

function amountText(entry: FoldedAmount, unit: "full" | "short"): string {
  const word = unit === "full" ? entry.unit : worded(SHORT_UNITS, entry.unit);
  return `${fmtVendorAmount(entry.amount, entry.lowerBound)} ${word}`;
}

/** Words the cost cell of a run: its dollar figure, or its vendor amounts when it has no dollar figure. */
export function runCostFigure(run: RunResponse): Figure {
  const base = runBaseCostFigure(run);
  const folded = foldByUnit(run.vendor_amounts ?? []);
  if (folded.length === 0) {
    return base;
  }
  const reportedBy =
    run.agent_kind === undefined
      ? undefined
      : REPORTED_BY(listed(folded.map((entry) => amountText(entry, "full"))), run.agent_kind);
  const lowerBound = folded.some((entry) => entry.lowerBound) ? VENDOR_LOWER_BOUND : undefined;
  if (base.text !== NOT_AVAILABLE) {
    const usd = run.estimated_cost_usd;
    return {
      text: base.text,
      note: joinNotes(base.note ?? (usd === null ? undefined : EXACT(usd)), reportedBy, lowerBound),
    };
  }
  return {
    text: folded.map((entry) => amountText(entry, "short")).join(", "),
    note: joinNotes(reportedBy, lowerBound, base.note),
  };
}

/** Words the estimated cost of one model part of a run. */
export function partCostFigure(part: ModelUsageResponse): Figure {
  const usd = part.estimated_cost_usd;
  switch (part.cost_status) {
    case "estimated":
      return { text: usd === null ? UNKNOWN_FIGURE : fmtMoney(usd) };
    case "partial":
      return usd === null
        ? { text: UNKNOWN_FIGURE }
        : {
            text: fmtMoneyCut(usd),
            note: partialPriceNote(part.model, part.unrated_classes) ?? RUN_COST_LOWER_BOUND,
          };
    case "unpriced":
      return { text: UNPRICED_TEXT };
    default:
      return { text: UNKNOWN_FIGURE };
  }
}

/** One phrase per line, null when the run was recorded before provenance was kept. */
export function provenanceLines(run: RunResponse): string[] | null {
  return sourcePhrases(run)?.map(upperFirst) ?? null;
}

/** Words where a part's price came from; null for a part with no amount. */
export function priceSourceFigure(part: ModelUsageResponse): Figure | null {
  if (part.estimated_cost_usd === null) {
    return null;
  }
  return part.price_source === null
    ? { text: NOT_RECORDED, note: PROVENANCE_UNKNOWN }
    : { text: worded(PRICE_SOURCE_LABELS, part.price_source) };
}

/** Words an amount in a vendor unit; a lower bound is cut and noted. */
export function vendorAmountFigure(
  a: UnitAmountResponse | VendorAmountResponse,
  unit: "full" | "short",
): Figure {
  return {
    text: amountText({ unit: a.unit, amount: a.amount, lowerBound: a.lower_bound }, unit),
    note: a.lower_bound ? VENDOR_LOWER_BOUND : undefined,
  };
}

/** Words the issue's token figure from its totals; its surrounding words belong to the screen. */
export function totalsTokensFigure(t: HistoryTotalsResponse): Figure {
  if (t.uncached_tokens === null) {
    return { text: NOT_AVAILABLE, note: aggregateCause(t.unmeasured_cause, t.unmeasured_units) };
  }
  return t.tokens_complete
    ? { text: fmtTokens(t.uncached_tokens) }
    : { text: fmtTokensCut(t.uncached_tokens), note: TOKENS_LOWER_BOUND };
}

/** Words the issue's cost from its totals; its surrounding words belong to the screen. */
export function totalsCostFigure(t: HistoryTotalsResponse): Figure {
  const usd = t.estimated_cost_usd;
  const share = t.agent_priced_usd;
  const models = t.agent_priced_models ?? [];
  switch (t.cost_status) {
    case "estimated":
      if (usd === null) {
        return { text: UNKNOWN_FIGURE };
      }
      return {
        text: fmtMoney(usd),
        note: share === null ? undefined : agentShareNote(usd, share, models),
      };
    case "partial":
      if (usd === null) {
        return { text: UNKNOWN_FIGURE };
      }
      return {
        text: fmtMoneyCut(usd),
        note: joinNotes(
          COST_LOWER_BOUND,
          share === null ? undefined : agentPartNote(share, models),
        ),
      };
    case "unpriced":
      return { text: UNPRICED_TEXT, note: STATS_UNPRICED };
    case "unmeasured":
      return { text: NOT_AVAILABLE, note: aggregateCause(t.unmeasured_cause, t.unmeasured_units) };
    default:
      return { text: UNKNOWN_FIGURE };
  }
}

/** Words the token figure of a report group or summary; a `base` tier records none. */
export function statsTokensFigure(f: StatsFigures, tier: string): Figure {
  if (tier === BASE_TIER) {
    return { text: NOT_RECORDED };
  }
  if (f.tokens === null) {
    return f.tokens_unmeasured_runs > 0
      ? { text: NOT_AVAILABLE, note: aggregateCause(f.unmeasured_cause, f.unmeasured_units) }
      : { text: NOT_RECORDED };
  }
  return f.tokens_lower_bound
    ? { text: fmtTokensCut(f.tokens.uncached), note: TOKENS_LOWER_BOUND }
    : { text: fmtTokens(f.tokens.uncached) };
}

/** Words the cost of a report group or summary; a `base` tier records none. */
export function statsCostFigure(f: CostFigures, tier: string): Figure {
  if (tier === BASE_TIER) {
    return { text: NOT_RECORDED };
  }
  if (f.cost_usd !== null) {
    return f.cost_lower_bound
      ? { text: fmtMoneyCut(f.cost_usd), note: COST_LOWER_BOUND }
      : { text: fmtMoney(f.cost_usd) };
  }
  if (f.cost_unpriced_runs > 0) {
    return { text: UNPRICED_TEXT, note: STATS_UNPRICED };
  }
  // A group of recorded CI results holds no measurable run, so it has no cost to state.
  return f.tokens_unmeasured_runs > 0
    ? { text: NOT_AVAILABLE, note: aggregateCause(f.unmeasured_cause, f.unmeasured_units) }
    : { text: NO_COST };
}

/** Words the cost per succeeded run of a report group or summary. */
export function statsCostPerSucceededFigure(f: StatsFigures, tier: string): Figure {
  if (tier === BASE_TIER) {
    return { text: NOT_RECORDED };
  }
  const value = f.cost_per_succeeded_run_usd;
  if (value !== null) {
    return f.cost_lower_bound
      ? { text: fmtMoneyCut(value), note: COST_LOWER_BOUND }
      : { text: fmtMoney(value) };
  }
  if (f.cost_usd === null) {
    return statsCostFigure(f, tier);
  }
  // Every succeeded run is unmeasured when the group has a cost and no per-run value.
  return f.succeeded > 0
    ? { text: NOT_AVAILABLE, note: aggregateCause(f.unmeasured_cause, f.unmeasured_units) }
    : { text: NOT_AVAILABLE };
}

/** Words a money figure of a running session or of the runs of an issue or window. */
export function liveMoneyFigure(usd: number, complete: boolean, scope: "session" | "runs"): Figure {
  if (complete) {
    return { text: fmtMoney(usd) };
  }
  return {
    text: fmtMoneyCut(usd),
    note: scope === "session" ? SESSION_COST_LOWER_BOUND : COST_LOWER_BOUND,
  };
}

/** Words a token figure of the runs of an issue or window. */
export function liveTokensFigure(tokens: number, complete: boolean): Figure {
  return complete
    ? { text: fmtTokens(tokens) }
    : { text: fmtTokensCut(tokens), note: TOKENS_LOWER_BOUND };
}

/** Words the cost of a running session or of the runs of an issue by its status; a status the client does not know reads "unknown". */
export function liveCostFigure(
  status: string,
  usd: number | null,
  scope: "session" | "runs",
): Figure {
  switch (status) {
    case "estimated":
    case "partial":
      return usd === null
        ? { text: UNKNOWN_FIGURE }
        : liveMoneyFigure(usd, status === "estimated", scope);
    case "unpriced":
      return { text: UNPRICED_TEXT };
    case "unmeasured":
      return { text: NOT_MEASURED };
    default:
      return { text: UNKNOWN_FIGURE };
  }
}

/** A running issue's cost is the live figure of its runs; any other issue states its recorded totals. */
export function issueCostFigure(detail: IssueDetailResponse, cost: CostFigure): Figure {
  const isRunning = detail.status === "running" && detail.running !== null;
  return !isRunning && detail.history !== null
    ? totalsCostFigure(detail.history.totals)
    : liveCostFigure(cost.status, cost.usd, "runs");
}

export function runningTokensText(running: RunningEntryResponse): string {
  const figure = running.uncached_tokens;
  if (!running.tokens_measured || figure === null) {
    return NOT_MEASURED;
  }
  return fmtTokens(figure);
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
