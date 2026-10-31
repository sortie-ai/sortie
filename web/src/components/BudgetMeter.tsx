import { cn } from "@/lib/cn";

import type { BudgetLine } from "@/lib/budget";
import type { ReactNode } from "react";

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
    <div className="grid grid-cols-[minmax(0,1fr)_auto] gap-x-sm gap-y-xxs text-body-md">
      <span className="text-ink">{name}</span>
      <span className="text-ink-body tabular-nums">
        {text ?? `${format(used)} of ${format(limit)}`}
      </span>
      <span
        aria-hidden="true"
        className={cn(
          "col-span-2 h-1 overflow-hidden rounded-full",
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
