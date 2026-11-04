import { NotedFigure } from "@/components/NotedFigure";
import { Tooltip, TooltipPopup, TooltipTrigger } from "@/components/ui/tooltip";
import { cn } from "@/lib/cn";

import type { Tokens } from "@/api/types.gen";
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

// A gap parts the kinds of a bar only while every segment stands at least SEGMENT_MIN_PX tall;
// PLOT_PX is the height the chart frame gives its plot.
const PLOT_PX = 112;
const SEGMENT_MIN_PX = 6;

const LOWER_BOUND_FILL =
  "after:absolute after:inset-0 after:bg-[repeating-linear-gradient(135deg,transparent_0_3px,color-mix(in_srgb,var(--color-canvas)_45%,transparent)_3px_6px)]";

function KindSwatch({ kind }: { kind: TokenKind }) {
  return <span aria-hidden="true" className={cn("size-sm shrink-0 rounded", kind.fill)} />;
}

/** Names every kind beside its swatch, for the note of a panel that draws them. */
export function KindLegend() {
  return (
    <span className="inline-flex flex-wrap items-baseline gap-x-md gap-y-xxs">
      {KIND_ORDER.map((kind) => (
        <span key={kind.key} className="inline-flex items-center gap-xs">
          <KindSwatch kind={kind} />
          <span className="self-baseline">{kind.label}</span>
        </span>
      ))}
    </span>
  );
}

export interface KindRowsProps {
  kinds: KindValues;
  figureOf: (n: number) => Figure;
}

/** Lists each kind with its figure, one row apiece, for the body of a note. */
export function KindRows({ kinds, figureOf }: KindRowsProps) {
  return KIND_ORDER.map((kind) => (
    <div key={kind.key} className="flex items-center gap-xs">
      <KindSwatch kind={kind} />
      {kind.label}
      <b className="ml-auto text-body-sm font-medium tabular-nums">
        <NotedFigure figure={figureOf(kinds[kind.key])} />
      </b>
    </div>
  ));
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
  footnote?: ReactNode;
}

/** Draws the stacked kinds as one bar across a cell, with every kind's figure in its note. */
export function SplitBar({ kinds, figureOf, footnote }: SplitBarProps) {
  return (
    <Tooltip>
      <TooltipTrigger
        delay={0}
        render={
          <span tabIndex={0} className="flex h-sm w-full gap-[2px] overflow-hidden rounded" />
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
        <div className="min-w-35">
          <KindRows kinds={kinds} figureOf={figureOf} />
          {footnote === undefined ? undefined : <div className="mt-xxs">{footnote}</div>}
        </div>
      </TooltipPopup>
    </Tooltip>
  );
}
