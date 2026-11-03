import { cn } from "@/lib/cn";

import type { ReactNode } from "react";

export interface PartsProps {
  parts: readonly ReactNode[];
  className?: string;
}

/** Sets the parts apart by a middle dot; a line, wrapped ones too, never starts with a dot. */
export function Parts({ parts, className }: PartsProps) {
  return (
    // Every part leads with its dot and is pulled into the left margin, where the clip hides the dot of
    // whichever part starts a line. The parts flow as text, so a block that trims its lines trims theirs.
    <span className={cn("block max-w-full overflow-x-clip", className)}>
      <span className="-ml-5 block">
        {parts.map((part, index) => (
          <span key={index} className="inline-flex items-baseline">
            <span aria-hidden="true" className="w-5 shrink-0 text-center">
              ·
            </span>
            {part}
          </span>
        ))}
      </span>
    </span>
  );
}
