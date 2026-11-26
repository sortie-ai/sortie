import { useStateQuery } from "@/api/queries";
import { LABEL_SHAPE } from "@/components/TrackerLabel";
import { cn } from "@/lib/cn";
import { stateToneOf } from "@/lib/state-tone";

import type { StateTone } from "@/lib/state-tone";

const TONE: Record<StateTone, string> = {
  idle: "border-line-medium bg-surface-raised text-ink-body",
  progress: "border-info-border bg-info-container text-info",
  done: "border-success-border bg-success-container text-success",
};

export interface StateBadgeProps {
  state: string;
  className?: string;
}

/** Shows a tracker state as a label, spelled as the tracker does, in the tone of how far the issue has come. */
export function StateBadge({ state, className }: StateBadgeProps) {
  const { data } = useStateQuery();
  const tone = stateToneOf(state, data?.config?.tracker);
  return (
    <span className={cn(LABEL_SHAPE, TONE[tone], className)}>
      <span className="truncate">{state}</span>
    </span>
  );
}
