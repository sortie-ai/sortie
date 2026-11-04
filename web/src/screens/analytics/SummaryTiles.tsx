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
import { fmtCount, fmtDuration, fmtMean, fmtPercent, plural } from "@/lib/format";

import type { Report, Summary } from "@/api/types.gen";
import type { Figure } from "@/lib/figures";
import type { Units } from "@/lib/units";
import type { ReactNode } from "react";

interface TileProps {
  label: string;
  value: Figure;
  hint?: string | null;
}

const TRIM = "trim-caps *:trim-caps";

// Spans two rows of the shared figure grid so a wrapped label keeps the labels and figures level.
// The grid sets no row gap, because a subgrid inherits it; the label margin spaces the rows.
function Tile({ label, value, hint }: TileProps) {
  return (
    <div className="row-span-2 grid min-w-0 grid-rows-subgrid">
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
    </div>
  );
}

interface RateProps {
  summary: Summary;
}

// Spans both rows of the figure grid, so it starts with the labels and ends on the figures' baseline.
function Rate({ summary }: RateProps) {
  return (
    <div className="col-span-full row-span-2 grid grid-flow-col grid-rows-subgrid justify-start gap-x-sm max-xl:mb-md xl:col-auto">
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
      />
    </div>
  );
}

export interface SummaryTilesProps {
  report: Report;
  /** Which figures the tiles after the run counts show. */
  units: Units;
}

export function SummaryTiles({ report, units }: SummaryTilesProps) {
  const { summary, schema_tier: tier } = report;
  const hasSucceeded = summary.succeeded > 0;
  const tiles: ReactNode[] = [
    <Tile key="runs" label="Runs" value={{ text: fmtCount(summary.runs) }} />,
    <Tile key="issues" label="Issues" value={{ text: fmtCount(summary.issues) }} />,
  ];
  if (hasSucceeded) {
    tiles.push(
      <Tile
        key="duration"
        label="Duration"
        value={{ text: fmtDuration(summary.duration_seconds.p50) }}
        hint="Median duration of a succeeded run."
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
      />,
    );
  }
  if (units === "tokens") {
    const isEveryRunMeasured =
      tier === "full" &&
      summary.tokens_unmeasured_runs === 0 &&
      summary.tokens_lower_bound_runs === 0;
    const tokens = tier === "base" ? null : summary.tokens;
    if (tokens !== null && tokens.cache_read > 0) {
      const isExact = !summary.tokens_lower_bound;
      tiles.push(
        <Tile
          key="cache-write"
          label="Cache write"
          value={liveTokensFigure(tokens.cache_write, isExact)}
        />,
        <Tile
          key="cache-read"
          label="Cache read"
          value={liveTokensFigure(tokens.cache_read, isExact)}
        />,
      );
    }
    const rate = tokens === null ? null : cacheHitRate(tokens.input, tokens.cache_read);
    if (rate !== null) {
      tiles.push(
        <Tile
          key="cache-hit-rate"
          label={LABEL_CACHE_HIT_RATE}
          value={{ text: fmtPercent(rate) }}
          hint={CACHE_HIT_RATE_HINT}
        />,
      );
    }
    tiles.push(
      <Tile
        key="tokens"
        label="Tokens"
        value={statsTokensFigure(summary, tier)}
        hint={isEveryRunMeasured ? TOKENS_TILE_CAPTION : null}
      />,
    );
  } else {
    tiles.push(<Tile key="cost" label="Estimated cost" value={statsCostFigure(summary, tier)} />);
    if (hasSucceeded) {
      tiles.push(
        <Tile
          key="cost-per-succeeded"
          label="Cost per run"
          hint={COST_PER_RUN_TILE_CAPTION}
          value={statsCostPerSucceededFigure(summary, tier)}
        />,
      );
    }
  }

  return (
    <div className="-mt-lg grid grid-cols-2 gap-x-lg md:grid-cols-3 xl:auto-cols-auto xl:grid-flow-col xl:grid-cols-none xl:grid-rows-[repeat(2,auto)] xl:justify-between">
      <Rate summary={summary} />
      {tiles}
    </div>
  );
}
