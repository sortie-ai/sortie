import { plural } from "@/lib/format";

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
    <section className="flex flex-col gap-sm">
      <div className="flex flex-wrap items-baseline gap-x-sm">
        <h2 className="font-heading text-h7 font-semibold text-ink">Not dispatched</h2>
        <span className="text-body-md text-ink-muted">{plural(count, "issue", "issues")}</span>
      </div>
      <ul className="divide-y divide-line overflow-hidden rounded-lg border border-line bg-canvas">
        {rows.map((row) => (
          <HeldItem key={row.issue_id} row={row} now={now} />
        ))}
      </ul>
    </section>
  );
}
