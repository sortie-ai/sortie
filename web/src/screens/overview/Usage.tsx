import { BudgetMeter } from "@/components/BudgetMeter";
import { Parts } from "@/components/Parts";
import { fmtAmount, fmtPercent, fmtUtcStamp, parseInstant } from "@/lib/format";

import { SpendBudget, spendWindows } from "./SpendBudget";

import type { StateResponse } from "@/api/types.gen";

type KindUsage = NonNullable<StateResponse["rate_limits"]>[string];
type UsageWindow = NonNullable<KindUsage["windows"]>[number];

const HOUR_MS = 3_600_000;

const STATUS_WORDS: Readonly<Record<string, string>> = {
  allowed: "Under the limit",
  warning: "Near the limit",
  exhausted: "Usage limit reached",
};

// A report under an hour old is current enough to need no time beside it.
function isStale(at: string, now: number): boolean {
  const instant = parseInstant(at);
  return instant === null || now - instant >= HOUR_MS;
}

function isDrawn(window: UsageWindow, now: number): boolean {
  const resetsAt = window.resets_at === null ? null : parseInstant(window.resets_at);
  return resetsAt === null || resetsAt > now;
}

interface UsageWindowBlockProps {
  window: UsageWindow;
  memberReportedAt: string;
  now: number;
}

function UsageWindowBlock({ window, memberReportedAt, now }: UsageWindowBlockProps) {
  const {
    label,
    used_percent: used,
    amount,
    status,
    resets_at: resetsAt,
    reported_at: reportedAt,
  } = window;
  const statusWords = status === null ? undefined : STATUS_WORDS[status];
  const hasBar = amount !== null || used !== null;
  const isBarAlert = hasBar && (status === "warning" || status === "exhausted");
  const parts: string[] = [];
  if (isBarAlert && statusWords !== undefined) {
    parts.push(statusWords);
  }
  parts.push(resetsAt === null ? "Reset time not reported" : `Resets ${fmtUtcStamp(resetsAt)}`);
  if (reportedAt !== memberReportedAt && isStale(reportedAt, now)) {
    parts.push(`Reported ${fmtUtcStamp(reportedAt)}`);
  }
  return (
    <div className="flex flex-col gap-xxs">
      {amount !== null ? (
        <BudgetMeter
          name={label}
          line={{ used: amount.used, limit: amount.limit, limitKnown: true }}
          format={fmtAmount}
          text={`${fmtAmount(amount.used)} of ${fmtAmount(amount.limit)} ${amount.unit}`}
        />
      ) : used === null ? (
        <div className="grid grid-cols-[minmax(0,1fr)_auto] gap-x-sm text-body-md">
          <span className="text-ink">{label}</span>
          <span className="text-ink-body">{statusWords ?? "Status not reported"}</span>
        </div>
      ) : (
        <BudgetMeter
          name={label}
          line={{ used, limit: 100, limitKnown: true }}
          format={(n) => fmtPercent(n / 100)}
          text={`${fmtPercent(used / 100)} used`}
        />
      )}
      <p className="text-body-sm text-ink-muted">
        <Parts parts={parts} />
      </p>
    </div>
  );
}

interface UsageKindBlockProps {
  kind: string;
  usage: KindUsage;
  windows: UsageWindow[];
  now: number;
}

function UsageKindBlock({ kind, usage, windows, now }: UsageKindBlockProps) {
  return (
    <div className="flex flex-col gap-sm rounded-lg border border-line bg-canvas p-md">
      <div className="grid grid-cols-[minmax(0,1fr)_auto] gap-x-sm text-body-md">
        <h3 className="font-medium text-ink">{kind}</h3>
        {isStale(usage.reported_at, now) ? (
          <span className="text-body-sm text-ink-muted">
            Reported {fmtUtcStamp(usage.reported_at)}
          </span>
        ) : undefined}
      </div>
      {windows.map((window, index) => (
        <UsageWindowBlock
          key={index}
          window={window}
          memberReportedAt={usage.reported_at}
          now={now}
        />
      ))}
    </div>
  );
}

export interface UsageProps {
  ceiling: StateResponse["spend_ceiling"];
  rateLimits: StateResponse["rate_limits"];
  /** The listed agent kinds in display order; empty while `config` is not served. */
  kinds: readonly string[];
  /** Overview's server-corrected clock in epoch milliseconds. */
  now: number;
}

/** A kind with no current window, and the section with nothing to show, render nothing. */
export function Usage({ ceiling, rateLimits, kinds, now }: UsageProps) {
  const reported = kinds.flatMap((kind) => {
    const usage = rateLimits[kind];
    const windows = (usage?.windows ?? []).filter((window) => isDrawn(window, now));
    return usage === undefined || windows.length === 0 ? [] : [{ kind, usage, windows }];
  });
  const hasBudget = ceiling !== undefined && spendWindows(ceiling).length > 0;
  if (!hasBudget && reported.length === 0) {
    return undefined;
  }
  return (
    <section className="flex flex-col gap-sm">
      <h2 className="font-heading text-h7 font-semibold text-ink">Usage</h2>
      <div className="grid gap-md md:grid-cols-2">
        {hasBudget ? (
          <SpendBudget ceiling={ceiling} isEstimateNoted={reported.length > 0} />
        ) : undefined}
        {reported.map(({ kind, usage, windows }) => (
          <UsageKindBlock key={kind} kind={kind} usage={usage} windows={windows} now={now} />
        ))}
      </div>
    </section>
  );
}
