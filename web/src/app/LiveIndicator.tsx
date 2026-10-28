import { useId } from "react";

import { useStateQuery } from "@/api/queries";
import { useLive, useNow } from "@/app/live";
import { Tooltip, TooltipPopup, TooltipTrigger } from "@/components/ui/tooltip";
import { cn } from "@/lib/cn";
import { fmtClockSec, fmtDue, parseInstant } from "@/lib/format";

import type { StateResponse } from "@/api/types.gen";

type Tone = "live" | "idle" | "error";

interface Indicator {
  word: string;
  tip: string;
  tone: Tone;
}

const TONE_DOT: Record<Tone, string> = {
  live: "bg-success-fill",
  idle: "bg-ink-muted",
  error: "bg-error-fill",
};

const TONE_WORD: Record<Tone, string> = {
  live: "text-success",
  idle: "text-ink",
  error: "text-ink",
};

function nextPoll(state: StateResponse | undefined, now: number): string | null {
  const due = parseInstant(state?.health?.poll.next_poll_at);
  return due === null ? null : fmtDue((due - now) / 1000);
}

interface IndicatorInput {
  paused: boolean;
  isHolding: boolean;
  isStreamOpen: boolean;
  state: StateResponse | undefined;
  lastUpdateAt: number | null;
  hasFailed: boolean;
  now: number;
}

function indicatorFor(input: IndicatorInput): Indicator {
  const { state, now } = input;
  if (input.paused) {
    return { word: "Paused", tip: "Paused. Press P to resume.", tone: "idle" };
  }
  if (input.isHolding) {
    return { word: "Holding", tip: "Holding still while you read.", tone: "idle" };
  }
  if (!input.isStreamOpen) {
    return { word: "Reconnecting", tip: "Reconnecting to live updates.", tone: "idle" };
  }
  if (input.hasFailed) {
    const since =
      input.lastUpdateAt === null ? "" : `, last update ${fmtClockSec(input.lastUpdateAt)}`;
    return { word: "Not updating", tip: `Not updating${since}.`, tone: "error" };
  }
  const next = nextPoll(state, now);
  if (state?.health?.poll.status === "fail") {
    return {
      word: "Poll failing",
      tip: `Poll failing, next try ${next ?? "unknown"}.`,
      tone: "error",
    };
  }
  const poll = next === null ? "" : `, next poll ${next}`;
  return { word: "Live", tip: `Live${poll}. Press P to pause.`, tone: "live" };
}

export interface LiveIndicatorProps {
  /** True while the peek holds Overview still. */
  isHolding: boolean;
  onTogglePause: () => void;
}

export function LiveIndicator({ isHolding, onTogglePause }: LiveIndicatorProps) {
  const { paused, streamStatus } = useLive();
  const { data, isError, dataUpdatedAt } = useStateQuery();
  const now = useNow();
  const { word, tip, tone } = indicatorFor({
    paused,
    isHolding,
    isStreamOpen: streamStatus === "open",
    state: data,
    lastUpdateAt: dataUpdatedAt === 0 ? null : dataUpdatedAt,
    hasFailed: isError,
    now,
  });

  const tipId = useId();

  return (
    <>
      <Tooltip>
        <TooltipTrigger
          render={
            <button
              type="button"
              onClick={onTogglePause}
              aria-describedby={tipId}
              className="inline-flex h-(--size-control) cursor-pointer items-center gap-xs rounded-lg px-sm text-body-sm whitespace-nowrap transition-colors duration-(--duration-feedback) hover:bg-state-hover focus-visible:-outline-offset-1 focus-visible:outline-2 focus-visible:outline-focus-ring active:bg-state-pressed"
            />
          }
        >
          <span className="text-ink-muted">Events</span>
          <span
            aria-hidden="true"
            className={cn("size-xs shrink-0 rounded-full", TONE_DOT[tone])}
          />
          <span className={cn("font-medium", TONE_WORD[tone])}>{word}</span>
        </TooltipTrigger>
        <TooltipPopup>{tip}</TooltipPopup>
      </Tooltip>
      <span id={tipId} className="sr-only">
        {tip}
      </span>
    </>
  );
}
