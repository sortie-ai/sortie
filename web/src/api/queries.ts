import { QueryClient, useInfiniteQuery, useQuery, useQueryClient } from "@tanstack/react-query";

import {
  ApiError,
  getIssue,
  getIssueGroups,
  getPrices,
  getRuns,
  getState,
  getStats,
  getStatsSpan,
} from "@/api/client";
import { setServerOffset } from "@/lib/server-clock";

import type { HistoryFilter } from "@/api/client";
import type {
  IssueDetailResponse,
  IssueGroupResponse,
  IssueGroupsResponse,
  PricesResponse,
  Report,
  RunsResponse,
  SpanReport,
  StateResponse,
} from "@/api/types.gen";
import type { InfiniteData, UseInfiniteQueryResult, UseQueryResult } from "@tanstack/react-query";

export type AnalyticsRange = "24h" | "7d" | "30d" | "90d" | "all";

/** A half-open UTC window [since, until), epoch milliseconds. */
export interface StatsWindow {
  since: number;
  until: number;
}

/** The one cache of server data in a tab; a change signal, not a timer, decides when data is stale. */
export const queryClient = new QueryClient({
  defaultOptions: {
    queries: { staleTime: Infinity, refetchOnWindowFocus: true },
    mutations: { retry: false },
  },
});

/** Builds the cache keys; `midnight` is the start of the reader's day in RFC 3339 UTC. */
export const queryKeys = {
  state: () => ["state"] as const,
  issue: (identifier: string) => ["issue", identifier] as const,
  ended: (midnight: string) => ["ended", midnight] as const,
  tally: (midnight: string) => ["tally", midnight] as const,
  firstRun: () => ["first-run"] as const,
  history: (filter: HistoryFilter) => ["history", filter] as const,
  historyRuns: (issueId: string, filter: HistoryFilter) =>
    ["history-runs", issueId, filter] as const,
  issueLookup: (query: string) => ["issue-lookup", query] as const,
  stats: (range: AnalyticsRange, zoom?: StatsWindow | null) =>
    zoom == null
      ? (["stats", range] as const)
      : (["stats", "zoom", zoom.since, zoom.until] as const),
  statsPrevious: (range: AnalyticsRange, until: string, zoom?: StatsWindow | null) =>
    zoom == null
      ? (["stats-previous", range, until] as const)
      : (["stats-previous", "zoom", zoom.since, zoom.until] as const),
  statsSpan: (window: StatsWindow) => ["stats-span", window.since, window.until] as const,
  statsSpanBefore: (window: StatsWindow) =>
    ["stats-span-before", window.since, window.until] as const,
  prices: () => ["prices"] as const,
};

export const HISTORY_PAGE_SIZE = 20;
export const HISTORY_RUNS_PAGE_SIZE = 50;

/** The least time between two change-signal refetches: a History refetch rereads every loaded page and an Analytics refetch rescans every run in its range. */
export const HISTORY_REFETCH_FLOOR_MS = 5_000;
export const STATS_REFETCH_FLOOR_MS = 30_000;

const HOURS_BY_RANGE: Record<AnalyticsRange, number | undefined> = {
  "24h": 24,
  "7d": 168,
  "30d": 720,
  "90d": 2160,
  all: undefined,
};

const MS_PER_HOUR = 3_600_000;

const ENDED_PAGE_SIZE = 200;

async function fetchEnded(midnight: string, signal: AbortSignal): Promise<RunsResponse[]> {
  const pages: RunsResponse[] = [];
  let cursor: string | undefined;
  do {
    const page = await getRuns({ ended_since: midnight, limit: ENDED_PAGE_SIZE, cursor }, signal);
    pages.push(page);
    cursor = page.next_cursor ?? undefined;
  } while (cursor !== undefined);
  return pages;
}

async function fetchHasRun(signal: AbortSignal): Promise<boolean> {
  try {
    const page = await getRuns({ limit: 1 }, signal);
    return (page.runs ?? []).length > 0;
  } catch (error) {
    if (error instanceof ApiError && error.status === 503 && error.code === "history_unavailable") {
      return false;
    }
    throw error;
  }
}

const ISSUE_LOOKUP_LIMIT = 5;

// The server matches an issue key exactly, case included, so a key typed in
// lowercase is also asked for in uppercase.
async function fetchIssueLookup(query: string, signal: AbortSignal): Promise<IssueGroupResponse[]> {
  const answer = await getIssueGroups({ q: query, limit: ISSUE_LOOKUP_LIMIT }, signal);
  return answer.groups ?? [];
}

async function fetchState(signal: AbortSignal): Promise<StateResponse> {
  const state = await getState(signal);
  setServerOffset(state.generated_at);
  return state;
}

/** Reads the runtime state and fixes the server clock offset from every response. */
export function useStateQuery(): UseQueryResult<StateResponse> {
  return useQuery({ queryKey: queryKeys.state(), queryFn: ({ signal }) => fetchState(signal) });
}

export function useIssueQuery(identifier: string | undefined): UseQueryResult<IssueDetailResponse> {
  return useQuery({
    queryKey: queryKeys.issue(identifier ?? ""),
    queryFn: ({ signal }) => getIssue(identifier ?? "", signal),
    enabled: identifier !== undefined,
  });
}

export function useEndedQuery(midnight: string): UseQueryResult<RunsResponse[]> {
  return useQuery({
    queryKey: queryKeys.ended(midnight),
    queryFn: ({ signal }) => fetchEnded(midnight, signal),
  });
}

export function useTallyQuery(midnight: string): UseQueryResult<Report> {
  return useQuery({
    queryKey: queryKeys.tally(midnight),
    queryFn: ({ signal }) => getStats({ since: midnight }, signal),
  });
}

/** `true` while no run is on record, `undefined` until the first answer; once a run exists the query is disabled for the rest of the page's life. */
export function useFirstRunQuery(): boolean | undefined {
  const client = useQueryClient();
  const hasRun = client.getQueryData<boolean>(queryKeys.firstRun()) === true;
  const { data } = useQuery({
    queryKey: queryKeys.firstRun(),
    queryFn: ({ signal }) => fetchHasRun(signal),
    enabled: !hasRun,
  });
  return data === undefined ? undefined : !data;
}

export function useHistoryQuery(
  filter: HistoryFilter,
): UseInfiniteQueryResult<InfiniteData<IssueGroupsResponse, string | undefined>> {
  return useInfiniteQuery({
    queryKey: queryKeys.history(filter),
    queryFn: ({ signal, pageParam }) =>
      getIssueGroups({ ...filter, cursor: pageParam, limit: HISTORY_PAGE_SIZE }, signal),
    initialPageParam: undefined as string | undefined,
    getNextPageParam: (last) => last.next_cursor ?? undefined,
    meta: { refetchFloorMs: HISTORY_REFETCH_FLOOR_MS },
  });
}

export function useHistoryRunsQuery(
  issueId: string,
  filter: HistoryFilter,
  enabled: boolean,
): UseInfiniteQueryResult<InfiniteData<RunsResponse, string | undefined>> {
  return useInfiniteQuery({
    queryKey: queryKeys.historyRuns(issueId, filter),
    queryFn: ({ signal, pageParam }) =>
      getRuns(
        {
          issue_id: issueId,
          rule: filter.rule,
          status: filter.status,
          stage: filter.stage,
          limit: HISTORY_RUNS_PAGE_SIZE,
          cursor: pageParam,
        },
        signal,
      ),
    initialPageParam: undefined as string | undefined,
    getNextPageParam: (last) => last.next_cursor ?? undefined,
    enabled,
    meta: { refetchFloorMs: HISTORY_REFETCH_FLOOR_MS },
  });
}

export function useIssueLookupQuery(query: string): UseQueryResult<IssueGroupResponse[]> {
  return useQuery({
    queryKey: queryKeys.issueLookup(query),
    queryFn: ({ signal }) => fetchIssueLookup(query, signal),
    enabled: query !== "",
    meta: { refetchFloorMs: HISTORY_REFETCH_FLOOR_MS },
  });
}

/** No previous data stands in while another range or zoom loads, so a view never shows the figures of the one before it. */
export function useStatsQuery(
  range: AnalyticsRange,
  zoom: StatsWindow | null = null,
): UseQueryResult<Report> {
  const hours = HOURS_BY_RANGE[range];
  return useQuery({
    queryKey: queryKeys.stats(range, zoom),
    queryFn: ({ signal }) =>
      zoom === null
        ? getStats({ since: hours === undefined ? undefined : `${hours}h` }, signal)
        : getStats(
            {
              since: new Date(zoom.since).toISOString(),
              until: new Date(zoom.until).toISOString(),
            },
            signal,
          ),
    meta: { refetchFloorMs: STATS_REFETCH_FLOOR_MS },
  });
}

function previousBounds(until: string, hours: number): { since: string; until: string } {
  return { since: new Date(Date.parse(until) - hours * MS_PER_HOUR).toISOString(), until };
}

function previousZoomBounds(until: string, zoom: StatsWindow): { since: string; until: string } {
  return { since: new Date(Date.parse(until) - (zoom.until - zoom.since)).toISOString(), until };
}

/**
 * Reads the report of the window of equal length that ends where the current report's window starts, so `until` is that report's served `since`.
 * The query is static, so a change signal or a window focus never rescans an old previous window; a refetch of the current report moves `until` to a new key instead.
 * The last report of the same range stands in while the new key loads, and none stands in across ranges or into or out of a zoom.
 */
export function useStatsPreviousQuery(
  range: AnalyticsRange,
  zoom: StatsWindow | null,
  until: string | null,
): UseQueryResult<Report> {
  const hours = HOURS_BY_RANGE[range];
  return useQuery({
    queryKey: queryKeys.statsPrevious(range, until ?? "", zoom),
    queryFn: ({ signal }) =>
      getStats(
        zoom === null
          ? previousBounds(until ?? "", hours ?? 0)
          : previousZoomBounds(until ?? "", zoom),
        signal,
      ),
    enabled: until !== null && (zoom !== null || hours !== undefined),
    staleTime: "static",
    placeholderData: (previousData, previousQuery) =>
      zoom === null && previousQuery?.queryKey[1] === range ? previousData : undefined,
  });
}

const NO_WINDOW: StatsWindow = { since: 0, until: 0 };

function fetchSpan(window: StatsWindow, signal: AbortSignal): Promise<SpanReport> {
  return getStatsSpan(
    { since: new Date(window.since).toISOString(), until: new Date(window.until).toISOString() },
    signal,
  );
}

/** Reads the report of a window that can hold the bucket in progress, so it refetches on the change signal; no other window's report stands in. */
export function useStatsSpanQuery(window: StatsWindow | null): UseQueryResult<SpanReport> {
  return useQuery({
    queryKey: queryKeys.statsSpan(window ?? NO_WINDOW),
    queryFn: ({ signal }) => fetchSpan(window ?? NO_WINDOW, signal),
    enabled: window !== null,
    meta: { refetchFloorMs: STATS_REFETCH_FLOOR_MS },
  });
}

/** Reads the report of a window that ends before the selection starts, so no turn ends inside it later and it is read once. */
export function useStatsSpanBeforeQuery(window: StatsWindow | null): UseQueryResult<SpanReport> {
  return useQuery({
    queryKey: queryKeys.statsSpanBefore(window ?? NO_WINDOW),
    queryFn: ({ signal }) => fetchSpan(window ?? NO_WINDOW, signal),
    enabled: window !== null,
    staleTime: "static",
  });
}

/** Stale at once so every opening of the screen asks again; the marker keeps a change signal from doing so, which would rerun its history read on every poll tick. */
export function usePricesQuery(): UseQueryResult<PricesResponse> {
  return useQuery({
    queryKey: queryKeys.prices(),
    queryFn: ({ signal }) => getPrices(signal),
    staleTime: 0,
    meta: { skipsChangeSignal: true },
  });
}
