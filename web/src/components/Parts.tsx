import { cn } from "@/lib/cn";

import type { ReactNode } from "react";

export interface PartsProps {
  parts: readonly ReactNode[];
  className?: string;
}

/** Sets the parts apart by their gap and never by a separator character. */
export function Parts({ parts, className }: PartsProps) {
  return (
    <span className={cn("inline-flex flex-wrap items-baseline gap-x-sm gap-y-xxs", className)}>
      {parts.map((part, index) => (
        <span key={index}>{part}</span>
      ))}
    </span>
  );
}
