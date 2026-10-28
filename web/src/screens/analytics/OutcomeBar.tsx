import { Card } from "@/components/ui/card";
import { cn } from "@/lib/cn";
import { fmtCount } from "@/lib/format";

import type { Summary } from "@/api/types.gen";

interface Segment {
  label: string;
  count: number;
  fill: string;
}

function segmentsOf(summary: Summary): Segment[] {
  return [
    { label: "Succeeded", count: summary.succeeded, fill: "bg-success-fill" },
    {
      label: "Stopped",
      count: summary.stopped,
      fill: "bg-warning-fill",
    },
    { label: "Cancelled", count: summary.cancelled, fill: "bg-info-fill" },
    { label: "Failed", count: summary.failed, fill: "bg-error-fill" },
  ].filter((segment) => segment.count > 0);
}

function roundingOf(index: number, count: number): string | undefined {
  if (count === 1) {
    return "rounded";
  }
  if (index === 0) {
    return "rounded-l";
  }
  return index === count - 1 ? "rounded-r" : undefined;
}

export interface OutcomeBarProps {
  summary: Summary;
}

/** Renders nothing while no outcome has runs. */
export function OutcomeBar({ summary }: OutcomeBarProps) {
  const segments = segmentsOf(summary);
  if (segments.length === 0) {
    return undefined;
  }

  return (
    <Card title="Outcomes">
      <div className="flex flex-col gap-xs px-md py-sm">
        <div className="flex flex-wrap gap-x-md gap-y-xxs text-body-sm text-ink-body">
          {segments.map((segment) => (
            <span key={segment.label} className="inline-flex items-center gap-xs">
              <span aria-hidden="true" className={cn("size-sm rounded", segment.fill)} />
              {segment.label} {fmtCount(segment.count)}
            </span>
          ))}
        </div>
        <div aria-hidden="true" className="flex h-4 gap-xxs">
          {segments.map((segment, index) => (
            <div
              key={segment.label}
              className={cn("min-w-xxs basis-0", segment.fill, roundingOf(index, segments.length))}
              style={{ flexGrow: segment.count }}
            />
          ))}
        </div>
      </div>
    </Card>
  );
}
