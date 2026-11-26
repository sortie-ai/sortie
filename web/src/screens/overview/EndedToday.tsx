import { IssueTitleLink } from "@/components/IssueTitleLink";
import { MarkSlot } from "@/components/Lane";
import { NotedFigure } from "@/components/NotedFigure";
import { Parts } from "@/components/Parts";
import { Link } from "@/components/carried-links";
import { MONO } from "@/components/ui/mono";
import { cn } from "@/lib/cn";
import { endingWord } from "@/lib/ended";
import { fmtClock } from "@/lib/format";
import { endingMarker } from "@/lib/lane";
import { ROW_GRID, ROW_LIST, rowStyles, secondLineColor } from "@/lib/rows";
import { Panel } from "@/components/Panel";

import { endedNavKey } from "./rows";

import type { EndedIssue } from "@/lib/ended";
import type { TallyLine, TallyPart } from "@/lib/tally";

const LINK =
  "text-link underline underline-offset-2 hover:text-link-hover focus-visible:outline-2 focus-visible:outline-focus-ring active:text-link-active";

interface TallyPartTextProps {
  part: TallyPart;
}

// One run of text, so the spaces between a figure and its words survive in a list.
function TallyPartText({ part }: TallyPartTextProps) {
  return (
    <span>
      {part.lead}
      {part.figure === undefined ? undefined : <NotedFigure figure={part.figure} />}
      {part.tail}
    </span>
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
    <Panel
      title="Ended today"
      meta={
        tally === undefined ? undefined : (
          <Parts
            parts={[
              ...tally.parts.map((part, index) => <TallyPartText key={index} part={part} />),
              ...(tally.cost === undefined ? [] : [<TallyPartText key="cost" part={tally.cost} />]),
            ]}
            className="wrap-anywhere"
          />
        )
      }
      note={
        <Link to="/history" className={cn("text-body-md", LINK)}>
          History
        </Link>
      }
    >
      {rows.length > 0 ? (
        <ul className={ROW_LIST}>
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
      ) : empty === "unavailable" ? (
        <p className="rounded-xl border border-line bg-canvas px-md py-lg text-body-md text-ink-body">
          Run history is not available.
        </p>
      ) : undefined}
    </Panel>
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
  const meta = [
    issue.title === null ? null : issue.displayKey,
    issue.rule === null ? null : (
      <span key="rule" className={MONO}>
        {issue.rule}
      </span>
    ),
    issue.agentKind === null ? null : (
      <span key="agent" className={MONO}>
        {issue.agentKind}
      </span>
    ),
  ].filter((part) => part !== null);

  return (
    <li
      data-nav-key={endedNavKey(issue.issueId)}
      onClick={() => onOpen(issue)}
      className={cn(ROW_GRID, rowStyles(isLook, true))}
    >
      <MarkSlot marker={marker} />
      <div className="min-w-0">
        <p className={cn("text-body-md text-ink", isUnread ? "font-bold" : "font-medium")}>
          <IssueTitleLink identifier={issue.identifier}>
            {issue.title ?? issue.displayKey}
          </IssueTitleLink>
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
