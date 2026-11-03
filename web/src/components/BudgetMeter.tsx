import { cn } from "@/lib/cn";

import type { BudgetLine } from "@/lib/budget";
import type { ReactNode } from "react";

export interface FigureLineProps {
  name: string;
  children: ReactNode;
}

/** A figure's name and its value on one line, so figures with a budget and without read alike. */
export function FigureLine({ name, children }: FigureLineProps) {
  return (
    <div className="grid grid-cols-[minmax(0,1fr)_auto] gap-x-sm text-body-md">
      <span className="text-ink">{name}</span>
      <span className="text-ink-body tabular-nums">{children}</span>
    </div>
  );
}

export interface BudgetMeterProps {
  name: string;
  line: BudgetLine;
  format: (n: number) => string;
  text?: ReactNode;
}

/** A missing or non-positive limit renders nothing. */
export function BudgetMeter({ name, line, format, text }: BudgetMeterProps) {
  const { used, limit } = line;
  if (used === null || limit === null || limit <= 0) {
    return undefined;
  }
  const isFull = used >= limit;
  return (
    <div className="flex flex-col gap-xxs">
      <FigureLine name={name}>{text ?? `${format(used)} of ${format(limit)}`}</FigureLine>
      <span
        aria-hidden="true"
        className={cn(
          "h-1 overflow-hidden rounded-full",
          isFull ? "bg-warning-container" : "bg-surface-raised",
        )}
      >
        <span
          className={cn("block h-full rounded-full", isFull ? "bg-warning-fill" : "bg-primary")}
          style={{ width: `${Math.min(100, (used / limit) * 100)}%` }}
        />
      </span>
    </div>
  );
}
