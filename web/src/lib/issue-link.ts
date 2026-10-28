import type { IssueDetailResponse } from "@/api/types.gen";

const ISSUE_ROUTE_PREFIX = "/issues/";
const FALLBACK_BACK = "/";

export interface IssueRouteState {
  back: string;
}

function isIssueRoute(pathname: string): boolean {
  return pathname.startsWith(ISSUE_ROUTE_PREFIX);
}

function withoutPeek(pathname: string, search: string): string {
  const params = new URLSearchParams(search);
  params.delete("peek");
  const text = params.toString();
  return text === "" ? pathname : `${pathname}?${text}`;
}

/** History state is writable by any script of the origin and by the address bar of a restored session, so a target that leaves the dashboard falls back to `/`. */
export function backOf(state: unknown): string {
  const back = (state as { back?: unknown } | null | undefined)?.back;
  if (
    typeof back === "string" &&
    back.startsWith("/") &&
    !back.startsWith("//") &&
    !isIssueRoute(back)
  ) {
    return back;
  }
  return FALLBACK_BACK;
}

/** An issue page opened from another issue page keeps that page's back target, so `Esc` never walks a chain of issues. */
export function issueLinkOf(
  identifier: string,
  from: { pathname: string; search: string; state: unknown },
): { to: string; state: IssueRouteState } {
  const back = isIssueRoute(from.pathname)
    ? backOf(from.state)
    : withoutPeek(from.pathname, from.search);
  return { to: `${ISSUE_ROUTE_PREFIX}${encodeURIComponent(identifier)}`, state: { back } };
}

export function sessionLinkOf(
  identifier: string,
  dispatchId: string,
  from: { pathname: string; search: string; state: unknown },
): { to: string; state: IssueRouteState } {
  const { to, state } = issueLinkOf(identifier, from);
  return { to: `${to}/sessions/${dispatchId}`, state };
}

/** The running session, else the newest run that recorded a dispatch ID; `null` for an issue with neither. */
export function latestSessionOf(
  detail: IssueDetailResponse,
): { dispatchId: string; isLive: boolean } | null {
  if (detail.running !== null) {
    return { dispatchId: detail.running.dispatch_id, isLive: true };
  }
  const run = (detail.history?.runs ?? []).find(
    (candidate) => candidate.dispatch_id !== null && candidate.dispatch_id !== "",
  );
  return run?.dispatch_id ? { dispatchId: run.dispatch_id, isLive: false } : null;
}
