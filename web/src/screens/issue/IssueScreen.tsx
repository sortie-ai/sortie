import { useParams } from "react-router";

import { useShortcuts } from "@/app/keys";
import { IssueDetail } from "@/components/IssueDetail";
import { useOpenLatestSession } from "@/components/use-open-session";
import { useIssueSummary } from "@/components/use-issue-summary";

interface IssuePageProps {
  identifier: string;
}

function IssuePage({ identifier }: IssuePageProps) {
  const row = useIssueSummary(identifier);
  const openLatestSession = useOpenLatestSession();
  useShortcuts([{ keys: ["o"], run: () => openLatestSession(identifier) }]);
  return <IssueDetail identifier={identifier} row={row} />;
}

/** Showing the issue reads its attention items and its ended entry. */
export function IssueScreen() {
  const { identifier } = useParams();
  return identifier === undefined ? undefined : <IssuePage identifier={identifier} />;
}
