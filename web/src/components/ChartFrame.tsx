import { createContext, useContext, useState } from "react";

import { Card } from "@/components/ui/card";
import { cn } from "@/lib/cn";

import type { ReactNode, Ref } from "react";

const ChartContext = createContext<Element | null>(null);

/** The chart the calling slot is drawn in, for a note to open beside the slot; null outside a chart. */
export function useChart(): Element | null {
  return useContext(ChartContext);
}

export interface ChartSubplot {
  /** What the second plot shows, for a screen reader; it follows the chart's label. */
  label: string;
  /** The name of the second plot's value axis, above it. */
  valueName: string;
  /** The second plot's value labels from its top down to its floor, each on a rule. */
  ticks: readonly string[];
  /** One slot per category of the plot above, side by side. */
  children: ReactNode;
}

export interface ChartFrameProps {
  /** What the chart shows, for a screen reader. */
  label: string;
  /** The name of the value axis, above the plot. */
  valueName: string;
  /** The name of the category axis, under the plot. */
  categoryName: string;
  /** The value labels from the top of the plot down to its floor, each on a rule. */
  ticks: readonly string[];
  plotRef?: Ref<HTMLDivElement>;
  /** A second plot under the category axis, in the same columns, so both share the value labels' width. */
  subplot?: ChartSubplot;
  /** One slot per category, side by side, each labeling itself under the floor. */
  children: ReactNode;
}

interface PlotProps {
  ticks: readonly string[];
  plotRef?: Ref<HTMLDivElement>;
  children: ReactNode;
}

/** The value labels in their column and the plot beside them: the rules, the floor, and the slots. */
function Plot({ ticks, plotRef, children }: PlotProps) {
  const rules = ticks.slice(0, -1).map((_, index) => index / (ticks.length - 1));

  return (
    <>
      {/* Labels a line tall, spread over the plot's height plus a line, center on its rules. */}
      <div className="my-[-0.5lh] flex flex-col items-end justify-between">
        {ticks.map((tick, index) => (
          <span key={index}>{tick}</span>
        ))}
      </div>
      <div ref={plotRef} className="relative">
        {rules.map((fraction) => (
          <div
            key={fraction}
            className="absolute inset-x-0 border-t border-line"
            style={{ top: `${fraction * 100}%` }}
          />
        ))}
        <div className="absolute inset-x-0 bottom-0 border-t border-line-strong" />
        <div className="absolute inset-0 grid auto-cols-fr grid-flow-col">{children}</div>
      </div>
    </>
  );
}

/** Frames a bar chart: a card around the axis names, the rules behind the bars, and the labels. */
export function ChartFrame({
  label,
  valueName,
  categoryName,
  ticks,
  plotRef,
  subplot,
  children,
}: ChartFrameProps) {
  const [chart, setChart] = useState<HTMLDivElement | null>(null);
  return (
    <Card>
      <div className="px-md py-sm">
        {/* The value labels take a column as wide as the widest of them, so the plot starts after it. */}
        <div
          ref={setChart}
          role="img"
          aria-label={subplot === undefined ? label : `${label}; ${subplot.label}`}
          className={cn(
            "grid",
            subplot === undefined ? "h-44" : "h-62",
            "grid-cols-[auto_minmax(0,1fr)]",
            subplot === undefined
              ? "grid-rows-[--spacing(6)_minmax(0,1fr)_--spacing(10)]"
              : "grid-rows-[--spacing(6)_minmax(0,1fr)_--spacing(10)_--spacing(6)_--spacing(12)]",
            "gap-x-1.5 text-body-sm text-ink-muted",
          )}
        >
          <ChartContext value={chart}>
            <span className="col-span-2">{valueName}</span>
            <Plot ticks={ticks} plotRef={plotRef}>
              {children}
            </Plot>
            <span className="col-start-2 self-end text-center">{categoryName}</span>
            {subplot === undefined ? undefined : (
              <>
                <span className="col-span-2">{subplot.valueName}</span>
                <Plot ticks={subplot.ticks}>{subplot.children}</Plot>
              </>
            )}
          </ChartContext>
        </div>
      </div>
    </Card>
  );
}
