import { useQueryClient } from "@tanstack/react-query";
import { useLocation, useNavigate } from "react-router";

import { getIssue } from "@/api/client";
import { queryKeys, useStateQuery } from "@/api/queries";
import { latestSessionOf, sessionLinkOf } from "@/lib/issue-link";

/** Does nothing when the issue has none or its read fails; an issue not yet cached is fetched once. */
export function useOpenLatestSession(): (identifier: string) => void {
  const { data: state } = useStateQuery();
  const queryClient = useQueryClient();
  const navigate = useNavigate();
  const location = useLocation();

  async function sessionOf(identifier: string): Promise<{ dispatchId: string } | null> {
    const running = (state?.running ?? []).find((row) => row.issue_identifier === identifier);
    if (running !== undefined) {
      return { dispatchId: running.dispatch_id };
    }
    try {
      const detail = await queryClient.fetchQuery({
        queryKey: queryKeys.issue(identifier),
        queryFn: ({ signal }) => getIssue(identifier, signal),
      });
      return latestSessionOf(detail);
    } catch {
      return null;
    }
  }

  return function openLatestSession(identifier: string) {
    void sessionOf(identifier).then((session) => {
      if (session !== null) {
        const { to, state: routeState } = sessionLinkOf(identifier, session.dispatchId, location);
        void navigate(to, { state: routeState });
      }
    });
  };
}
