import {
  ArrowRightIcon,
  CheckIcon,
  CircleAlertFilledIcon,
  CircleDotIcon,
  CirclePauseIcon,
  CircleXIcon,
  ClockIcon,
  LoaderCircleIcon,
  RotateCwIcon,
  TriangleAlertIcon,
  XIcon,
} from "@/components/icons";
import { cn } from "@/lib/cn";

import type { IconProps } from "@/components/icons";
import type { Mark } from "@/lib/run-status";
import type { ReactNode } from "react";

type Tone = "neutral" | "attention" | "success" | "error";

interface StatusEntry {
  Glyph: (props: IconProps) => ReactNode;
  word: string;
  tone: Tone;
}

const STATUS: Record<Mark, StatusEntry> = {
  running: { Glyph: LoaderCircleIcon, word: "Running", tone: "neutral" },
  current: { Glyph: CircleDotIcon, word: "Current", tone: "neutral" },
  waiting: { Glyph: ClockIcon, word: "Waiting", tone: "neutral" },
  approval: { Glyph: ClockIcon, word: "Waiting on approval", tone: "neutral" },
  retry: { Glyph: RotateCwIcon, word: "Retry scheduled", tone: "neutral" },
  held: { Glyph: CirclePauseIcon, word: "Held, acknowledged", tone: "neutral" },
  needs: { Glyph: CircleAlertFilledIcon, word: "Needs attention", tone: "attention" },
  stopped: {
    Glyph: CirclePauseIcon,
    word: "Stopped",
    tone: "attention",
  },
  handed: { Glyph: ArrowRightIcon, word: "Handed off", tone: "neutral" },
  succeeded: { Glyph: CheckIcon, word: "Succeeded", tone: "success" },
  failed: { Glyph: XIcon, word: "Failed", tone: "error" },
  degraded: { Glyph: CircleXIcon, word: "Degraded", tone: "error" },
  clear: { Glyph: CheckIcon, word: "All clear", tone: "neutral" },
  stalled: { Glyph: TriangleAlertIcon, word: "No events for longer than usual", tone: "neutral" },
};

const GLYPH_TONE: Record<Tone, string> = {
  neutral: "text-ink-body",
  attention: "text-warning-fill",
  success: "text-success-fill",
  error: "text-error-fill",
};

// Green marks only a glyph, so a success label stays neutral.
const LABEL_TONE: Record<Tone, string> = {
  neutral: "text-ink-body",
  attention: "text-warning",
  success: "text-ink-body",
  error: "text-error",
};

export function markWord(mark: Mark): string {
  return STATUS[mark].word;
}

export interface StatusMarkProps {
  mark: Mark;
  /** Visible text beside the glyph; without it the mark names itself to screen readers and a tooltip. */
  label?: ReactNode;
  /** Tooltip of the glyph-only form; defaults to the mark's word. */
  title?: string;
  className?: string;
}

export function StatusMark({ mark, label, title, className }: StatusMarkProps) {
  const { Glyph, word, tone } = STATUS[mark];
  const isSpinning = mark === "running";
  const hasLabel = label !== undefined;

  return (
    <span
      className={cn("inline-flex items-center gap-xs", className)}
      title={hasLabel ? undefined : (title ?? word)}
    >
      <Glyph
        className={cn(GLYPH_TONE[tone], isSpinning && "animate-spin motion-reduce:animate-none")}
        style={isSpinning ? { animationDuration: "var(--duration-spin)" } : undefined}
      />
      {hasLabel ? (
        <span className={LABEL_TONE[tone]}>{label}</span>
      ) : (
        <span className="sr-only">{word}</span>
      )}
    </span>
  );
}
