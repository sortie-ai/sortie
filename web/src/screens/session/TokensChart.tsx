import { useRef } from "react";

import { ChartFrame, useChart } from "@/components/ChartFrame";
import {
  CacheReadColumn,
  KindLegend,
  KindRows,
  KindStack,
  STACKED_KINDS,
} from "@/components/TokenKinds";
import { Tooltip, TooltipPopup, TooltipTrigger } from "@/components/ui/tooltip";
import { useWidth } from "@/components/use-width";
import { cn } from "@/lib/cn";
import { fmtClockSec, fmtCount, fmtDuration, fmtTokens } from "@/lib/format";

import type { KindValues } from "@/components/TokenKinds";
import type { ReactNode } from "react";

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

/** The kinds a turn stacks, `null` for a plain bar and for the turn in progress, which stays hollow. */
function stackedKindsOf(bar: TurnBar): KindValues | null {
  return bar.inProgress ? null : bar.kinds;
}

function noteOf(bar: TurnBar): ReactNode {
  const kinds = stackedKindsOf(bar);
  return (
    <div className="min-w-35">
      <div className="mb-xxs font-bold">{headingOf(bar)}</div>
      {kinds === null ? (
        <div className="flex items-center gap-xs">
          Tokens
          <b className="ml-auto font-medium tabular-nums">{fmtCount(bar.tokens)}</b>
        </div>
      ) : (
        <KindRows kinds={kinds} figureOf={(n) => ({ text: fmtCount(n) })} />
      )}
      {bar.seconds === null ? undefined : (
        <div className="flex items-center gap-xs">
          Duration
          <b className="ml-auto font-medium tabular-nums">{fmtDuration(bar.seconds)}</b>
        </div>
      )}
    </div>
  );
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
  note: ReactNode;
}

function BarSlot({ bar, top, hasLabel, note }: BarSlotProps) {
  const chart = useChart();
  const kinds = stackedKindsOf(bar);
  return (
    <Tooltip>
      <TooltipTrigger
        delay={0}
        render={
          <div
            tabIndex={0}
            className="relative flex h-full items-end justify-center outline-none focus-visible:outline-2 focus-visible:-outline-offset-2 focus-visible:outline-focus-ring"
          />
        }
      >
        {kinds === null ? (
          <div
            className={cn(
              "w-[max(1px,calc(100%-4px))] max-w-10 rounded-t",
              bar.inProgress ? "border border-primary bg-surface-raised" : "bg-primary",
            )}
            style={{ height: `${(bar.tokens / top) * 100}%` }}
          />
        ) : (
          <KindStack
            kinds={STACKED_KINDS}
            values={kinds}
            heightPercent={(100 * bar.tokens) / top}
            width="w-[max(1px,calc(100%-4px))] max-w-10"
            isLowerBound={false}
          />
        )}
        {hasLabel ? (
          <span className="absolute top-full left-1/2 mt-0.5 -translate-x-1/2 text-body-sm whitespace-nowrap text-ink-muted">
            {bar.turn}
          </span>
        ) : undefined}
      </TooltipTrigger>
      <TooltipPopup besideChart={chart}>{note}</TooltipPopup>
    </Tooltip>
  );
}

export interface TokensLegendProps {
  hasInProgress: boolean;
}

export function TokensLegend({ hasInProgress }: TokensLegendProps) {
  return (
    <KindLegend>
      {hasInProgress ? (
        <span className="inline-flex items-center gap-xs">
          <span
            aria-hidden="true"
            className="size-sm rounded border border-primary bg-surface-raised"
          />
          <span className="self-baseline">In progress</span>
        </span>
      ) : undefined}
    </KindLegend>
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
  const turns = `${bars.length} ${bars.length === 1 ? "turn" : "turns"}`;
  const notes = bars.map(noteOf);
  const cacheReads = bars.map((bar) => stackedKindsOf(bar)?.cache_read ?? 0);
  const cacheTop = topOf(Math.max(0, ...cacheReads));
  const subplot = cacheReads.some((n) => n > 0)
    ? {
        label: `Cache read per turn, ${turns}`,
        valueName: "Cache read",
        ticks: [fmtTokens(cacheTop), fmtTokens(0)],
        children: bars.map((_, index) => (
          <CacheReadColumn
            key={index}
            cacheRead={cacheReads[index] ?? 0}
            top={cacheTop}
            note={notes[index]}
          />
        )),
      }
    : undefined;

  return (
    <ChartFrame
      label={`Tokens per turn, ${turns}`}
      valueName="Tokens"
      categoryName="Turn"
      ticks={[1, 0.5, 0].map((fraction) => fmtTokens(top * fraction))}
      plotRef={plotRef}
      subplot={subplot}
    >
      {bars.map((bar, index) => (
        <BarSlot
          key={index}
          bar={bar}
          top={top}
          hasLabel={bar.turn !== null && index % stride === 0}
          note={notes[index]}
        />
      ))}
    </ChartFrame>
  );
}
