import { useEffect, useMemo } from "react";

import { useEndedQuery, useStateQuery } from "@/api/queries";
import { activeRows } from "@/lib/active-rows";
import { buildEnded } from "@/lib/ended";
import { now, localMidnight } from "@/lib/server-clock";
import { attentionKey, endedKey, markRead } from "@/lib/unread";

import type { IssueDetailRow } from "@/components/IssueDetail";

/** Its attention items and its ended entry count as read while it is shown. */
export function useIssueSummary(identifier: string): IssueDetailRow | undefined {
  const { data: state } = useStateQuery();
  const { data: pages } = useEndedQuery(localMidnight(now()).toISOString());

  const summary = useMemo<IssueDetailRow | undefined>(() => {
    if (state === undefined) {
      return undefined;
    }
    const row = activeRows(state).find((candidate) => candidate.identifier === identifier);
    if (row !== undefined) {
      return row;
    }
    const ended = pages === undefined ? undefined : buildEnded(pages, state);
    const issue = ended?.find((candidate) => candidate.identifier === identifier);
    return issue === undefined
      ? undefined
      : {
          displayKey: issue.displayKey,
          title: issue.title,
          statusTextFull: issue.outcome.status_text_full,
        };
  }, [state, pages, identifier]);

  const unreadKeys = useMemo(() => {
    const keys = (state?.attention ?? [])
      .filter((item) => item.issue_identifier === identifier)
      .map((item) => attentionKey(item.id));
    const ended =
      state === undefined || pages === undefined
        ? undefined
        : buildEnded(pages, state).find((candidate) => candidate.identifier === identifier);
    if (ended !== undefined) {
      keys.push(endedKey(ended.issueId, ended.outcome.observed_at));
    }
    return keys;
  }, [state, pages, identifier]);

  useEffect(() => {
    markRead(unreadKeys);
  }, [unreadKeys]);

  return summary;
}
