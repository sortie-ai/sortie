import { useStateQuery } from "@/api/queries";
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

export function StateBadge({ state, className }: StateBadgeProps) {
  const { data } = useStateQuery();
  const tone = stateToneOf(state, data?.config?.tracker);
  return (
    <span
      className={cn(
        "inline-flex h-5 max-w-full items-center rounded-[3px] border px-[6px] align-middle text-badge font-semibold whitespace-nowrap uppercase",
        TONE[tone],
        className,
      )}
    >
      <span className="truncate">{state}</span>
    </span>
  );
}
