import { useCallback, useEffect, useRef, useState } from "react";

import { ChartFrame, useChart } from "@/components/ChartFrame";
import { Panel } from "@/components/Panel";
import {
  CacheReadColumn,
  IN_PROGRESS,
  KIND_ORDER,
  KindLegend,
  KindRows,
  KindStack,
  STACKED_KINDS,
  kindsOfCosts,
  kindsOfStats,
} from "@/components/TokenKinds";
import { Button } from "@/components/ui/button";
import { Tooltip, TooltipPopup, TooltipTrigger } from "@/components/ui/tooltip";
import { useWidth } from "@/components/use-width";
import { cn } from "@/lib/cn";
import { CHART_UNKNOWN, COST_LOWER_BOUND, NOT_MEASURED, TOKENS_LOWER_BOUND } from "@/lib/figures";
import {
  fmtMoney,
  fmtTokens,
  fmtUtcClock,
  fmtUtcPeriodTick,
  parseInstant,
  plural,
} from "@/lib/format";

import {
  averageAndPeak,
  canZoomInto,
  isEmptyBucket,
  selectedWindowOf,
  spanIndexesOf,
  valueOf,
} from "./activity";
import { PERIOD_NAMES, bucketNameOf, inProgressTextOf, peakAtOf } from "./bucket-names";
import { SelectionBlock } from "./SelectionBlock";

import type { Spend, SpendBucket } from "@/api/types.gen";
import type { TooltipProps } from "@/components/ui/tooltip";
import type { Units } from "@/lib/units";
import type { Period } from "./activity";
import type { AnalyticsView, AnalyticsViewActions } from "./analytics-range";
import type {
  CSSProperties,
  KeyboardEvent as ReactKeyboardEvent,
  PointerEvent as ReactPointerEvent,
  ReactNode,
} from "react";

type SubDayPeriod = Exclude<Period, "day" | "month">;

const SUB_DAY_MS: Record<SubDayPeriod, number> = {
  five_minutes: 5 * 60_000,
  quarter_hour: 15 * 60_000,
  hour: 60 * 60_000,
};

const THREE_HOURS_MS = 3 * 60 * 60_000;

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

interface CostBarViewProps {
  isLowerBound: boolean;
  isInProgress: boolean;
  height: string;
  width: string;
}

function CostBarView({ isLowerBound, isInProgress, height, width }: CostBarViewProps) {
  return (
    <div
      className={cn(width, "rounded-t", !isLowerBound && "bg-primary", isInProgress && IN_PROGRESS)}
      style={{ height, ...(isLowerBound ? LOWER_BOUND_FILL : undefined) }}
    />
  );
}

// A bucket whose cost is unknown still draws a thin lower-bound bar when runs
// spent tokens it could not price or measure, so it never reads as a day
// without spend. A bucket holding a run recorded before per-turn records has no
// cost split, so it draws one fill instead of the kinds.
function costBarOf(bucket: SpendBucket, top: number, width: string): ReactNode {
  if (isEmptyBucket(bucket)) {
    return undefined;
  }
  if (bucket.cost_usd === null) {
    const isUnknown = bucket.cost_unpriced_runs > 0 || bucket.tokens_unmeasured_runs > 0;
    return isUnknown ? (
      <CostBarView isLowerBound isInProgress={false} height="2px" width={width} />
    ) : undefined;
  }
  const height = (100 * bucket.cost_usd) / top;
  if (bucket.cost_by_kind === null) {
    return (
      <CostBarView
        isLowerBound={bucket.cost_lower_bound}
        isInProgress={bucket.in_progress}
        height={`${height}%`}
        width={width}
      />
    );
  }
  return (
    <KindStack
      kinds={KIND_ORDER}
      values={kindsOfCosts(bucket.cost_by_kind)}
      heightPercent={height}
      width={width}
      isLowerBound={bucket.cost_lower_bound}
      isInProgress={bucket.in_progress}
    />
  );
}

function costNoteOf(bucket: SpendBucket): ReactNode {
  if (isEmptyBucket(bucket)) {
    return NO_RUNS;
  }
  if (bucket.cost_usd === null) {
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
      {bucket.cost_lower_bound ? <div className="mt-xxs">{COST_LOWER_BOUND}</div> : undefined}
    </>
  );
}

function tokensBarOf(bucket: SpendBucket, top: number, width: string): ReactNode {
  if (isEmptyBucket(bucket)) {
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
      isInProgress={bucket.in_progress}
    />
  );
}

function bucketTokensNote(bucket: SpendBucket): ReactNode {
  if (isEmptyBucket(bucket)) {
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

function fromOf(bucket: SpendBucket, index: number, since: string | null): string | undefined {
  const sinceAt = parseInstant(since);
  const startAt = parseInstant(bucket.start);
  if (index !== 0 || since === null || sinceAt === null || startAt === null || sinceAt <= startAt) {
    return undefined;
  }
  return `from ${fmtUtcClock(since)} UTC`;
}

const SLOT =
  "relative flex h-full cursor-pointer items-end justify-center rounded-t outline-none select-none touch-none hover:bg-state-hover focus-visible:outline-2 focus-visible:-outline-offset-2 focus-visible:outline-focus-ring data-dim:opacity-60";

interface UsageSlotProps {
  bar: ReactNode;
  peak: ReactNode;
  note: ReactNode;
  tick: string | undefined;
  index: number;
  label: string;
  isPressed: boolean;
  isDim: boolean;
  isQuiet: boolean;
  onPress: (event: ReactPointerEvent<HTMLElement>, index: number) => void;
  onKey: (event: ReactKeyboardEvent<HTMLElement>, index: number) => void;
}

function UsageSlot({
  bar,
  peak,
  note,
  tick,
  index,
  label,
  isPressed,
  isDim,
  isQuiet,
  onPress,
  onKey,
}: UsageSlotProps) {
  const chart = useChart();
  const [isOpen, setIsOpen] = useState(false);
  const isPressFocused = useRef(false);
  // A note opened before the press is dropped, so it stays shut after the release.
  if (isQuiet && isOpen) {
    setIsOpen(false);
  }
  const handleOpenChange: NonNullable<TooltipProps["onOpenChange"]> = (next, details) => {
    const isFocusOfPress = details.reason === "trigger-focus" && isPressFocused.current;
    setIsOpen(next && !isQuiet && !isFocusOfPress);
  };
  const handlePointerDown = (event: ReactPointerEvent<HTMLElement>) => {
    if (event.button === 0) {
      isPressFocused.current = true;
      onPress(event, index);
    }
  };
  return (
    <Tooltip open={isOpen} onOpenChange={handleOpenChange}>
      <TooltipTrigger
        delay={0}
        render={<div tabIndex={0} className={SLOT} />}
        role="button"
        aria-pressed={isPressed}
        aria-label={label}
        data-dim={isDim ? "" : undefined}
        data-slot-index={index}
        onPointerDown={handlePointerDown}
        onKeyDown={(event) => onKey(event, index)}
        onBlur={() => {
          isPressFocused.current = false;
        }}
      >
        {bar}
        {peak}
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

// The edge slots anchor their figure to the plot's edge so it never spills past the card.
function peakAnchorOf(index: number, count: number): string {
  if (index < 3) {
    return "left-0";
  }
  return index > count - 4 ? "right-0" : "left-1/2 -translate-x-1/2";
}

interface PeakFigureProps {
  text: string;
  heightPercent: number;
  anchor: string;
}

function PeakFigure({ text, heightPercent, anchor }: PeakFigureProps) {
  return (
    <span
      className={cn(
        "pointer-events-none absolute text-body-sm font-medium whitespace-nowrap text-ink tabular-nums",
        anchor,
      )}
      style={{ bottom: `calc(${heightPercent}% + 2px)` }}
    >
      {text}
    </span>
  );
}

function AverageLine({ heightPercent }: { heightPercent: number }) {
  return (
    <div
      aria-hidden="true"
      className="pointer-events-none absolute inset-x-0 z-10 border-t-2 border-dashed border-ink-muted"
      style={{ bottom: `${heightPercent}%` }}
    >
      <span className="absolute bottom-full left-0 mb-xxs text-body-sm text-ink-muted">Avg</span>
    </div>
  );
}

interface FigureRowProps {
  averageText: string;
  per: string;
  peak: { text: string; at: string } | undefined;
}

function FigureRow({ averageText, per, peak }: FigureRowProps) {
  return (
    <div className="flex flex-wrap items-center gap-x-lg gap-y-xxs text-body-sm text-ink-muted">
      <span className="inline-flex items-center gap-xs">
        <span aria-hidden="true" className="w-md border-t-2 border-dashed border-ink-muted" />
        Average <b className="text-body-sm font-medium text-ink tabular-nums">
          {averageText}
        </b> per{" "}
        {per}
      </span>
      {peak === undefined ? undefined : (
        <span>
          Peak <b className="text-body-sm font-medium text-ink tabular-nums">{peak.text}</b> at{" "}
          {peak.at}
        </span>
      )}
    </div>
  );
}

interface Drag {
  anchor: number;
  end: number;
  moved: boolean;
}

function clampIndex(index: number, count: number): number {
  return Math.max(0, Math.min(count - 1, index));
}

function slotIndexAt(clientX: number, plot: DOMRect, count: number): number {
  return clampIndex(Math.floor(((clientX - plot.left) / plot.width) * count), count);
}

function focusSlot(plot: HTMLElement | null, index: number, options?: FocusOptions) {
  plot?.querySelector<HTMLElement>(`[data-slot-index="${index}"]`)?.focus(options);
}

// The command palette and the shortcuts dialog keep focus inside themselves, so an Escape
// coming from within a dialog is theirs to close. Matching the target instead of any open
// dialog leaves out a hint, which is a popover with the same role, and a menu, which clears.
function isEscapeOfDialog(event: KeyboardEvent): boolean {
  return event.target instanceof Element && event.target.closest('[role="dialog"]') !== null;
}

export interface UsageChartProps {
  spend: Spend;
  /** The report's lower bound, which cuts the first bucket short while it lies inside it. */
  since: string | null;
  units: Units;
  view: AnalyticsView;
  actions: AnalyticsViewActions;
}

/** Every date and time it writes is in UTC. */
export function UsageChart({ spend, since, units, view, actions }: UsageChartProps) {
  const plotRef = useRef<HTMLDivElement>(null);
  const plotWidth = useWidth(plotRef);
  const isTokens = units === "tokens";
  const { period } = spend;
  const buckets = spend.buckets ?? [];
  const starts = buckets.map((bucket) => Date.parse(bucket.start));
  const top = topOf(SCALE_HEADROOM * Math.max(0, ...buckets.map((b) => valueOf(b, units))));
  const width = barWidthOf(buckets.length);
  const formatValue = isTokens ? fmtTokens : fmtMoney;
  const { average, peakIndex } = averageAndPeak(buckets, units);
  const peakBucket = peakIndex === null ? undefined : buckets[peakIndex];
  const ticks = shownTicksOf(
    buckets.map((bucket) => tickOf(bucket.start, period, buckets.length)),
    plotWidth,
  );
  const { per: perName, axis: axisName } = PERIOD_NAMES[period];
  const valueName = isTokens ? "Tokens" : "Estimated cost";

  const [drag, setDrag] = useState<Drag>();
  const dragRef = useRef<Drag>(undefined);
  const isDragging = drag !== undefined;
  // The document listeners outlive a render, so they read what the latest one saw.
  const latest = useRef({ starts, selection: view.selection, actions });
  useEffect(() => {
    latest.current = { starts, selection: view.selection, actions };
  });

  const setDragState = useCallback((next: Drag | undefined) => {
    dragRef.current = next;
    setDrag(next);
  }, []);

  const selectBetween = useCallback((from: number, to: number, focusIndex: number) => {
    const { starts: current, actions: latestActions } = latest.current;
    const first = current[Math.min(from, to)];
    const last = current[Math.max(from, to)];
    if (first === undefined || last === undefined) {
      return;
    }
    latestActions.select({ first, last });
    focusSlot(plotRef.current, focusIndex, { preventScroll: true });
  }, []);

  const toggle = useCallback((index: number) => {
    const { starts: current, selection, actions: latestActions } = latest.current;
    const start = current[index];
    if (start === undefined) {
      return;
    }
    const isOnlySelected = selection?.first === start && selection.last === start;
    latestActions.select(isOnlySelected ? null : { first: start, last: start });
  }, []);

  const resize = useCallback(
    (index: number, end: number) => {
      const { starts: current, selection } = latest.current;
      const [first, last] = spanIndexesOf(current, selection) ?? [index, index];
      const anchor = index === last ? first : index === first ? last : index;
      selectBetween(anchor, end, end);
    },
    [selectBetween],
  );

  useEffect(() => {
    if (!isDragging) {
      return;
    }
    const handleMove = (event: PointerEvent) => {
      const current = dragRef.current;
      const plot = plotRef.current;
      if (current === undefined || plot === null) {
        return;
      }
      const end = slotIndexAt(
        event.clientX,
        plot.getBoundingClientRect(),
        latest.current.starts.length,
      );
      if (end !== current.end) {
        setDragState({ ...current, end, moved: true });
      }
    };
    const handleRelease = () => {
      const current = dragRef.current;
      setDragState(undefined);
      if (current === undefined) {
        return;
      }
      if (current.moved) {
        selectBetween(current.anchor, current.end, current.end);
      } else {
        toggle(current.anchor);
      }
    };
    document.addEventListener("pointermove", handleMove);
    document.addEventListener("pointerup", handleRelease);
    document.addEventListener("pointercancel", handleRelease);
    return () => {
      document.removeEventListener("pointermove", handleMove);
      document.removeEventListener("pointerup", handleRelease);
      document.removeEventListener("pointercancel", handleRelease);
    };
  }, [isDragging, selectBetween, setDragState, toggle]);

  useEffect(() => {
    const handleKeyDown = (event: KeyboardEvent) => {
      if (event.key !== "Escape" || isEscapeOfDialog(event)) {
        return;
      }
      const { selection, actions: latestActions } = latest.current;
      if (selection !== null) {
        latestActions.select(null);
      }
    };
    document.addEventListener("keydown", handleKeyDown);
    return () => document.removeEventListener("keydown", handleKeyDown);
  }, []);

  const handlePress = (event: ReactPointerEvent<HTMLElement>, index: number) => {
    event.preventDefault();
    event.currentTarget.focus({ preventScroll: true });
    setDragState({ anchor: index, end: index, moved: false });
  };

  const handleKey = (event: ReactKeyboardEvent<HTMLElement>, index: number) => {
    // Alt or Cmd with an arrow is the browser's Back and Forward.
    if (event.altKey || event.ctrlKey || event.metaKey) {
      return;
    }
    if (event.key === "Enter" || event.key === " ") {
      event.preventDefault();
      toggle(index);
      return;
    }
    const step = { ArrowLeft: -1, ArrowRight: 1 }[event.key];
    if (step === undefined) {
      return;
    }
    event.preventDefault();
    const target = clampIndex(index + step, buckets.length);
    if (event.shiftKey) {
      resize(index, target);
    } else {
      focusSlot(plotRef.current, target);
    }
  };

  const committed = spanIndexesOf(starts, view.selection);
  const draft: [number, number] | null =
    drag === undefined
      ? committed
      : [Math.min(drag.anchor, drag.end), Math.max(drag.anchor, drag.end)];
  const isInDraft = (index: number) => draft !== null && index >= draft[0] && index <= draft[1];

  const notes = buckets.map((bucket, index) => {
    const from = fromOf(bucket, index, since);
    return (
      <div key={bucket.start} className="min-w-35">
        <div className="mb-xxs font-bold">{bucketNameOf(bucket.start, period)}</div>
        {from === undefined ? undefined : <div>{from}</div>}
        {bucket.in_progress ? <div>{inProgressTextOf(bucket.start, period)}</div> : undefined}
        <div>{isTokens ? bucketTokensNote(bucket) : costNoteOf(bucket)}</div>
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
              isQuiet={isDragging}
            />
          )),
        }
      : undefined;

  const selectedWindow =
    view.selection === null ? undefined : selectedWindowOf(view.selection, period);
  const foot =
    committed === null || selectedWindow === undefined ? undefined : (
      <SelectionBlock
        buckets={buckets.slice(committed[0], committed[1] + 1)}
        period={period}
        units={units}
        canZoom={canZoomInto(
          selectedWindow,
          period,
          committed[1] - committed[0] + 1,
          buckets.length,
        )}
        onZoomIn={() => actions.zoomIn(selectedWindow)}
        onClear={() => actions.select(null)}
      />
    );

  return (
    <Panel
      title={isTokens ? `Tokens per ${perName}` : "Spend"}
      note={<KindLegend />}
      right={
        view.zoom === null ? undefined : (
          <>
            <Button variant="secondary" onClick={actions.zoomOut}>
              Zoom out
            </Button>
            <Button variant="ghost" onClick={actions.resetZoom}>
              Reset zoom
            </Button>
          </>
        )
      }
    >
      <ChartFrame
        label={`${valueName} per ${axisName}, ${plural(buckets.length, "bucket", "buckets")}`}
        valueName={valueName}
        categoryName={axisName}
        ticks={[1, 0.5, 0].map((fraction) => formatValue(top * fraction))}
        plotRef={plotRef}
        subplot={subplot}
        head={
          <FigureRow
            averageText={formatValue(average)}
            per={perName}
            peak={
              peakBucket === undefined
                ? undefined
                : {
                    text: formatValue(valueOf(peakBucket, units)),
                    at: peakAtOf(peakBucket.start, period),
                  }
            }
          />
        }
        behind={
          draft === null ? undefined : (
            <div
              aria-hidden="true"
              className="pointer-events-none absolute inset-y-0 rounded bg-item-selected"
              style={{
                left: `${(100 * draft[0]) / buckets.length}%`,
                width: `${(100 * (draft[1] - draft[0] + 1)) / buckets.length}%`,
              }}
            />
          )
        }
        front={average > 0 ? <AverageLine heightPercent={(100 * average) / top} /> : undefined}
        foot={foot}
      >
        {buckets.map((bucket, index) => (
          <UsageSlot
            key={bucket.start}
            bar={isTokens ? tokensBarOf(bucket, top, width) : costBarOf(bucket, top, width)}
            peak={
              index === peakIndex ? (
                <PeakFigure
                  text={formatValue(valueOf(bucket, units))}
                  heightPercent={(100 * valueOf(bucket, units)) / top}
                  anchor={peakAnchorOf(index, buckets.length)}
                />
              ) : undefined
            }
            note={notes[index]}
            tick={ticks[index]}
            index={index}
            label={bucketNameOf(bucket.start, period)}
            isPressed={isInDraft(index)}
            isDim={draft !== null && !isInDraft(index)}
            isQuiet={isDragging}
            onPress={handlePress}
            onKey={handleKey}
          />
        ))}
      </ChartFrame>
    </Panel>
  );
}
