import { useLayoutEffect, useRef, useState } from "react";

import { Card } from "@/components/ui/card";
import { Tooltip, TooltipPopup, TooltipTrigger } from "@/components/ui/tooltip";
import { cn } from "@/lib/cn";
import {
  fmtMoney,
  fmtUtcClock,
  fmtUtcPeriod,
  fmtUtcPeriodTick,
  fmtUtcStamp,
  parseInstant,
  plural,
} from "@/lib/format";

import type { Spend, SpendBucket } from "@/api/types.gen";
import type { CSSProperties, RefObject } from "react";

type Period = "day" | "month";

// Both stripe tones stay in the primary hue and stay filled, so a bar that is only
// a lower bound never reads as empty.
const LOWER_BOUND_FILL: CSSProperties = {
  backgroundImage:
    "repeating-linear-gradient(135deg, var(--color-primary) 0 3px, color-mix(in srgb, var(--color-primary) 55%, var(--color-canvas)) 3px 6px)",
};

const LABEL_CHAR_PX = 7;
const LABEL_GAP_PX = 8;

// The smallest 1, 2, or 5 times a power of ten that holds the highest bucket
// keeps a day of a few cents from flattening against a coarse scale; the scale
// has no floor, so a range that costs cents reads in cents.
function topOf(max: number): number {
  if (max <= 0) {
    return 1;
  }
  const magnitude = 10 ** Math.floor(Math.log10(max));
  const step = [1, 2, 5].find((candidate) => candidate * magnitude >= max) ?? 10;
  return step * magnitude;
}

type BarKind = "estimated" | "lower-bound" | "unknown";

interface Bar {
  kind: BarKind;
  height: string;
}

// A bucket whose cost is unknown still draws a thin lower-bound bar when runs
// spent tokens it could not price or measure, so it never reads as a day
// without spend.
function barOf(bucket: SpendBucket, top: number): Bar | undefined {
  if (bucket.runs === 0) {
    return undefined;
  }
  const isUnknown = bucket.cost_unpriced_runs > 0 || bucket.tokens_unmeasured_runs > 0;
  if (bucket.cost_usd === null) {
    return isUnknown ? { kind: "unknown", height: "2px" } : undefined;
  }
  return {
    kind: isUnknown || bucket.cost_partial_runs > 0 ? "lower-bound" : "estimated",
    height: `${(bucket.cost_usd / top) * 100}%`,
  };
}

const NOTE_BY_KIND: Record<BarKind, string> = {
  estimated: "Estimated cost of the runs that finished in this period.",
  "lower-bound":
    "At least this much. Some runs could not be priced or measured, so the cost is higher.",
  unknown: "Cost unknown. The runs could not be priced or measured.",
};

function noteOf(bucket: SpendBucket, bar: Bar | undefined): string {
  if (bucket.runs === 0) {
    return "No runs finished in this period.";
  }
  return bar === undefined ? "The runs of this period spent nothing." : NOTE_BY_KIND[bar.kind];
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

// Every Nth slot carries a label once the slots are narrower than the widest
// label, so the labels never overlap.
function labelStrideOf(labels: readonly string[], plotWidth: number): number {
  if (plotWidth <= 0 || labels.length === 0) {
    return 1;
  }
  const widest = Math.max(...labels.map((label) => label.length));
  const labelWidth = widest * LABEL_CHAR_PX + LABEL_GAP_PX;
  return Math.max(1, Math.ceil(labelWidth / (plotWidth / labels.length)));
}

function fromOf(
  bucket: SpendBucket,
  index: number,
  since: string | null,
  period: Period,
): string | undefined {
  const sinceAt = parseInstant(since);
  const startAt = parseInstant(bucket.start);
  if (index !== 0 || since === null || sinceAt === null || startAt === null || sinceAt <= startAt) {
    return undefined;
  }
  return period === "day" ? `from ${fmtUtcClock(since)} UTC` : `from ${fmtUtcStamp(since)}`;
}

interface SpendSlotProps {
  bucket: SpendBucket;
  bar: Bar | undefined;
  period: Period;
  from: string | undefined;
  tick: string | undefined;
}

function SpendSlot({ bucket, bar, period, from, tick }: SpendSlotProps) {
  return (
    <Tooltip>
      <TooltipTrigger
        delay={0}
        render={<div tabIndex={0} className="relative flex h-full items-end justify-center" />}
      >
        {bar === undefined ? undefined : (
          <div
            className={cn(
              "w-[max(1px,calc(100%-4px))] max-w-10 rounded-t",
              bar.kind === "estimated" && "bg-primary",
            )}
            style={{
              height: bar.height,
              ...(bar.kind === "estimated" ? undefined : LOWER_BOUND_FILL),
            }}
          />
        )}
        {tick === undefined ? undefined : (
          <span className="absolute top-full left-1/2 mt-0.5 -translate-x-1/2 text-body-sm whitespace-nowrap text-ink-muted">
            {tick}
          </span>
        )}
      </TooltipTrigger>
      <TooltipPopup>
        <div className="min-w-35">
          <div className="mb-xxs font-bold">{fmtUtcPeriod(bucket.start, period)}</div>
          {from === undefined ? undefined : <div>{from}</div>}
          <div>{noteOf(bucket, bar)}</div>
        </div>
      </TooltipPopup>
    </Tooltip>
  );
}

/** Props of {@link SpendChart}. */
export interface SpendChartProps {
  spend: Spend;
  /** The report's lower bound, which cuts the first bucket short while it lies inside it. */
  since: string | null;
}

/**
 * Draws the estimated cost of each served UTC period as a bar chart, with a
 * tooltip on every slot that names the period and says in words what its bar
 * means. Every date and time it writes is in UTC.
 */
export function SpendChart({ spend, since }: SpendChartProps) {
  const plotRef = useRef<HTMLDivElement>(null);
  const plotWidth = useWidth(plotRef);
  const period: Period = spend.period === "day" ? "day" : "month";
  const buckets = spend.buckets ?? [];
  const top = topOf(Math.max(0, ...buckets.map((bucket) => bucket.cost_usd ?? 0)));
  const bars = buckets.map((bucket) => barOf(bucket, top));
  const ticks = buckets.map((bucket) => fmtUtcPeriodTick(bucket.start, period));
  const stride = labelStrideOf(ticks, plotWidth);
  const topLabel = fmtMoney(top);
  const gutter = topLabel.length * LABEL_CHAR_PX + LABEL_GAP_PX;
  const axisName = `UTC ${period}`;

  return (
    <Card title="Spend">
      <div className="flex flex-col gap-xs px-md py-sm">
        <div
          role="img"
          aria-label={`Estimated cost per ${axisName}, ${plural(buckets.length, period, `${period}s`)}`}
          className="relative h-44"
        >
          <span className="absolute top-0 left-0 text-body-sm text-ink-muted">Estimated cost</span>
          <div ref={plotRef} className="absolute top-6 right-0 bottom-10" style={{ left: gutter }}>
            <div className="absolute inset-x-0 top-0 border-t border-line" />
            <div className="absolute inset-x-0 top-1/2 border-t border-line" />
            <div className="absolute inset-x-0 bottom-0 border-t border-line-strong" />
            <span className="absolute top-0 right-full mr-1.5 -translate-y-1/2 text-body-sm text-ink-muted">
              {topLabel}
            </span>
            <div className="absolute inset-0 grid auto-cols-fr grid-flow-col">
              {buckets.map((bucket, index) => (
                <SpendSlot
                  key={bucket.start}
                  bucket={bucket}
                  bar={bars[index]}
                  period={period}
                  from={fromOf(bucket, index, since, period)}
                  tick={index % stride === 0 ? ticks[index] : undefined}
                />
              ))}
            </div>
          </div>
          <span
            className="absolute right-0 bottom-0 text-center text-body-sm text-ink-muted"
            style={{ left: gutter }}
          >
            {axisName}
          </span>
        </div>
      </div>
    </Card>
  );
}
