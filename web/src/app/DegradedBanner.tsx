import { Link, useMatch } from "react-router";

import { useStateQuery } from "@/api/queries";
import { TriangleAlertIcon } from "@/components/icons";
import { fmtClock } from "@/lib/format";

import type { StateResponse } from "@/api/types.gen";

interface CheckLine {
  label: string;
  failingSince: string | null;
  error: string | undefined;
}

function failingChecks(state: StateResponse): CheckLine[] {
  const { health } = state;
  if (health === undefined) {
    return [];
  }
  const checks: (CheckLine & { status: string })[] = [
    {
      label: `${health.workflow.file ?? "WORKFLOW.md"} reload`,
      status: health.workflow.status,
      failingSince: health.workflow.failing_since,
      error: health.workflow.error,
    },
    {
      label: "Dispatch preflight",
      status: health.preflight.status,
      failingSince: health.preflight.failing_since,
      error: health.preflight.error,
    },
    {
      label: "Tracker poll",
      status: health.poll.status,
      failingSince: health.poll.failing_since,
      error: health.poll.error,
    },
    {
      label: "Database check",
      status: health.database.status,
      failingSince: health.database.failing_since,
      error: health.database.error,
    },
  ];
  if (health.scm !== null) {
    checks.push({
      label: "Source control reads",
      status: health.scm.status,
      failingSince: health.scm.failing_since,
      error: health.scm.error,
    });
  }
  return checks.filter((check) => check.status === "fail");
}

export function DegradedBanner() {
  const { data } = useStateQuery();
  const isOnHealth = useMatch("/health") !== null;
  const lines = data === undefined ? [] : failingChecks(data);
  if (lines.length === 0) {
    return undefined;
  }

  return (
    <div
      role="status"
      className="mb-lg flex items-start gap-sm rounded-xl bg-error-container p-md text-body-md text-ink"
    >
      <TriangleAlertIcon className="mt-xxs text-error" />
      <div className="min-w-0 wrap-anywhere">
        {lines.map((line) => (
          <div key={line.label}>
            <strong className="font-bold text-error">{line.label}</strong>
            {line.failingSince === null ? "" : ` failing since ${fmtClock(line.failingSince)}`}
            {line.error === undefined || line.error === "" ? "" : `: ${line.error}`}
          </div>
        ))}
      </div>
      {isOnHealth ? undefined : (
        <Link
          to="/health"
          className="ml-auto whitespace-nowrap text-ink underline underline-offset-2 focus-visible:outline-2 focus-visible:outline-focus-ring"
        >
          Open Health
        </Link>
      )}
    </div>
  );
}
