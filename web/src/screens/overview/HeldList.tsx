import { plural } from "@/lib/format";
import { Panel } from "@/components/Panel";

import { HeldItem } from "./HeldItem";

import type { HeldIssueResponse } from "@/api/types.gen";

export interface HeldListProps {
  rows: readonly HeldIssueResponse[];
  /** `counts.held` as served. */
  count: number;
  now: number;
}

/** The held issues in served order, never sorted or filtered by the client. */
export function HeldList({ rows, count, now }: HeldListProps) {
  if (rows.length === 0) {
    return undefined;
  }
  return (
    <Panel title="Not dispatched" meta={plural(count, "issue", "issues")}>
      <ul className="divide-y divide-line overflow-hidden rounded-xl border border-line bg-canvas">
        {rows.map((row) => (
          <HeldItem key={row.issue_id} row={row} now={now} />
        ))}
      </ul>
    </Panel>
  );
}
