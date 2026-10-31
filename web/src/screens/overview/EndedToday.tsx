import { Link } from "react-router";

import { StandingMark } from "@/components/Lane";
import { NotedFigure } from "@/components/NotedFigure";
import { Parts } from "@/components/Parts";
import { cn } from "@/lib/cn";
import { endingWord } from "@/lib/ended";
import { fmtClock } from "@/lib/format";
import { endingMarker } from "@/lib/lane";
import { rowStyles, secondLineColor } from "@/lib/rows";

import { endedNavKey } from "./rows";

import type { EndedIssue } from "@/lib/ended";
import type { TallyLine, TallyPart } from "@/lib/tally";

const LINK =
  "text-link underline underline-offset-2 hover:text-link-hover focus-visible:outline-2 focus-visible:outline-focus-ring active:text-link-active";

interface TallyPartTextProps {
  part: TallyPart;
}

function TallyPartText({ part }: TallyPartTextProps) {
  return (
    <>
      {part.lead}
      {part.figure === undefined ? undefined : <NotedFigure figure={part.figure} />}
      {part.tail}
    </>
  );
}

export type EndedEmpty = "unavailable" | "first-run" | "none" | "loading";

export interface EndedTodayProps {
  rows: readonly EndedIssue[];
  /** Issue ids of the rows the reader has not read; those rows carry the unread dot. */
  unreadIds: ReadonlySet<string>;
  /** Why the list is empty when it is; ignored while `rows` holds issues. */
  empty: EndedEmpty;
  tally: TallyLine | undefined;
  selectedKey: string | undefined;
  showSelection: boolean;
  onOpen: (issue: EndedIssue) => void;
}

export function EndedToday({
  rows,
  unreadIds,
  empty,
  tally,
  selectedKey,
  showSelection,
  onOpen,
}: EndedTodayProps) {
  return (
    <section className="flex flex-col gap-sm">
      <div className="flex flex-wrap items-baseline gap-x-sm">
        <h2 className="font-heading text-h7 font-semibold text-ink">Ended today</h2>
        {tally === undefined ? undefined : (
          <Parts
            parts={tally.parts.map((part, index) => (
              <TallyPartText key={index} part={part} />
            ))}
            className="text-body-md text-ink-muted wrap-anywhere"
          />
        )}
        {tally?.cost === undefined ? undefined : (
          <span className="text-body-md text-ink-muted wrap-anywhere">
            <TallyPartText part={tally.cost} />
          </span>
        )}
        <Link to="/history" className={cn("ml-auto text-body-md", LINK)}>
          History
        </Link>
      </div>
      {rows.length > 0 ? (
        <ul className="divide-y divide-line overflow-hidden rounded-lg border border-line bg-canvas">
          {rows.map((issue) => (
            <EndedRow
              key={issue.issueId}
              issue={issue}
              isUnread={unreadIds.has(issue.issueId)}
              isSelected={selectedKey === endedNavKey(issue.issueId)}
              showSelection={showSelection}
              onOpen={onOpen}
            />
          ))}
        </ul>
      ) : empty === "loading" ? undefined : empty === "unavailable" ? (
        <p className="rounded-lg border border-line bg-canvas px-md py-lg text-body-md text-ink-body">
          Run history is not available.
        </p>
      ) : (
        // Holds the section's place so the page does not shift when the first ending arrives.
        <div aria-hidden="true" className="min-h-lh border border-transparent py-lg text-body-md" />
      )}
    </section>
  );
}

interface EndedRowProps {
  issue: EndedIssue;
  isUnread: boolean;
  isSelected: boolean;
  showSelection: boolean;
  onOpen: (issue: EndedIssue) => void;
}

function EndedRow({ issue, isUnread, isSelected, showSelection, onOpen }: EndedRowProps) {
  const isLook = isSelected && showSelection;
  const marker = endingMarker(issue.outcome.kind);
  const meta = [issue.title === null ? null : issue.displayKey, issue.rule, issue.agentKind].filter(
    (part) => part !== null,
  );

  return (
    <li
      data-nav-key={endedNavKey(issue.issueId)}
      onClick={() => onOpen(issue)}
      className={cn(
        "grid grid-cols-[auto_minmax(0,1fr)] gap-x-sm gap-y-xxs px-md py-sm md:grid-cols-[auto_minmax(0,1fr)_minmax(0,1fr)_auto] md:gap-x-md wrap-anywhere",
        rowStyles(isLook, true),
      )}
    >
      <span className="flex w-(--size-icon) items-center justify-center self-center">
        {marker === null ? undefined : <StandingMark marker={marker} />}
      </span>
      <div className="min-w-0">
        <p className={cn("text-body-md text-ink", isUnread ? "font-bold" : "font-medium")}>
          {issue.title ?? issue.displayKey}
        </p>
        {meta.length === 0 ? undefined : (
          <p className={cn("text-body-sm tabular-nums", secondLineColor(isLook))}>
            <Parts parts={meta} />
          </p>
        )}
      </div>
      <p className="text-body-md text-ink-body max-md:col-start-2">{endingWord(issue.outcome)}</p>
      <p className="text-body-md text-ink-body tabular-nums max-md:col-start-2 md:text-right">
        {fmtClock(issue.outcome.observed_at)}
      </p>
    </li>
  );
}
