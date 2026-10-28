import { cn } from "@/lib/cn";

import type { Headline as HeadlineText, Segment } from "./headline";

const EMPHASIS_CLASS = {
  strong: "font-medium text-ink",
  critical: "text-error",
} as const;

function renderSegments(segments: readonly Segment[]) {
  return segments.map((segment, index) => (
    <span
      // Segments are a fixed sequence rebuilt on each render, never reordered.
      key={index}
      className={cn(segment.emphasis === undefined ? undefined : EMPHASIS_CLASS[segment.emphasis])}
    >
      {segment.text}
    </span>
  ));
}

export interface HeadlineProps {
  headline: HeadlineText;
}

export function Headline({ headline }: HeadlineProps) {
  return (
    <header>
      <h1 className="mb-xs font-heading text-h4 font-semibold text-ink">
        {renderSegments(headline.heading)}
      </h1>
      <p className="text-body-lg text-ink-body">{renderSegments(headline.lede)}</p>
    </header>
  );
}
