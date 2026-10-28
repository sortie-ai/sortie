import { ExternalLinkIcon } from "@/components/icons";
import { ServedText } from "@/components/ServedText";
import { buttonStyles } from "@/components/ui/button";
import { fmtAgo } from "@/lib/format";
import { safeHttpUrl } from "@/lib/links";

import type { HeldIssueResponse } from "@/api/types.gen";

export interface HeldItemProps {
  row: HeldIssueResponse;
  /** Server-clock time in epoch milliseconds that the hold's age measures against. */
  now: number;
}

/** It opens nothing inside the dashboard. */
export function HeldItem({ row, now }: HeldItemProps) {
  const key = row.display_identifier || row.issue_identifier;
  const url = safeHttpUrl(row.issue_url);

  return (
    <li className="grid grid-cols-[minmax(0,1fr)_auto] gap-x-md gap-y-xxs p-md max-md:grid-cols-[minmax(0,1fr)]">
      <p className="flex min-w-0 flex-wrap items-baseline gap-x-sm gap-y-xxs">
        <span className="text-body-md font-medium text-ink">{row.issue_title || key}</span>
        {row.issue_title ? (
          <span className="text-body-sm whitespace-nowrap text-ink-muted tabular-nums">{key}</span>
        ) : undefined}
      </p>
      <p className="text-body-md text-ink-body">
        <ServedText text={row.hold.reason_text} />
      </p>
      <p className="text-body-sm text-ink-muted">
        <ServedText text={row.hold.release_text} />
      </p>
      <div className="col-start-2 row-span-3 row-start-1 flex flex-col items-end justify-between gap-xs max-md:col-start-1 max-md:row-span-1 max-md:row-start-auto max-md:flex-row-reverse max-md:items-center max-md:justify-end">
        <span className="text-body-sm whitespace-nowrap text-ink-muted tabular-nums">
          {fmtAgo(row.hold.held_since, now)}
        </span>
        {url === null ? undefined : (
          <a
            href={url}
            target="_blank"
            rel="noopener noreferrer"
            title={`Open ${key} in the tracker`}
            className={buttonStyles("secondary")}
          >
            <ExternalLinkIcon />
            Open issue
          </a>
        )}
      </div>
    </li>
  );
}
