import { useRef } from "react";

import { ChartFrame, useChart } from "@/components/ChartFrame";
import { Panel } from "@/components/Panel";
import {
  CacheReadColumn,
  KindLegend,
  KindRows,
  KindStack,
  STACKED_KINDS,
  kindsOfCosts,
  kindsOfStats,
} from "@/components/TokenKinds";
import { Tooltip, TooltipPopup, TooltipTrigger } from "@/components/ui/tooltip";
import { useWidth } from "@/components/use-width";
import { cn } from "@/lib/cn";
import { CHART_UNKNOWN, COST_LOWER_BOUND, NOT_MEASURED, TOKENS_LOWER_BOUND } from "@/lib/figures";
import {
  fmtMoney,
  fmtTokens,
  fmtUtcClock,
  fmtUtcPeriod,
  fmtUtcPeriodTick,
  fmtUtcStamp,
  parseInstant,
  plural,
} from "@/lib/format";

import type { Spend, SpendBucket } from "@/api/types.gen";
import type { Units } from "@/lib/units";
import type { CSSProperties, ReactNode } from "react";

type Period = Spend["period"];
type SubDayPeriod = Exclude<Period, "day" | "month">;

const SUB_DAY_MS: Record<SubDayPeriod, number> = {
  five_minutes: 5 * 60_000,
  quarter_hour: 15 * 60_000,
  hour: 60 * 60_000,
};

const THREE_HOURS_MS = 3 * 60 * 60_000;

// The axis caption doubles as the chart's aria label and the Cache read plot's label.
const PERIOD_NAMES: Record<Period, { per: string; axis: string }> = {
  five_minutes: { per: "5 minutes", axis: "UTC, 5 minutes" },
  quarter_hour: { per: "15 minutes", axis: "UTC, 15 minutes" },
  hour: { per: "hour", axis: "UTC hour" },
  day: { per: "day", axis: "UTC day" },
  month: { per: "month", axis: "UTC month" },
};

function isSubDay(period: Period): period is SubDayPeriod {
  return period !== "day" && period !== "month";
}

// Both stripe tones stay in the primary hue and stay filled, so a bar that is only
// a lower bound never reads as empty.
const LOWER_BOUND_FILL: CSSProperties = {
  backgroundImage:
    "repeating-linear-gradient(135deg, var(--color-primary) 0 3px, color-mix(in srgb, var(--color-primary) 55%, var(--color-canvas)) 3px 6px)",
};

const LABEL_CHAR_PX = 7;
const LABEL_GAP_PX = 8;
const SCALE_HEADROOM = 1.2;
const NO_RUNS = "No agent turns in this period.";

// Whole class strings, so the stylesheet scanner finds each one; the gutter narrows as the bars crowd.
function barWidthOf(count: number): string {
  if (count > 200) {
    return "w-[max(1px,calc(100%-1px))] max-w-10";
  }
  return count > 60
    ? "w-[max(1px,calc(100%-2px))] max-w-10"
    : "w-[max(1px,calc(100%-4px))] max-w-10";
}

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

function valueOf(bucket: SpendBucket, units: Units): number {
  return units === "tokens" ? (bucket.tokens?.uncached ?? 0) : (bucket.cost_usd ?? 0);
}

type CostBarKind = "estimated" | "lower-bound" | "unknown";

interface CostBar {
  kind: CostBarKind;
  height: string;
}

// A bucket whose cost is unknown still draws a thin lower-bound bar when runs
// spent tokens it could not price or measure, so it never reads as a day
// without spend.
function costBarOf(bucket: SpendBucket, top: number): CostBar | undefined {
  if (bucket.runs === 0) {
    return undefined;
  }
  if (bucket.cost_usd === null) {
    const isUnknown = bucket.cost_unpriced_runs > 0 || bucket.tokens_unmeasured_runs > 0;
    return isUnknown ? { kind: "unknown", height: "2px" } : undefined;
  }
  return {
    kind: bucket.cost_lower_bound ? "lower-bound" : "estimated",
    height: `${(bucket.cost_usd / top) * 100}%`,
  };
}

interface NoteRowProps {
  label: string;
  figure: string;
  className?: string;
}

function NoteRow({ label, figure, className }: NoteRowProps) {
  return (
    <div className={cn("flex items-center gap-xs", className)}>
      {label}
      <b className="ml-auto text-body-sm font-medium tabular-nums">{figure}</b>
    </div>
  );
}

function costNoteOf(bucket: SpendBucket, bar: CostBar | undefined): ReactNode {
  if (bucket.runs === 0) {
    return NO_RUNS;
  }
  if (bar === undefined) {
    return "The runs of this period spent nothing.";
  }
  if (bar.kind === "unknown" || bucket.cost_usd === null) {
    return CHART_UNKNOWN;
  }
  const { cost_by_kind: costByKind } = bucket;
  return (
    <>
      {costByKind === null ? undefined : (
        <KindRows
          kinds={kindsOfCosts(costByKind)}
          figureOf={(n) => ({ text: fmtMoney(n) })}
          showsHitRate={false}
        />
      )}
      <NoteRow
        label="Estimated cost"
        figure={fmtMoney(bucket.cost_usd)}
        className={costByKind === null ? undefined : "mt-xxs"}
      />
      {bar.kind === "lower-bound" ? <div className="mt-xxs">{COST_LOWER_BOUND}</div> : undefined}
    </>
  );
}

function CostBarView({ bar, width }: { bar: CostBar; width: string }) {
  return (
    <div
      className={cn(width, "rounded-t", bar.kind === "estimated" && "bg-primary")}
      style={{
        height: bar.height,
        ...(bar.kind === "estimated" ? undefined : LOWER_BOUND_FILL),
      }}
    />
  );
}

function tokensBarOf(bucket: SpendBucket, top: number, width: string): ReactNode {
  if (bucket.runs === 0) {
    return undefined;
  }
  if (bucket.tokens === null) {
    return <div className={cn("h-0.5 rounded-t bg-primary", width)} />;
  }
  if (bucket.tokens.uncached === 0) {
    return undefined;
  }
  return (
    <KindStack
      kinds={STACKED_KINDS}
      values={kindsOfStats(bucket.tokens)}
      heightPercent={(100 * bucket.tokens.uncached) / top}
      width={width}
      isLowerBound={bucket.tokens_lower_bound}
    />
  );
}

function bucketTokensNote(bucket: SpendBucket): ReactNode {
  if (bucket.runs === 0) {
    return NO_RUNS;
  }
  if (bucket.tokens === null) {
    return <NoteRow label="Tokens" figure={NOT_MEASURED} />;
  }
  return (
    <>
      <KindRows kinds={kindsOfStats(bucket.tokens)} figureOf={(n) => ({ text: fmtTokens(n) })} />
      {bucket.tokens_lower_bound ? <div className="mt-xxs">{TOKENS_LOWER_BOUND}</div> : undefined}
    </>
  );
}

// Sub-day ticks sit on clock boundaries instead of every Nth slot, so a 5-minute
// chart labels quarter hours only while it spans three hours or less.
function tickOf(start: string, period: Period, count: number): string {
  const instant = parseInstant(start);
  if (instant === null) {
    return "";
  }
  const date = new Date(instant);
  switch (period) {
    case "five_minutes": {
      const minutes = count * SUB_DAY_MS.five_minutes <= THREE_HOURS_MS ? 15 : 60;
      return date.getUTCMinutes() % minutes === 0 ? fmtUtcClock(start) : "";
    }
    case "quarter_hour":
      return date.getUTCMinutes() === 0 ? fmtUtcClock(start) : "";
    case "hour":
      return date.getUTCHours() === 0 ? fmtUtcPeriodTick(start, "day") : "";
    default:
      return fmtUtcPeriodTick(start, period);
  }
}

// A sub-day heading names the day and the clock span; a day or month names itself.
function headingOf(start: string, period: Period): string {
  if (!isSubDay(period)) {
    return fmtUtcPeriod(start, period);
  }
  const instant = parseInstant(start);
  if (instant === null) {
    return fmtUtcPeriod(start, "day");
  }
  const end = new Date(instant + SUB_DAY_MS[period]).toISOString();
  return `${fmtUtcPeriod(start, "day")}, ${fmtUtcClock(start)} to ${fmtUtcClock(end)} UTC`;
}

// Every Nth label is kept once the labels are narrower than the widest one, so
// they never overlap. The stride counts labels, not slots: counting slots drops
// every label whose slot index misses the stride.
function shownTicksOf(ticks: readonly string[], plotWidth: number): (string | undefined)[] {
  const labels = ticks.filter((tick) => tick !== "");
  if (plotWidth <= 0 || labels.length === 0) {
    return ticks.map((tick) => (tick === "" ? undefined : tick));
  }
  const widest = Math.max(...labels.map((label) => label.length));
  const labelWidth = widest * LABEL_CHAR_PX + LABEL_GAP_PX;
  const stride = Math.max(1, Math.ceil(labelWidth / (plotWidth / labels.length)));
  let rank = 0;
  return ticks.map((tick) => {
    if (tick === "") {
      return undefined;
    }
    const shown = rank % stride === 0;
    rank += 1;
    return shown ? tick : undefined;
  });
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
  return period === "month" ? `from ${fmtUtcStamp(since)}` : `from ${fmtUtcClock(since)} UTC`;
}

const SLOT =
  "relative flex h-full items-end justify-center rounded-t outline-none hover:bg-state-hover focus-visible:outline-2 focus-visible:-outline-offset-2 focus-visible:outline-focus-ring";

interface UsageSlotProps {
  bar: ReactNode;
  note: ReactNode;
  tick: string | undefined;
}

function UsageSlot({ bar, note, tick }: UsageSlotProps) {
  const chart = useChart();
  return (
    <Tooltip>
      <TooltipTrigger delay={0} render={<div tabIndex={0} className={SLOT} />}>
        {bar}
        {tick === undefined ? undefined : (
          <span className="absolute top-full left-1/2 mt-0.5 -translate-x-1/2 text-body-sm whitespace-nowrap text-ink-muted">
            {tick}
          </span>
        )}
      </TooltipTrigger>
      <TooltipPopup besideChart={chart}>{note}</TooltipPopup>
    </Tooltip>
  );
}

export interface UsageChartProps {
  spend: Spend;
  /** The report's lower bound, which cuts the first bucket short while it lies inside it. */
  since: string | null;
  units: Units;
}

/** Every date and time it writes is in UTC. */
export function UsageChart({ spend, since, units }: UsageChartProps) {
  const plotRef = useRef<HTMLDivElement>(null);
  const plotWidth = useWidth(plotRef);
  const isTokens = units === "tokens";
  const { period } = spend;
  const buckets = spend.buckets ?? [];
  const top = topOf(SCALE_HEADROOM * Math.max(0, ...buckets.map((b) => valueOf(b, units))));
  const width = barWidthOf(buckets.length);
  const ticks = shownTicksOf(
    buckets.map((bucket) => tickOf(bucket.start, period, buckets.length)),
    plotWidth,
  );
  const { per: perName, axis: axisName } = PERIOD_NAMES[period];
  const valueName = isTokens ? "Tokens" : "Estimated cost";

  const costBars = buckets.map((bucket) => costBarOf(bucket, top));
  const notes = buckets.map((bucket, index) => {
    const from = fromOf(bucket, index, since, period);
    return (
      <div key={bucket.start} className="min-w-35">
        <div className="mb-xxs font-bold">{headingOf(bucket.start, period)}</div>
        {from === undefined ? undefined : <div>{from}</div>}
        <div>{isTokens ? bucketTokensNote(bucket) : costNoteOf(bucket, costBars[index])}</div>
      </div>
    );
  });

  const cacheReads = buckets.map((bucket) => bucket.tokens?.cache_read ?? 0);
  const cacheReadTop = topOf(Math.max(0, ...cacheReads));
  const subplot =
    isTokens && cacheReads.some((n) => n > 0)
      ? {
          label: `Cache read per ${axisName}`,
          valueName: "Cache read",
          ticks: [fmtTokens(cacheReadTop), fmtTokens(0)],
          children: buckets.map((bucket, index) => (
            <CacheReadColumn
              key={bucket.start}
              cacheRead={cacheReads[index] ?? 0}
              top={cacheReadTop}
              note={notes[index]}
            />
          )),
        }
      : undefined;

  return (
    <Panel
      title={isTokens ? `Tokens per ${perName}` : "Spend"}
      note={isTokens ? <KindLegend /> : undefined}
    >
      <ChartFrame
        label={`${valueName} per ${axisName}, ${plural(buckets.length, "bucket", "buckets")}`}
        valueName={valueName}
        categoryName={axisName}
        ticks={[1, 0.5, 0].map((fraction) =>
          isTokens ? fmtTokens(top * fraction) : fmtMoney(top * fraction),
        )}
        plotRef={plotRef}
        subplot={subplot}
      >
        {buckets.map((bucket, index) => {
          const costBar = costBars[index];
          return (
            <UsageSlot
              key={bucket.start}
              bar={
                isTokens ? (
                  tokensBarOf(bucket, top, width)
                ) : costBar === undefined ? undefined : (
                  <CostBarView bar={costBar} width={width} />
                )
              }
              note={notes[index]}
              tick={ticks[index]}
            />
          );
        })}
      </ChartFrame>
    </Panel>
  );
}
