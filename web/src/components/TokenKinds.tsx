import { useChart } from "@/components/ChartFrame";
import { NotedFigure } from "@/components/NotedFigure";
import { Hint } from "@/components/ui/hint";
import { Tooltip, TooltipPopup, TooltipTrigger } from "@/components/ui/tooltip";
import { cn } from "@/lib/cn";
import { LABEL_CACHE_HIT_RATE } from "@/lib/figures";
import { fmtPercent } from "@/lib/format";

import type { KindCosts, TokenInfo, Tokens } from "@/api/types.gen";
import type { Figure } from "@/lib/figures";
import type { ReactNode } from "react";

/** The amounts of one token figure, split by kind. */
export type KindValues = Record<"input" | "output" | "cache_write" | "cache_read", number>;

export interface TokenKind {
  key: keyof KindValues;
  label: string;
  /** The background class that colors the kind wherever it is drawn. */
  fill: string;
}

const INPUT: TokenKind = { key: "input", label: "Input", fill: "bg-chart-a" };
const OUTPUT: TokenKind = { key: "output", label: "Output", fill: "bg-chart-b" };
const CACHE_WRITE: TokenKind = { key: "cache_write", label: "Cache write", fill: "bg-chart-c" };
const CACHE_READ: TokenKind = { key: "cache_read", label: "Cache read", fill: "bg-chart-d" };

/** The kinds that add up to the uncached figure, from the baseline up. */
export const STACKED_KINDS: readonly TokenKind[] = [INPUT, OUTPUT, CACHE_WRITE];

/** Every kind in the order a legend or a note lists them: cache read follows cache write. */
export const KIND_ORDER: readonly TokenKind[] = [...STACKED_KINDS, CACHE_READ];

/** Reads the kinds of a served token report; the served fresh input is the input kind. */
export function kindsOfStats(tokens: Tokens): KindValues {
  return {
    input: tokens.fresh_input,
    output: tokens.output,
    cache_write: tokens.cache_write,
    cache_read: tokens.cache_read,
  };
}

/** Reads the kinds of a served cost split; the served fresh input cost is the input kind. */
export function kindsOfCosts(costs: KindCosts): KindValues {
  return {
    input: costs.fresh_input_usd,
    output: costs.output_usd,
    cache_write: costs.cache_write_usd,
    cache_read: costs.cache_read_usd,
  };
}

/** Every kind at zero: the base of a run read from its first event, and an issue with no run history. */
export const EMPTY_KINDS: KindValues = { input: 0, output: 0, cache_write: 0, cache_read: 0 };

/** Input less both cache counters, floored at zero; null while input, cache read, or cache write is null. Input arrives cache-inclusive. */
export function freshInputOf(tokens: TokenInfo): number | null {
  const { input_tokens: input, cache_read_tokens: read, cache_write_tokens: write } = tokens;
  if (input === null || read === null || write === null) {
    return null;
  }
  return Math.max(0, input - read - write);
}

/** The kinds of a served TokenInfo; null while any of its counters is null. */
export function kindsOfTokenInfo(tokens: TokenInfo): KindValues | null {
  const input = freshInputOf(tokens);
  const { output_tokens: output, cache_write_tokens: write, cache_read_tokens: read } = tokens;
  if (input === null || output === null || write === null || read === null) {
    return null;
  }
  return { input, output, cache_write: write, cache_read: read };
}

/** Cache reads over the cache-inclusive input; null while either is null or not above zero. A zero cache read also means an agent that reports no cache data, so it states no rate. */
export function cacheHitRate(input: number | null, cacheRead: number | null): number | null {
  if (input === null || cacheRead === null || input <= 0 || cacheRead <= 0) {
    return null;
  }
  return cacheRead / input;
}

/** The cache hit rate of a set of kinds, whose input is the fresh input plus both cache kinds. */
export function hitRateOfKinds(kinds: KindValues): number | null {
  return cacheHitRate(kinds.input + kinds.cache_write + kinds.cache_read, kinds.cache_read);
}

// A gap parts the kinds of a bar only while every segment stands at least SEGMENT_MIN_PX tall;
// PLOT_PX is the height the chart frame gives its plot.
const PLOT_PX = 112;
const SEGMENT_MIN_PX = 6;

const LOWER_BOUND_FILL =
  "after:absolute after:inset-0 after:bg-[repeating-linear-gradient(135deg,transparent_0_3px,color-mix(in_srgb,var(--color-canvas)_45%,transparent)_3px_6px)]";

function KindSwatch({ kind }: { kind: TokenKind }) {
  return <span aria-hidden="true" className={cn("size-sm shrink-0 rounded", kind.fill)} />;
}

export interface KindLabelProps {
  kind: TokenKind;
}

/** Names one kind beside its swatch. */
export function KindLabel({ kind }: KindLabelProps) {
  return (
    <span className="inline-flex items-center gap-xs">
      <KindSwatch kind={kind} />
      <span className="self-baseline">{kind.label}</span>
    </span>
  );
}

export interface KindLegendProps {
  /** Rendered after the kinds, inside the same wrapper. */
  children?: ReactNode;
}

/** Names every kind beside its swatch, for the note of a panel that draws them. */
export function KindLegend({ children }: KindLegendProps) {
  return (
    <span className="inline-flex flex-wrap items-baseline gap-x-md gap-y-xxs">
      {KIND_ORDER.map((kind) => (
        <KindLabel key={kind.key} kind={kind} />
      ))}
      {children}
    </span>
  );
}

export interface KindRowsProps {
  kinds: KindValues;
  figureOf: (n: number) => Figure;
  /** Appends the cache hit rate row while it has a value; true when absent. Costs pass false, because a ratio of costs is no hit rate. */
  showsHitRate?: boolean;
}

/** Lists each kind with its figure, one row apiece, then the cache hit rate while it has a value, for the body of a note. */
export function KindRows({ kinds, figureOf, showsHitRate = true }: KindRowsProps) {
  const rate = showsHitRate ? hitRateOfKinds(kinds) : null;
  const rows = KIND_ORDER.map((kind) => (
    <div key={kind.key} className="flex items-center gap-xs">
      <KindSwatch kind={kind} />
      {kind.label}
      <b className="ml-auto text-body-sm font-medium tabular-nums">
        <NotedFigure figure={figureOf(kinds[kind.key])} />
      </b>
    </div>
  ));
  return rate === null
    ? rows
    : [
        ...rows,
        <div key="hit-rate" className="flex items-center gap-xs">
          {LABEL_CACHE_HIT_RATE}
          <b className="ml-auto text-body-sm font-medium tabular-nums">{fmtPercent(rate)}</b>
        </div>,
      ];
}

interface KindsNoteProps extends KindRowsProps {
  footnote?: string | undefined;
}

function KindsNote({ kinds, figureOf, footnote }: KindsNoteProps) {
  return (
    <div className="min-w-35">
      <KindRows kinds={kinds} figureOf={figureOf} />
      {footnote === undefined ? undefined : <div className="mt-xxs">{footnote}</div>}
    </div>
  );
}

export interface KindStackProps {
  kinds: readonly TokenKind[];
  values: KindValues;
  /** The height of the whole stack as a percentage of the plot. */
  heightPercent: number;
  /** The width classes of the bar. */
  width: string;
  isLowerBound: boolean;
}

/** Draws the kinds as one bar from the baseline up, each segment in proportion to its amount. */
export function KindStack({ kinds, values, heightPercent, width, isLowerBound }: KindStackProps) {
  const parts = kinds.filter((kind) => values[kind.key] > 0);
  const whole = parts.reduce((sum, kind) => sum + values[kind.key], 0);
  const smallest = Math.min(
    ...parts.map((kind) => ((heightPercent / 100) * PLOT_PX * values[kind.key]) / whole),
  );
  const isParted = parts.length > 1 && smallest >= SEGMENT_MIN_PX;

  return (
    <div
      className={cn(
        "relative flex flex-col-reverse overflow-hidden rounded-t",
        width,
        isParted && "gap-px",
        isLowerBound && LOWER_BOUND_FILL,
      )}
      style={{ height: `${heightPercent}%` }}
    >
      {parts.map((kind) => (
        <div
          key={kind.key}
          className={cn("min-h-px", kind.fill)}
          style={{ flexGrow: values[kind.key] }}
        />
      ))}
    </div>
  );
}

export interface SplitBarProps {
  kinds: KindValues;
  figureOf: (n: number) => Figure;
  /** A sentence under the kinds' rows, in the note. */
  footnote?: string;
}

/** Draws the stacked kinds as one bar across a cell, with every kind's figure in its note. */
export function SplitBar({ kinds, figureOf, footnote }: SplitBarProps) {
  const rate = hitRateOfKinds(kinds);
  return (
    <Tooltip>
      <TooltipTrigger
        delay={0}
        render={
          <span
            tabIndex={0}
            role="img"
            aria-label={[
              ...KIND_ORDER.map((kind) => `${kind.label} ${figureOf(kinds[kind.key]).text}`),
              ...(rate === null ? [] : [`${LABEL_CACHE_HIT_RATE} ${fmtPercent(rate)}`]),
              ...(footnote === undefined ? [] : [footnote]),
            ].join(", ")}
            className="flex h-sm w-full gap-[2px] overflow-hidden rounded"
          />
        }
      >
        {STACKED_KINDS.filter((kind) => kinds[kind.key] > 0).map((kind) => (
          <span
            key={kind.key}
            className={cn("min-w-px", kind.fill)}
            style={{ flexGrow: kinds[kind.key] }}
          />
        ))}
      </TooltipTrigger>
      <TooltipPopup>
        <KindsNote kinds={kinds} figureOf={figureOf} footnote={footnote} />
      </TooltipPopup>
    </Tooltip>
  );
}

export interface KindsFigureProps {
  /** The Tokens figure; its note becomes the kinds tooltip's footnote. */
  figure: Figure;
  /** null renders the figure as NotedFigure renders it. */
  kinds: KindValues | null;
  figureOf: (n: number) => Figure;
}

/** Renders a Tokens figure that opens the kinds of its amount in a hint, which a touch device opens too. */
export function KindsFigure({ figure, kinds, figureOf }: KindsFigureProps) {
  if (kinds === null) {
    return <NotedFigure figure={figure} />;
  }
  return (
    <Hint delay={0} hint={<KindsNote kinds={kinds} figureOf={figureOf} footnote={figure.note} />}>
      {figure.text}
    </Hint>
  );
}

export interface CacheReadColumnProps {
  cacheRead: number;
  top: number;
  note: ReactNode;
}

/** Draws one period's cache reads as a column of the plot under the kinds' stacks, opening the same note as its slot. The slot above is the keyboard stop, so a column takes no focus of its own. */
export function CacheReadColumn({ cacheRead, top, note }: CacheReadColumnProps) {
  const chart = useChart();
  return (
    <Tooltip>
      <TooltipTrigger delay={0} render={<div className="flex h-full items-end justify-center" />}>
        {cacheRead > 0 ? (
          <div
            className="w-[max(1px,calc(100%-4px))] max-w-10 rounded-t bg-chart-d"
            style={{ height: `${(cacheRead / top) * 100}%` }}
          />
        ) : undefined}
      </TooltipTrigger>
      <TooltipPopup besideChart={chart}>{note}</TooltipPopup>
    </Tooltip>
  );
}
