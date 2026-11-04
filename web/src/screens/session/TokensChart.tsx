import { useRef } from "react";

import { ChartFrame, useChart } from "@/components/ChartFrame";
import { Tooltip, TooltipPopup, TooltipTrigger } from "@/components/ui/tooltip";
import { useWidth } from "@/components/use-width";
import { cn } from "@/lib/cn";
import { fmtClockSec, fmtCount, fmtDuration, fmtTokens } from "@/lib/format";

import type { TurnBar } from "./timeline";

const LABEL_DIGIT_PX = 7;
const LABEL_GAP_PX = 8;

// The smallest 1, 2, or 5 times a power of ten that holds the highest bar keeps
// a turn of a few thousand tokens from flattening against a coarse scale.
function topOf(max: number): number {
  const magnitude = 10 ** Math.floor(Math.log10(Math.max(max, 1)));
  const step = [1, 2, 5].find((candidate) => candidate * magnitude >= max) ?? 10;
  return step * magnitude;
}

function headingOf(bar: TurnBar): string {
  if (bar.turn !== null) {
    return `Turn ${bar.turn}`;
  }
  return bar.at === null ? "Turn in progress" : fmtClockSec(bar.at);
}

// Every Nth bar carries a label once the slots are narrower than the widest
// turn number, so the labels never overlap.
function labelStrideOf(bars: readonly TurnBar[], plotWidth: number): number {
  if (plotWidth <= 0) {
    return 1;
  }
  const highest = Math.max(1, ...bars.map((bar) => bar.turn ?? 0));
  const labelWidth = String(highest).length * LABEL_DIGIT_PX + LABEL_GAP_PX;
  return Math.max(1, Math.ceil(labelWidth / (plotWidth / bars.length)));
}

interface BarSlotProps {
  bar: TurnBar;
  top: number;
  hasLabel: boolean;
}

function BarSlot({ bar, top, hasLabel }: BarSlotProps) {
  const chart = useChart();
  return (
    <Tooltip>
      <TooltipTrigger
        delay={0}
        render={<div className="relative flex h-full items-end justify-center" />}
      >
        <div
          className={cn(
            "w-[max(1px,calc(100%-4px))] max-w-10 rounded-t",
            bar.inProgress ? "border border-primary bg-surface-raised" : "bg-primary",
          )}
          style={{ height: `${(bar.tokens / top) * 100}%` }}
        />
        {hasLabel ? (
          <span className="absolute top-full left-1/2 mt-0.5 -translate-x-1/2 text-body-sm whitespace-nowrap text-ink-muted">
            {bar.turn}
          </span>
        ) : undefined}
      </TooltipTrigger>
      <TooltipPopup besideChart={chart}>
        <div className="min-w-35">
          <div className="mb-xxs font-bold">{headingOf(bar)}</div>
          <div className="flex items-center gap-xs">
            Tokens
            <b className="ml-auto font-medium tabular-nums">{fmtCount(bar.tokens)}</b>
          </div>
          {bar.seconds === null ? undefined : (
            <div className="flex items-center gap-xs">
              Duration
              <b className="ml-auto font-medium tabular-nums">{fmtDuration(bar.seconds)}</b>
            </div>
          )}
        </div>
      </TooltipPopup>
    </Tooltip>
  );
}

export interface TokensLegendProps {
  hasInProgress: boolean;
}

export function TokensLegend({ hasInProgress }: TokensLegendProps) {
  return (
    <span className="inline-flex flex-wrap items-baseline gap-x-md gap-y-xxs">
      <span className="inline-flex items-center gap-xs">
        <span aria-hidden="true" className="size-sm rounded bg-primary" />
        <span className="self-baseline">Completed turn</span>
      </span>
      {hasInProgress ? (
        <span className="inline-flex items-center gap-xs">
          <span
            aria-hidden="true"
            className="size-sm rounded border border-primary bg-surface-raised"
          />
          <span className="self-baseline">In progress</span>
        </span>
      ) : undefined}
    </span>
  );
}

export interface TokensChartProps {
  /** One bar per turn in timeline order; at least one. */
  bars: readonly TurnBar[];
}

export function TokensChart({ bars }: TokensChartProps) {
  const plotRef = useRef<HTMLDivElement>(null);
  const plotWidth = useWidth(plotRef);
  const stride = labelStrideOf(bars, plotWidth);
  const top = topOf(Math.max(...bars.map((bar) => bar.tokens)));

  return (
    <ChartFrame
      label={`Tokens per turn, ${bars.length} ${bars.length === 1 ? "turn" : "turns"}`}
      valueName="Tokens"
      categoryName="Turn"
      ticks={[1, 0.5, 0].map((fraction) => fmtTokens(top * fraction))}
      plotRef={plotRef}
    >
      {bars.map((bar, index) => (
        <BarSlot
          key={index}
          bar={bar}
          top={top}
          hasLabel={bar.turn !== null && index % stride === 0}
        />
      ))}
    </ChartFrame>
  );
}
