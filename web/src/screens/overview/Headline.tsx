import { ScreenHeader } from "@/components/ScreenHeader";
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
  /** Missing while the screen does not yet know what to say; only the title shows. */
  headline: HeadlineText | undefined;
}

/** Heads Overview with the headline as its standfirst, its first sentence set apart in weight. */
export function Headline({ headline }: HeadlineProps) {
  return (
    <ScreenHeader
      title="Overview"
      standfirst={
        headline === undefined ? undefined : (
          <>
            <span className="font-medium text-ink">{renderSegments(headline.heading)}</span>{" "}
            {renderSegments(headline.lede)}
          </>
        )
      }
    />
  );
}
