import { useLayoutEffect, useRef, useState } from "react";

import { Tooltip, TooltipPopup, TooltipTrigger } from "@/components/ui/tooltip";
import { cn } from "@/lib/cn";
import { fmtClockSec, fmtCount, fmtDuration, fmtTokens } from "@/lib/format";

import type { RefObject } from "react";
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

function useWidth(ref: RefObject<HTMLElement | null>): number {
  const [width, setWidth] = useState(0);
  useLayoutEffect(() => {
    const element = ref.current;
    if (element === null) {
      return;
    }
    setWidth(element.clientWidth);
    const observer = new ResizeObserver(([entry]) => {
      if (entry !== undefined) {
        setWidth(entry.contentRect.width);
      }
    });
    observer.observe(element);
    return () => observer.disconnect();
  }, [ref]);
  return width;
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
      <TooltipPopup>
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

/** Props of {@link TokensLegend}. */
export interface TokensLegendProps {
  /** Whether a bar for the turn in progress is drawn. */
  hasInProgress: boolean;
}

/** Names the fills of the tokens chart: completed turns, and the turn in progress while one is drawn. */
export function TokensLegend({ hasInProgress }: TokensLegendProps) {
  return (
    <span className="inline-flex flex-wrap items-center gap-x-md gap-y-xxs">
      <span className="inline-flex items-center gap-xs">
        <span aria-hidden="true" className="size-sm rounded bg-primary" />
        Completed turn
      </span>
      {hasInProgress ? (
        <span className="inline-flex items-center gap-xs">
          <span
            aria-hidden="true"
            className="size-sm rounded border border-primary bg-surface-raised"
          />
          In progress
        </span>
      ) : undefined}
    </span>
  );
}

/** Props of {@link TokensChart}. */
export interface TokensChartProps {
  /** One bar per turn in timeline order; at least one. */
  bars: readonly TurnBar[];
}

/** Draws the tokens of each turn as a bar chart, with a tooltip of turn, tokens, and duration on every bar. */
export function TokensChart({ bars }: TokensChartProps) {
  const plotRef = useRef<HTMLDivElement>(null);
  const plotWidth = useWidth(plotRef);
  const stride = labelStrideOf(bars, plotWidth);
  const top = topOf(Math.max(...bars.map((bar) => bar.tokens)));

  return (
    <div
      role="img"
      aria-label={`Tokens per turn, ${bars.length} ${bars.length === 1 ? "turn" : "turns"}`}
      className="relative h-44"
    >
      <span className="absolute top-0 left-0 text-body-sm text-ink-muted">Tokens</span>
      <div ref={plotRef} className="absolute top-6 right-0 bottom-10 left-7.5">
        {[0, 0.5].map((fraction) => (
          <div
            key={fraction}
            className="absolute inset-x-0 border-t border-line"
            style={{ top: `${fraction * 100}%` }}
          />
        ))}
        <div className="absolute inset-x-0 bottom-0 border-t border-line-strong" />
        {[0, 0.5, 1].map((fraction) => (
          <span
            key={fraction}
            className="absolute right-full mr-1.5 -translate-y-1/2 text-body-sm text-ink-muted"
            style={{ top: `${(1 - fraction) * 100}%` }}
          >
            {fmtTokens(top * fraction)}
          </span>
        ))}
        <div className="absolute inset-0 grid auto-cols-fr grid-flow-col">
          {bars.map((bar, index) => (
            <BarSlot
              key={index}
              bar={bar}
              top={top}
              hasLabel={bar.turn !== null && index % stride === 0}
            />
          ))}
        </div>
      </div>
      <span className="absolute right-0 bottom-0 left-7.5 text-center text-body-sm text-ink-muted">
        Turn
      </span>
    </div>
  );
}
