import { useEffect } from "react";

import { ApiError } from "@/api/client";
import { useHistoryRunsQuery, useIssueQuery } from "@/api/queries";
import { storyRunsOf } from "@/components/IssueStory";
import { numberedRunsOf } from "@/lib/run-number";
import { isDispatchId } from "@/stream/core";

import type {
  IssueDetailResponse,
  RunResponse,
  RunsResponse,
  RunningEntryResponse,
} from "@/api/types.gen";

export type SessionRun =
  | { kind: "loading" }
  | { kind: "unknown_issue" }
  | { kind: "history_down" }
  | { kind: "no_such_session" }
  | {
      kind: "live";
      running: RunningEntryResponse;
      number: number;
      key: string;
      title: string | null;
    }
  | {
      kind: "finished";
      run: RunResponse;
      number: number;
      key: string;
      title: string | null;
    };

interface Resolution {
  result: SessionRun;
  needsPages: boolean;
}

function resolveSession(
  detail: IssueDetailResponse,
  dispatchId: string,
  pages: readonly RunsResponse[],
  isPageFailed: boolean,
): Resolution {
  const { history, running } = detail;
  const total = history?.totals.run_count ?? 0;
  const numbered = numberedRunsOf(storyRunsOf(history?.runs ?? [], pages), total);
  const last = pages.at(-1);
  const hasMore = history?.next_cursor != null && (last === undefined || last.next_cursor !== null);
  const title = detail.issue.title || null;
  const withTitle = (served: string | undefined) => title ?? (served || null);

  if (running?.dispatch_id === dispatchId) {
    return {
      result: {
        kind: "live",
        running,
        number: total + 1,
        key: running.display_identifier || detail.issue_identifier,
        title: withTitle(running.issue_title),
      },
      needsPages: false,
    };
  }

  const found = numbered.find((entry) => entry.run.dispatch_id === dispatchId);
  if (found !== undefined) {
    const { run, number } = found;
    return {
      result: {
        kind: "finished",
        run,
        number,
        key: run.display_identifier || detail.issue_identifier,
        title: withTitle(run.issue_title),
      },
      needsPages: false,
    };
  }

  if (hasMore) {
    return { result: { kind: "loading" }, needsPages: true };
  }
  return {
    result: { kind: isPageFailed ? "history_down" : "no_such_session" },
    needsPages: false,
  };
}

function isHistoryDown(error: unknown): boolean {
  return error instanceof ApiError && error.status === 503 && error.code === "history_unavailable";
}

/** Loads older runs only when the session is not among the served ones, and only for the caller that passes `drivePages`; the others read what it has cached. */
export function useSessionRun(
  identifier: string,
  dispatchId: string,
  options: { drivePages: boolean },
): SessionRun {
  const { drivePages } = options;
  const { data: detail, error } = useIssueQuery(identifier);
  const isValid = isDispatchId(dispatchId);
  const served =
    isValid && detail !== undefined ? resolveSession(detail, dispatchId, [], false) : undefined;
  const older = useHistoryRunsQuery(
    detail?.issue_id ?? "",
    {},
    drivePages && served?.needsPages === true,
  );
  const { hasNextPage, isFetching, isError, fetchNextPage } = older;
  const resolution =
    isValid && detail !== undefined
      ? resolveSession(detail, dispatchId, older.data?.pages ?? [], isError)
      : undefined;
  const needsPages = resolution?.needsPages === true;

  useEffect(() => {
    if (drivePages && needsPages && hasNextPage && !isFetching && !isError) {
      void fetchNextPage({ cancelRefetch: false });
    }
  }, [drivePages, needsPages, hasNextPage, isFetching, isError, fetchNextPage]);

  if (!isValid) {
    return { kind: "no_such_session" };
  }
  if (error instanceof ApiError && error.status === 404) {
    return { kind: "unknown_issue" };
  }
  if (isHistoryDown(error)) {
    return { kind: "history_down" };
  }
  return resolution?.result ?? { kind: "loading" };
}
