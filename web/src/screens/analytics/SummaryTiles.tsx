import { NotedFigure } from "@/components/NotedFigure";
import { Hint } from "@/components/ui/hint";
import {
  COST_PER_RUN_TILE_CAPTION,
  TOKENS_TILE_CAPTION,
  statsCacheReadsText,
  statsCostFigure,
  statsCostPerSucceededFigure,
  statsTokensFigure,
} from "@/lib/figures";
import { fmtCount, fmtDuration, fmtMean, fmtPercent, plural } from "@/lib/format";

import type { Report } from "@/api/types.gen";
import type { Figure } from "@/lib/figures";
import type { ReactNode } from "react";

interface TileProps {
  label: string;
  value: Figure;
  hint?: string | null;
}

// Spans two rows of the shared figure grid so the labels and the figures of one
// visual row stay level when a label wraps. The grid sets no row gap because a
// subgrid inherits it between its own rows; the label margin spaces the visual
// rows instead.
function Tile({ label, value, hint }: TileProps) {
  return (
    <div className="row-span-2 grid min-w-0 grid-rows-subgrid">
      <span className="mt-lg text-body-sm text-ink-muted">
        {hint === undefined || hint === null ? label : <Hint hint={hint}>{label}</Hint>}
      </span>
      <span className="mt-xxs font-heading text-h5 font-semibold text-ink tabular-nums">
        <NotedFigure figure={value} />
      </span>
    </div>
  );
}

export interface SummaryTilesProps {
  report: Report;
  /** Whether a rate source is available, which adds the cost tile. */
  hasCost: boolean;
}

export function SummaryTiles({ report, hasCost }: SummaryTilesProps) {
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
        hint="Median time of a succeeded run."
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
        hint={summary.mean_turns_succeeded === null ? undefined : "mean per succeeded run"}
      />,
    );
  }
  const isEveryRunMeasured =
    tier === "full" &&
    summary.tokens_unmeasured_runs === 0 &&
    summary.tokens_lower_bound_runs === 0;
  tiles.push(
    <Tile
      key="tokens"
      label="Tokens"
      value={statsTokensFigure(summary, tier)}
      hint={isEveryRunMeasured ? TOKENS_TILE_CAPTION : null}
    />,
  );
  const cacheReads = statsCacheReadsText(summary, tier);
  tiles.push(
    cacheReads === null ? (
      <div key="cache-reads" className="row-span-2" />
    ) : (
      <Tile key="cache-reads" label="Cache reads" value={{ text: cacheReads }} />
    ),
  );
  if (hasCost) {
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
    <div className="grid grid-cols-1 gap-lg md:grid-cols-[auto_minmax(0,1fr)] md:items-end md:gap-x-xxxl">
      <div className="flex flex-col">
        <span className="font-heading text-h3 font-semibold text-ink tabular-nums">
          {fmtPercent(summary.success_rate)}
        </span>
        <span className="mt-xs text-body-md text-ink-body">
          {fmtCount(summary.succeeded)} of {plural(summary.runs, "run", "runs")} succeeded
        </span>
      </div>
      <div className="-mt-lg grid grid-cols-2 gap-x-lg md:grid-cols-3 xl:auto-cols-fr xl:grid-flow-col xl:grid-cols-none xl:grid-rows-[repeat(2,auto)]">
        {tiles}
      </div>
    </div>
  );
}
