import { NotedFigure } from "@/components/NotedFigure";
import { cacheHitRate } from "@/components/TokenKinds";
import { Hint } from "@/components/ui/hint";
import { cn } from "@/lib/cn";
import {
  CACHE_HIT_RATE_HINT,
  COST_PER_RUN_TILE_CAPTION,
  LABEL_CACHE_HIT_RATE,
  TOKENS_TILE_CAPTION,
  liveTokensFigure,
  statsCostFigure,
  statsCostPerSucceededFigure,
  statsTokensFigure,
} from "@/lib/figures";
import {
  fmtChange,
  fmtCount,
  fmtDuration,
  fmtMean,
  fmtMoney,
  fmtPercent,
  fmtPoints,
  fmtTokens,
  plural,
  upperFirst,
} from "@/lib/format";

import type { Report, Summary } from "@/api/types.gen";
import type { Figure } from "@/lib/figures";
import type { Units } from "@/lib/units";
import type { ReactNode } from "react";

/** The range a summary's figures change against. */
export interface Comparison {
  /** The previous range as a sentence names it, lowercase: "previous 7 days". */
  period: string;
  /** The previous range's summary; it counts at least one run. */
  previous: Summary;
}

type Basis = Comparison | "pending" | null;

interface TileProps {
  label: string;
  value: Figure;
  hint?: string | null;
  /** The change row's content; absent, the row is drawn empty. */
  change?: ReactNode;
}

const TRIM = "trim-caps *:trim-caps";

// Holds the height of a filled change row while the previous range loads.
const PENDING_CHANGE = "\u00a0";

// Spans the shared figure grid's rows so wrapped labels, figures, and changes stay level across tiles.
// The grid sets no row gap, because a subgrid inherits it; the label margin spaces the rows.
function Tile({ label, value, hint, change }: TileProps) {
  return (
    <div className="row-span-3 grid min-w-0 grid-rows-subgrid">
      <span className={cn("mt-lg text-body-sm text-ink-muted", TRIM)}>
        {hint === undefined || hint === null ? label : <Hint hint={hint}>{label}</Hint>}
      </span>
      <span
        className={cn(
          "mt-xs self-end font-heading text-h5 font-semibold text-ink tabular-nums",
          TRIM,
        )}
      >
        <NotedFigure figure={value} />
      </span>
      <span className={cn("mt-xs text-body-sm text-ink-body tabular-nums", TRIM)}>{change}</span>
    </div>
  );
}

/** The summary values a change compares; `null` where a figure reads as not recorded, unavailable, or unpriced. */
interface Figures {
  runs: number;
  issues: number;
  duration: number | null;
  turns: number | null;
  cacheWrite: number | null;
  cacheRead: number | null;
  hitRate: number | null;
  uncached: number | null;
  cost: number | null;
  costPerRun: number | null;
}

interface Against {
  period: string;
  before: Figures;
}

function tokensOf(summary: Summary, tier: string): Summary["tokens"] {
  return tier === "base" ? null : summary.tokens;
}

function figuresOf(summary: Summary, tier: string): Figures {
  const tokens = tokensOf(summary, tier);
  const isPriced = tier !== "base";
  return {
    runs: summary.runs,
    issues: summary.issues,
    duration: summary.duration_seconds.p50,
    turns: summary.mean_turns_succeeded,
    cacheWrite: tokens?.cache_write ?? null,
    cacheRead: tokens?.cache_read ?? null,
    hitRate: tokens === null ? null : cacheHitRate(tokens.input, tokens.cache_read),
    uncached: tokens?.uncached ?? null,
    cost: isPriced ? summary.cost_usd : null,
    costPerRun: isPriced ? summary.cost_per_succeeded_run_usd : null,
  };
}

function againstOf(basis: Basis, tier: string): Against | "pending" | undefined {
  if (basis === null) {
    return undefined;
  }
  return basis === "pending"
    ? basis
    : { period: basis.period, before: figuresOf(basis.previous, tier) };
}

// The hover gives the previous figure rounded, even for a lower bound, so a change never claims a bound.
function changeOf(
  against: Against | "pending" | undefined,
  now: Figures,
  key: keyof Figures,
  formatPrevious: (value: number) => string,
  formatChange: (now: number | null, before: number | null) => string | null = fmtChange,
): ReactNode {
  if (against === "pending") {
    return PENDING_CHANGE;
  }
  if (against === undefined) {
    return undefined;
  }
  const before = against.before[key];
  const text = before === null ? null : formatChange(now[key], before);
  if (before === null || text === null) {
    return undefined;
  }
  return <Hint hint={`${upperFirst(against.period)}: ${formatPrevious(before)}`}>{text}</Hint>;
}

function rateChangeOf(summary: Summary, basis: Basis): ReactNode {
  if (basis === "pending") {
    return PENDING_CHANGE;
  }
  if (basis === null) {
    return undefined;
  }
  const text = fmtPoints(summary.success_rate, basis.previous.success_rate);
  if (text === null) {
    return undefined;
  }
  const tone = text.startsWith("+")
    ? "text-success"
    : text.startsWith("-")
      ? "text-error"
      : "text-ink-muted";
  return (
    <>
      <span className={tone}>{text}</span>
      <span className="text-ink-muted">{` on the ${basis.period}`}</span>
    </>
  );
}

interface RateProps {
  summary: Summary;
  comparison: Basis;
}

// Spans the figure grid's rows, so it starts with the labels and ends under the figures.
function Rate({ summary, comparison }: RateProps) {
  return (
    <div className="col-span-full row-span-3 grid grid-flow-col grid-rows-subgrid justify-start gap-x-sm max-xl:mb-md xl:col-auto">
      <span
        className={cn(
          "row-span-2 mt-lg self-start font-heading text-[calc(var(--text-body-sm)+var(--spacing-xxs)+var(--text-h5))] leading-none font-semibold text-ink tabular-nums [font-size-adjust:cap-height_1]",
          TRIM,
        )}
      >
        {fmtPercent(summary.success_rate)}
      </span>
      <Tile
        label={`${fmtCount(summary.succeeded)} of ${plural(summary.runs, "run", "runs")}`}
        value={{ text: "succeeded" }}
        change={rateChangeOf(summary, comparison)}
      />
    </div>
  );
}

export interface SummaryTilesProps {
  report: Report;
  /** Which figures the tiles after the run counts show. */
  units: Units;
  /** `null` leaves every change row empty; `"pending"` leaves it empty at one line's height. */
  comparison: Basis;
}

export function SummaryTiles({ report, units, comparison }: SummaryTilesProps) {
  const { summary, schema_tier: tier } = report;
  const hasSucceeded = summary.succeeded > 0;
  const now = figuresOf(summary, tier);
  const against = againstOf(comparison, tier);
  const tiles: ReactNode[] = [
    <Tile
      key="runs"
      label="Runs"
      value={{ text: fmtCount(summary.runs) }}
      change={changeOf(against, now, "runs", fmtCount)}
    />,
    <Tile
      key="issues"
      label="Issues"
      value={{ text: fmtCount(summary.issues) }}
      change={changeOf(against, now, "issues", fmtCount)}
    />,
  ];
  if (hasSucceeded) {
    tiles.push(
      <Tile
        key="duration"
        label="Duration"
        value={{ text: fmtDuration(summary.duration_seconds.p50) }}
        hint="Median duration of a succeeded run."
        change={changeOf(against, now, "duration", fmtDuration)}
      />,
      <Tile
        key="turns"
        label="Turns"
        value={{
          text:
            summary.mean_turns_succeeded === null
              ? "not recorded"
              : fmtMean(summary.mean_turns_succeeded),
        }}
        hint={
          summary.mean_turns_succeeded === null
            ? undefined
            : "Mean number of turns of a succeeded run."
        }
        change={changeOf(against, now, "turns", fmtMean)}
      />,
    );
  }
  if (units === "tokens") {
    const isEveryRunMeasured =
      tier === "full" &&
      summary.tokens_unmeasured_runs === 0 &&
      summary.tokens_lower_bound_runs === 0;
    const tokens = tokensOf(summary, tier);
    if (tokens !== null && tokens.cache_read > 0) {
      const isExact = !summary.tokens_lower_bound;
      tiles.push(
        <Tile
          key="cache-write"
          label="Cache write"
          value={liveTokensFigure(tokens.cache_write, isExact)}
          change={changeOf(against, now, "cacheWrite", fmtTokens)}
        />,
        <Tile
          key="cache-read"
          label="Cache read"
          value={liveTokensFigure(tokens.cache_read, isExact)}
          change={changeOf(against, now, "cacheRead", fmtTokens)}
        />,
      );
    }
    if (now.hitRate !== null) {
      tiles.push(
        <Tile
          key="cache-hit-rate"
          label={LABEL_CACHE_HIT_RATE}
          value={{ text: fmtPercent(now.hitRate) }}
          hint={CACHE_HIT_RATE_HINT}
          change={changeOf(against, now, "hitRate", fmtPercent, fmtPoints)}
        />,
      );
    }
    tiles.push(
      <Tile
        key="tokens"
        label="Tokens"
        value={statsTokensFigure(summary, tier)}
        hint={isEveryRunMeasured ? TOKENS_TILE_CAPTION : null}
        change={changeOf(against, now, "uncached", fmtTokens)}
      />,
    );
  } else {
    tiles.push(
      <Tile
        key="cost"
        label="Estimated cost"
        value={statsCostFigure(summary, tier)}
        change={changeOf(against, now, "cost", fmtMoney)}
      />,
    );
    if (hasSucceeded) {
      tiles.push(
        <Tile
          key="cost-per-succeeded"
          label="Cost per run"
          hint={COST_PER_RUN_TILE_CAPTION}
          value={statsCostPerSucceededFigure(summary, tier)}
          change={changeOf(against, now, "costPerRun", fmtMoney)}
        />,
      );
    }
  }

  return (
    <div className="-mt-lg grid grid-cols-2 gap-x-lg md:grid-cols-3 xl:auto-cols-auto xl:grid-flow-col xl:grid-cols-none xl:grid-rows-[repeat(3,auto)] xl:justify-between">
      <Rate summary={summary} comparison={comparison} />
      {tiles}
    </div>
  );
}
