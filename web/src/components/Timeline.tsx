import { cn } from "@/lib/cn";

import type { ReactNode } from "react";

export type BeatNode = "open" | "solid" | "now" | "attention";

const NODE: Record<BeatNode, string> = {
  open: "border-ink-muted bg-(--ground)",
  solid: "border-ink-body bg-ink-body",
  now: "border-primary bg-primary shadow-[0_0_0_3px_var(--ground)]",
  attention: "border-warning-fill bg-warning-fill",
};

/** The frame of a beat that holds a card; the beat's node takes the same inset, so it stays on the card's first line. */
const FRAME = "rounded-xl border border-line bg-canvas px-md py-sm";
const FRAME_INSET = "box-content border-y border-transparent py-sm";

const TIME_COLUMN = "[--time:56px] [--beat-gap:--spacing(2)] max-md:[--time:48px]";

/** Sets `--edge`, the left edge of a beat's time, for a head that has to stand on it; it assumes `text-body-sm` and leaves room for a sign. */
export const TIME_EDGE = cn(
  TIME_COLUMN,
  "text-body-sm [--edge:max(calc(var(--time)-2.86em),--spacing(3))]",
);

/** Draws a story's beats down one rule, which runs through every node's center. */
export function Timeline({ children }: { children: ReactNode }) {
  return (
    <ul
      className={cn(
        "relative [--node:20px] before:absolute before:top-xs before:bottom-xs before:left-[calc(var(--time)+var(--beat-gap)+var(--node)/2)] before:w-px before:-translate-x-1/2 before:bg-line max-md:[--node:16px]",
        TIME_COLUMN,
      )}
    >
      {children}
    </ul>
  );
}

export interface BeatProps {
  time: string;
  node: BeatNode;
  /** Whether the beat frames what happened as a card. */
  isFramed?: boolean;
  children: ReactNode;
}

export function Beat({ time, node, isFramed = false, children }: BeatProps) {
  return (
    <li className="grid grid-cols-[var(--time)_var(--node)_minmax(0,1fr)] items-start gap-x-(--beat-gap) py-xs text-body-md">
      {/* The time stands on the baseline of the beat's first line, card or not. */}
      <span className="self-baseline text-right text-body-sm text-ink-muted tabular-nums">
        {time}
      </span>
      {/* A box one line tall centers the node on the beat's first line. */}
      <span aria-hidden="true" className={cn("flex h-lh justify-center", isFramed && FRAME_INSET)}>
        <span
          className={cn(
            "relative z-(--z-raised) size-2.25 self-center rounded-full border-[1.5px]",
            NODE[node],
          )}
        />
      </span>
      <div className={cn("min-w-0 self-baseline", isFramed && FRAME)}>{children}</div>
    </li>
  );
}
