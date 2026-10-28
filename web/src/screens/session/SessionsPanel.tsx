import { useEffect, useState } from "react";
import { Link, useLocation } from "react-router";

import { useHistoryRunsQuery, useIssueQuery } from "@/api/queries";
import { useNow } from "@/app/live";
import { storyRunsOf } from "@/components/IssueStory";
import { Panel } from "@/components/Panel";
import { StatusMark } from "@/components/StatusMark";
import { Button } from "@/components/ui/button";
import { Table, TableBody, TableCell, TableRow } from "@/components/ui/table";
import { fmtDuration, lowerFirst, parseInstant } from "@/lib/format";
import { sessionLinkOf } from "@/lib/issue-link";
import { numberedRunsOf } from "@/lib/run-number";
import { runOutcome } from "@/lib/run-status";

import type { IssueDetailResponse, RunsResponse } from "@/api/types.gen";
import type { Mark } from "@/lib/run-status";

const LINK_STYLES =
  "text-link underline underline-offset-2 hover:text-link-hover focus-visible:outline-2 focus-visible:outline-focus-ring active:text-link-active";

interface SessionRow {
  key: string;
  number: number;
  dispatchId: string | null;
  startedAt: string;
  endedAt: string | null;
  word: string;
  mark: Mark | null;
}

function secondsBetween(from: string, to: number | null): number | null {
  const start = parseInstant(from);
  return start === null || to === null || to < start ? null : (to - start) / 1000;
}

function rowsOf(detail: IssueDetailResponse, pages: readonly RunsResponse[]): SessionRow[] {
  const { history, running } = detail;
  const total = history?.totals.run_count ?? 0;
  const finished = numberedRunsOf(storyRunsOf(history?.runs ?? [], pages), total)
    .reverse()
    .map(({ run, number }): SessionRow => {
      const { word, mark } = runOutcome(run);
      return {
        key: String(run.run_id),
        number,
        dispatchId: run.dispatch_id || null,
        startedAt: run.started_at,
        endedAt: run.completed_at,
        word: lowerFirst(word),
        mark,
      };
    });
  if (running === null) {
    return finished;
  }
  const live: SessionRow = {
    key: "live",
    number: total + 1,
    dispatchId: running.dispatch_id,
    startedAt: running.started_at,
    endedAt: null,
    word: "running",
    mark: "running",
  };
  return [live, ...finished];
}

function LiveDuration({ startedAt }: { startedAt: string }) {
  return fmtDuration(secondsBetween(startedAt, useNow()));
}

interface SessionRowViewProps {
  row: SessionRow;
  identifier: string;
  isCurrent: boolean;
}

function SessionRowView({ row, identifier, isCurrent }: SessionRowViewProps) {
  const location = useLocation();
  const name = `Session ${row.number}`;
  const link =
    isCurrent || row.dispatchId === null
      ? undefined
      : sessionLinkOf(identifier, row.dispatchId, location);

  return (
    <TableRow>
      <TableCell>
        {link === undefined ? (
          <strong className={isCurrent ? "font-bold text-ink" : "font-medium"}>{name}</strong>
        ) : (
          <Link to={link.to} state={link.state} className={LINK_STYLES}>
            {name}
          </Link>
        )}
      </TableCell>
      <TableCell isWrapping>
        {row.mark === null ? row.word : <StatusMark mark={row.mark} label={row.word} />}
      </TableCell>
      <TableCell look="number">
        {row.endedAt === null ? (
          <LiveDuration startedAt={row.startedAt} />
        ) : (
          fmtDuration(secondsBetween(row.startedAt, parseInstant(row.endedAt)))
        )}
      </TableCell>
    </TableRow>
  );
}

/** Props of {@link SessionsPanel}. */
export interface SessionsPanelProps {
  identifier: string;
  /** The dispatch ID of the session on screen, whose row is not a link. */
  dispatchId: string;
}

/**
 * Renders every session of the issue, newest first, each named by the run
 * number the issue page gives it. It draws nothing while the issue has a
 * single session. Older runs load on request.
 */
export function SessionsPanel({ identifier, dispatchId }: SessionsPanelProps) {
  const { data: detail } = useIssueQuery(identifier);
  const [wanted, setWanted] = useState(0);
  const older = useHistoryRunsQuery(detail?.issue_id ?? "", {}, wanted > 0);
  const { hasNextPage, isFetching, isError, fetchNextPage } = older;
  const loadedPages = older.data?.pages.length ?? 0;

  useEffect(() => {
    if (wanted > loadedPages && loadedPages > 0 && hasNextPage && !isFetching && !isError) {
      void fetchNextPage();
    }
  }, [wanted, loadedPages, hasNextPage, isFetching, isError, fetchNextPage]);

  if (detail === undefined) {
    return undefined;
  }
  const { history, running } = detail;
  const total = history?.totals.run_count ?? 0;
  if (total + (running === null ? 0 : 1) < 2) {
    return undefined;
  }

  const pages = older.data?.pages ?? [];
  const rows = rowsOf(detail, pages);
  const isExhausted = wanted > 0 && older.data !== undefined && !hasNextPage;
  const canShowOlder =
    history !== null &&
    history.next_cursor !== null &&
    rows.length - (running === null ? 0 : 1) < total &&
    !isExhausted;

  const handleShowOlder = () => {
    if (isError) {
      void (older.data === undefined ? older.refetch() : fetchNextPage());
      return;
    }
    setWanted(Math.max(wanted, loadedPages, 1) + 1);
  };

  return (
    <Panel title="Sessions">
      <Table aria-label="Sessions" isCompact>
        <TableBody>
          {rows.map((row) => (
            <SessionRowView
              key={row.key}
              row={row}
              identifier={identifier}
              isCurrent={row.dispatchId === dispatchId}
            />
          ))}
        </TableBody>
      </Table>
      {canShowOlder ? (
        <div className="flex flex-wrap items-center gap-x-sm gap-y-xs">
          <Button disabled={isFetching} onClick={handleShowOlder}>
            Show older
          </Button>
          {isError ? (
            <p className="text-body-sm text-ink-body">Older runs could not be read.</p>
          ) : undefined}
        </div>
      ) : undefined}
    </Panel>
  );
}
