import { Collapsible } from "@base-ui/react/collapsible";
import { useEffect, useLayoutEffect, useRef, useState } from "react";

import { ChevronDownIcon } from "@/components/icons";
import { ServedText } from "@/components/ServedText";
import { StatusMark } from "@/components/StatusMark";
import { Button } from "@/components/ui/button";
import { cn } from "@/lib/cn";
import { fmtClockSec, fmtCount, fmtDuration, fmtTokens, parseInstant, plural } from "@/lib/format";
import { runOutcome } from "@/lib/run-status";
import { now } from "@/lib/server-clock";

import { foldPhraseOf, ToolLine } from "./ToolLine";

import type { RunEventResponse } from "@/api/types.gen";
import type { Timeline, TimelineLine } from "./timeline";
import type { ReactNode } from "react";

const FRESH_EVENT_MS = 10_000;
const FOLLOW_SLACK_PX = 24;
const RELEASE_EVENTS = ["pointerup", "pointercancel", "touchend", "touchcancel"] as const;

function isFreshAt(at: string, isLive: boolean): boolean {
  const instant = parseInstant(at);
  return isLive && instant !== null && now() - instant <= FRESH_EVENT_MS;
}

function newestSeqOf(timeline: Timeline): number {
  for (let index = timeline.lines.length - 1; index >= 0; index -= 1) {
    const line = timeline.lines[index];
    if (line.kind === "tools") {
      return line.calls[line.calls.length - 1].seq;
    }
    if (line.kind !== "gap") {
      return line.event.seq;
    }
  }
  return 0;
}

function countEventsAfter(timeline: Timeline, seq: number): number {
  let count = 0;
  for (const line of timeline.lines) {
    if (line.kind === "tools") {
      count += line.calls.filter((call) => call.seq > seq).length;
    } else if (line.kind !== "gap" && line.event.seq > seq) {
      count += 1;
    }
  }
  return count;
}

interface RowProps {
  title?: string;
  /** The instant shown in the time cell; without it the content spans the row. */
  time?: string;
  isAsk?: boolean;
  isFresh?: boolean;
  children: ReactNode;
}

function Row({ title, time, isAsk = false, isFresh = false, children }: RowProps) {
  return (
    <div
      title={title}
      className={cn(
        "grid grid-cols-[64px_minmax(0,1fr)] items-baseline gap-sm border-t border-line py-xs text-body-md first:border-t-0",
        isAsk && "bg-warning-container text-warning",
        isFresh &&
          "transition-[opacity,translate] duration-(--duration-enter) ease-enter starting:translate-y-(--offset-anchored) starting:opacity-0",
      )}
    >
      {time === undefined ? undefined : (
        <span
          className={cn("text-body-sm tabular-nums", isAsk ? "text-warning" : "text-ink-muted")}
        >
          {fmtClockSec(time)}
        </span>
      )}
      <div className={cn("min-w-0", time === undefined && "col-span-full")}>{children}</div>
    </div>
  );
}

interface TurnItemsProps {
  turn: number | null;
  tokens: number | null;
  seconds: number | null;
  className?: string;
}

function TurnItems({ turn, tokens, seconds, className }: TurnItemsProps) {
  return (
    <>
      {turn === null ? undefined : (
        <span className={cn("whitespace-nowrap", className)}>{`Turn ${turn}`}</span>
      )}
      {tokens === null ? undefined : (
        <span className={cn("whitespace-nowrap", className)}>{`${fmtTokens(tokens)} tokens`}</span>
      )}
      {seconds === null ? undefined : (
        <span className={cn("whitespace-nowrap", className)}>{fmtDuration(seconds)}</span>
      )}
    </>
  );
}

interface EventLineProps {
  event: RunEventResponse;
  isFresh: boolean;
}

function EventLine({ event, isFresh }: EventLineProps) {
  let content: ReactNode;
  switch (event.event) {
    case "notification":
    case "other_message":
      content = (
        <span className="text-ink">
          <ServedText text={event.text} />
        </span>
      );
      break;
    case "tool_result":
      content = <ToolLine call={event} />;
      break;
    case "startup_failed":
      content = <StatusMark mark="failed" label={<ServedText text={event.text} />} />;
      break;
    default:
      content = (
        <span className="text-ink-body">
          <ServedText text={event.text} />
        </span>
      );
  }

  return (
    <Row title={event.event} time={event.at} isFresh={isFresh}>
      {content}
    </Row>
  );
}

interface FailedLeadProps {
  event: RunEventResponse;
  end: PaneEnd | undefined;
}

/** Marks a failed turn end as failed, except the one that closes a finished session, which carries the run's own outcome. */
function FailedLead({ event, end }: FailedLeadProps) {
  if (end === undefined) {
    return <StatusMark mark="failed" label={<ServedText text={event.text} />} />;
  }
  const { word, mark } = runOutcome(end);
  return mark === null ? <span>{word}</span> : <StatusMark mark={mark} label={word} />;
}

interface TurnLineProps {
  line: Extract<TimelineLine, { kind: "turn" }>;
  /** How the run ended, given for the line that closes a finished session. */
  end?: PaneEnd;
  isFresh: boolean;
}

function TurnLine({ line, end, isFresh }: TurnLineProps) {
  const { event, turn, tokens, seconds } = line;
  const isFailed = event.event === "turn_failed" || event.event === "turn_ended_with_error";
  const isAsk = event.event === "turn_input_required";
  const isNumbered = event.event === "turn_completed" && turn !== null;
  let lead: ReactNode;
  if (isFailed) {
    lead = <FailedLead event={event} end={end} />;
  } else if (isNumbered) {
    lead = <span>{`Turn ${turn} completed`}</span>;
  } else {
    lead = (
      <span>
        <ServedText text={event.text} />
      </span>
    );
  }

  return (
    <Row title={event.event} time={event.at} isAsk={isAsk} isFresh={isFresh}>
      <div
        className={cn(
          "flex flex-wrap items-baseline gap-x-sm gap-y-xxs",
          !isFailed && !isAsk && "text-body-sm text-ink-muted",
        )}
      >
        {lead}
        <TurnItems
          turn={isNumbered ? null : turn}
          tokens={tokens}
          seconds={seconds}
          className={isFailed ? "text-body-sm text-ink-muted" : undefined}
        />
        {isNumbered && event.message.trim() !== "" ? (
          <span className="basis-full text-body-md text-ink">
            <ServedText text={event.message} />
          </span>
        ) : undefined}
      </div>
    </Row>
  );
}

function toolDurationOf(calls: readonly RunEventResponse[]): string | undefined {
  const measured = calls.filter((call) => call.duration_ms !== undefined);
  if (measured.length === 0) {
    return undefined;
  }
  const seconds = measured.reduce((sum, call) => sum + (call.duration_ms ?? 0), 0) / 1000;
  return fmtDuration(seconds);
}

interface ToolsLineProps {
  line: Extract<TimelineLine, { kind: "tools" }>;
  isOpen: boolean;
  onOpenChange: (isOpen: boolean) => void;
  isFresh: boolean;
}

function ToolsLine({ line, isOpen, onOpenChange, isFresh }: ToolsLineProps) {
  const { calls } = line;
  const failed = calls.filter((call) => call.tool_error === true);
  const duration = toolDurationOf(calls);

  return (
    <Row title="tool_result" time={calls[0].at} isFresh={isFresh}>
      <Collapsible.Root open={isOpen} onOpenChange={onOpenChange}>
        <Collapsible.Trigger className="flex w-full cursor-pointer items-baseline gap-xs rounded text-left text-ink-body transition-colors duration-(--duration-feedback) hover:text-ink focus-visible:-outline-offset-2 focus-visible:outline-2 focus-visible:outline-focus-ring">
          <ChevronDownIcon
            className={cn(
              "self-center transition-transform duration-(--duration-feedback)",
              !isOpen && "-rotate-90",
            )}
          />
          <span className="flex min-w-0 flex-wrap items-baseline gap-x-sm gap-y-xxs">
            <span>{foldPhraseOf(calls)}</span>
            {duration === undefined ? undefined : <span>{duration}</span>}
            {failed.length === 0 ? undefined : (
              <StatusMark mark="failed" label={`${failed.length} failed`} />
            )}
          </span>
        </Collapsible.Trigger>
        <Collapsible.Panel>
          <div className="grid gap-xxs pt-xs text-body-sm">
            {calls.map((call) => (
              <ToolLine key={call.seq} call={call} />
            ))}
          </div>
        </Collapsible.Panel>
      </Collapsible.Root>
    </Row>
  );
}

function GapLine({ missing }: { missing: number }) {
  return (
    <Row>
      <span className="text-ink-muted">{`${plural(missing, "event", "events")} not retained`}</span>
    </Row>
  );
}

export interface PaneEnd {
  status: string;
  declared_outcome: string | null;
}

interface FootProps {
  isLive: boolean;
  isPaused: boolean;
  isFollowing: boolean;
  lastEventAt: string | null;
  heldEvents: number;
  isEmpty: boolean;
}

function Foot({ isLive, isPaused, isFollowing, lastEventAt, heldEvents, isEmpty }: FootProps) {
  let content: ReactNode;
  if (isEmpty) {
    content = undefined;
  } else if (isLive) {
    content = (
      <>
        <span className="inline-flex items-center gap-xs">
          <span
            aria-hidden="true"
            className={cn(
              "size-xs shrink-0 rounded-full",
              isPaused ? "bg-ink-muted" : "bg-success-fill",
            )}
          />
          {isPaused ? "Paused" : "Live"}
        </span>
        {lastEventAt === null ? undefined : (
          <span>{`last event at ${fmtClockSec(lastEventAt)}`}</span>
        )}
        {isFollowing ? <span>following new events</span> : undefined}
      </>
    );
  } else {
    content = <span>{plural(heldEvents, "event", "events")}</span>;
  }

  return (
    <div className="flex min-h-8 flex-wrap items-center gap-x-sm gap-y-xxs py-xs text-body-sm text-ink-muted">
      {content}
    </div>
  );
}

interface FollowState {
  isFollowing: boolean;
  unseen: number;
  newestSeq: number;
}

export interface EventPaneProps {
  timeline: Timeline;
  /** How a finished session ended; `null` while the session is live. */
  end: PaneEnd | null;
  /** Whether the stream answered that the run's events are not retained. */
  isNotRetained: boolean;
  isPaused: boolean;
  lastEventAt: string | null;
}

/** Holds follow and expansion state for one session, so key it by the session. */
export function EventPane({ timeline, end, isNotRetained, isPaused, lastEventAt }: EventPaneProps) {
  const isLive = end === null;
  const paneRef = useRef<HTMLDivElement>(null);
  const isHeldRef = useRef(false);
  const newestSeq = newestSeqOf(timeline);
  const [follow, setFollow] = useState<FollowState>({ isFollowing: true, unseen: 0, newestSeq });
  const [expanded, setExpanded] = useState<ReadonlySet<string>>(() => new Set());

  if (follow.newestSeq !== newestSeq) {
    const added = follow.isFollowing ? 0 : countEventsAfter(timeline, follow.newestSeq);
    setFollow({ ...follow, newestSeq, unseen: follow.unseen + added });
  }

  useEffect(() => {
    const release = () => {
      isHeldRef.current = false;
    };
    for (const type of RELEASE_EVENTS) {
      window.addEventListener(type, release);
    }
    return () => {
      for (const type of RELEASE_EVENTS) {
        window.removeEventListener(type, release);
      }
    };
  }, []);

  // Before paint, so a newly added line is never seen out of view.
  useLayoutEffect(() => {
    const pane = paneRef.current;
    if (pane !== null && isLive && follow.isFollowing && !isHeldRef.current) {
      pane.scrollTop = pane.scrollHeight;
    }
  }, [isLive, follow.isFollowing, newestSeq]);

  const stopFollowing = () => {
    if (isLive) {
      setFollow((current) =>
        current.isFollowing ? { ...current, isFollowing: false, unseen: 0 } : current,
      );
    }
  };

  const handleScroll = () => {
    const pane = paneRef.current;
    if (pane === null || !isLive) {
      return;
    }
    const isAtBottom = pane.scrollHeight - pane.scrollTop - pane.clientHeight <= FOLLOW_SLACK_PX;
    setFollow((current) =>
      current.isFollowing === isAtBottom
        ? current
        : { ...current, isFollowing: isAtBottom, unseen: 0 },
    );
  };

  // Upward intent ends following before the scroll lands, so a burst of events
  // cannot pin the pane against a reader scrolling up.
  const handleWheel = (event: { deltaY: number }) => {
    if (event.deltaY < 0) {
      stopFollowing();
    }
  };

  const handleKeyDown = (event: { key: string }) => {
    if (event.key === "ArrowUp" || event.key === "PageUp" || event.key === "Home") {
      stopFollowing();
    }
  };

  // A pin never runs while a pointer is held, so a text selection is not fought.
  const handleHold = () => {
    isHeldRef.current = true;
  };

  const handleJump = () => {
    const pane = paneRef.current;
    if (pane !== null) {
      pane.scrollTop = pane.scrollHeight;
      pane.focus();
    }
    setFollow((current) => ({ ...current, isFollowing: true, unseen: 0 }));
  };

  const handleOpenChange = (key: string, isOpen: boolean) => {
    setExpanded((current) => {
      const next = new Set(current);
      if (isOpen) {
        next.add(key);
      } else {
        next.delete(key);
      }
      return next;
    });
  };

  const isEmpty = isNotRetained && timeline.lines.length === 0;
  const isJumpShown = isLive && !follow.isFollowing;

  return (
    <div>
      <div
        ref={paneRef}
        role="region"
        aria-label="Events"
        aria-live="off"
        tabIndex={0}
        onScroll={handleScroll}
        onWheel={handleWheel}
        onKeyDown={handleKeyDown}
        onPointerDown={handleHold}
        onTouchStart={handleHold}
        className="relative max-h-[calc(100vh-250px)] min-h-70 overflow-y-auto rounded-xl border border-line bg-canvas px-md py-xs focus-visible:-outline-offset-2 focus-visible:outline-2 focus-visible:outline-focus-ring"
      >
        {isEmpty ? (
          <Row>
            <span className="text-ink-muted">No events retained</span>
          </Row>
        ) : undefined}
        {timeline.lines.map((line, index) => {
          switch (line.kind) {
            case "event":
              return (
                <EventLine
                  key={line.key}
                  event={line.event}
                  isFresh={isFreshAt(line.event.at, isLive)}
                />
              );
            case "turn":
              return (
                <TurnLine
                  key={line.key}
                  line={line}
                  end={index === timeline.lines.length - 1 ? (end ?? undefined) : undefined}
                  isFresh={isFreshAt(line.event.at, isLive)}
                />
              );
            case "tools":
              return (
                <ToolsLine
                  key={line.key}
                  line={line}
                  isOpen={expanded.has(line.key)}
                  onOpenChange={(isOpen) => handleOpenChange(line.key, isOpen)}
                  isFresh={isFreshAt(line.calls[line.calls.length - 1].at, isLive)}
                />
              );
            case "gap":
              return <GapLine key={line.key} missing={line.missing} />;
          }
        })}
        {isJumpShown ? (
          <div className="sticky bottom-xs ml-auto w-max transition-[opacity,translate] duration-(--duration-enter-small) ease-enter starting:translate-y-(--offset-anchored) starting:opacity-0">
            <Button variant="primary" onClick={handleJump}>
              Jump to latest
              {follow.unseen > 0 ? <span>{`${fmtCount(follow.unseen)} new`}</span> : undefined}
            </Button>
          </div>
        ) : undefined}
      </div>
      <Foot
        isLive={isLive}
        isPaused={isPaused}
        isFollowing={follow.isFollowing}
        lastEventAt={lastEventAt}
        heldEvents={countEventsAfter(timeline, 0)}
        isEmpty={isEmpty}
      />
    </div>
  );
}
