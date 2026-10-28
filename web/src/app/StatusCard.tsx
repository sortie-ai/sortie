import { Link } from "react-router";

import { useStateQuery } from "@/api/queries";
import { cn } from "@/lib/cn";
import {
  ACKNOWLEDGED_WORD,
  acknowledgedCount,
  condition,
  conditionWord,
  failingChecks,
} from "@/lib/condition";
import { fmtCount, pluralVerb } from "@/lib/format";

import type { StateResponse } from "@/api/types.gen";
import type { Condition } from "@/lib/condition";

const CONDITION_DOT: Record<Condition, string> = {
  clear: "bg-success-fill",
  needs: "bg-warning-fill",
  held: "bg-ink-muted",
  degraded: "bg-error-fill",
};

interface AgentsLineProps {
  running: number;
  cap: number | undefined;
}

// A cap of one reads as "Agent running" or "Agent idle", never as a fraction.
function AgentsLine({ running, cap }: AgentsLineProps) {
  if (cap === 1) {
    return running > 0 ? "Agent running" : "Agent idle";
  }
  if (running === 0) {
    return "No agents running";
  }
  const count = <span className="text-ink tabular-nums">{running}</span>;
  if (cap === undefined) {
    return (
      <>
        {count} {running === 1 ? "agent" : "agents"} running
      </>
    );
  }
  return (
    <>
      {count} of {cap} {cap === 1 ? "agent" : "agents"} running
    </>
  );
}

function joinNames(names: string[]): string {
  return names.length <= 1
    ? names.join("")
    : `${names.slice(0, -1).join(", ")} and ${names[names.length - 1]}`;
}

interface HeadlineProps {
  state: StateResponse;
  current: Condition;
}

function Headline({ state, current }: HeadlineProps) {
  if (current === "held") {
    return (
      <>
        <span className="text-ink tabular-nums">{fmtCount(acknowledgedCount(state))}</span>{" "}
        {ACKNOWLEDGED_WORD}
      </>
    );
  }
  if (current !== "needs") {
    return conditionWord(current);
  }
  const { noun, verb } = pluralVerb(state.counts.attention, ["item", "items"], ["needs", "need"]);
  return (
    <>
      <span className="text-ink tabular-nums">{fmtCount(state.counts.attention)}</span> {noun}{" "}
      {verb} attention
    </>
  );
}

interface StatusSummaryProps {
  state: StateResponse | undefined;
}

function StatusSummary({ state }: StatusSummaryProps) {
  const current = state === undefined ? undefined : condition(state);

  return (
    <>
      <span className="flex items-center gap-xs text-body-md font-medium text-ink-body">
        <span
          aria-hidden="true"
          className={cn(
            "size-xs shrink-0 rounded-full",
            current === undefined ? "bg-ink-muted" : CONDITION_DOT[current],
          )}
        />
        <span>
          {state === undefined || current === undefined ? (
            "Loading"
          ) : (
            <Headline state={state} current={current} />
          )}
        </span>
      </span>
      {state !== undefined && current === "degraded" ? (
        <span className="mt-xxs block text-body-sm text-error wrap-anywhere">
          {joinNames(failingChecks(state))} failing
        </span>
      ) : undefined}
      {state === undefined ? undefined : (
        <span className="mt-xxs block text-body-sm text-ink-muted wrap-anywhere">
          <AgentsLine
            running={state.counts.running}
            cap={state.config?.agent.max_concurrent_agents}
          />
        </span>
      )}
    </>
  );
}

export function StatusCard() {
  const { data } = useStateQuery();

  return (
    <Link
      to="/"
      className="block rounded-xl bg-canvas p-sm transition-colors duration-(--duration-feedback) hover:bg-item-hover-panel focus-visible:-outline-offset-1 focus-visible:outline-2 focus-visible:outline-focus-ring"
    >
      <StatusSummary state={data} />
    </Link>
  );
}
