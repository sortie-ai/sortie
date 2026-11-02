import { Card } from "@/components/ui/card";

import type { ReactNode, Ref } from "react";

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
  /** One slot per category, side by side, each labeling itself under the floor. */
  children: ReactNode;
}

/** Frames a bar chart: a card around the axis names, the rules behind the bars, and the labels. */
export function ChartFrame({
  label,
  valueName,
  categoryName,
  ticks,
  plotRef,
  children,
}: ChartFrameProps) {
  const rules = ticks.slice(0, -1).map((_, index) => index / (ticks.length - 1));

  return (
    <Card>
      <div className="px-md py-sm">
        {/* The value labels take a column as wide as the widest of them, so the plot starts after it. */}
        <div
          role="img"
          aria-label={label}
          className="grid h-44 grid-cols-[auto_minmax(0,1fr)] grid-rows-[--spacing(6)_minmax(0,1fr)_--spacing(10)] gap-x-1.5 text-body-sm text-ink-muted"
        >
          <span className="col-span-2">{valueName}</span>
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
          <span className="col-start-2 self-end text-center">{categoryName}</span>
        </div>
      </div>
    </Card>
  );
}
