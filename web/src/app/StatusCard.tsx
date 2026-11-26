import { useStateQuery } from "@/api/queries";
import { Link } from "@/components/carried-links";
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

// A line ends in an ellipsis; the padding keeps its ascenders and descenders inside the clip.
const ONE_LINE = "trim-caps -my-xxs block truncate py-xxs";

const CONDITION_DOT: Record<Condition, string> = {
  clear: "bg-success-fill",
  needs: "bg-warning-fill",
  held: "bg-ink-muted",
  degraded: "bg-error-fill",
};

interface AgentsProps {
  running: number;
  cap: number | undefined;
}

// A cap of one reads as "Agent running" or "Agent idle", never as a fraction.
function agentsText({ running, cap }: AgentsProps): string {
  if (cap === 1) {
    return running > 0 ? "Agent running" : "Agent idle";
  }
  if (running === 0) {
    return "No agents running";
  }
  return cap === undefined
    ? `${running} ${running === 1 ? "agent" : "agents"} running`
    : `${running} of ${cap} agents running`;
}

function AgentsLine(props: AgentsProps) {
  return <span className={cn("text-body-sm text-ink-muted", ONE_LINE)}>{agentsText(props)}</span>;
}

// One name reads in full; several would not fit the card, so they are counted and named in the title.
function failingText(names: string[]): string {
  return names.length === 1 ? `${names[0]} failing` : `${names.length} checks failing`;
}

// One thin segment per agent the cap allows, filled while it runs.
function AgentsBar({ running, cap }: AgentsProps & { cap: number }) {
  return (
    <span aria-hidden="true" className="flex h-0.5 grow gap-1">
      {Array.from({ length: cap }, (_, index) => (
        <span
          key={index}
          className={cn("grow rounded-full", index < running ? "bg-primary" : "bg-surface-raised")}
        />
      ))}
    </span>
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
        {fmtCount(acknowledgedCount(state))} {ACKNOWLEDGED_WORD}
      </>
    );
  }
  if (current !== "needs") {
    return conditionWord(current);
  }
  const { noun, verb } = pluralVerb(state.counts.attention, ["item", "items"], ["needs", "need"]);
  return (
    <>
      {fmtCount(state.counts.attention)} {noun} {verb} attention
    </>
  );
}

interface StatusSummaryProps {
  state: StateResponse | undefined;
  failing: string[];
}

function StatusSummary({ state, failing }: StatusSummaryProps) {
  const current = state === undefined ? undefined : condition(state);
  const running = state?.counts.running ?? 0;
  const cap = state?.config?.agent.max_concurrent_agents;

  return (
    <>
      {/* A box as tall as a screen title's capitals puts the line on the title's baseline. */}
      <span className="flex min-h-[round(up,1cap,1px)] items-end font-heading text-h4">
        <span className="flex min-w-0 items-center gap-xs font-sans text-body-md font-semibold tracking-normal text-ink">
          <span
            aria-hidden="true"
            className={cn(
              "size-xs shrink-0 rounded-full",
              current === undefined ? "bg-ink-muted" : CONDITION_DOT[current],
            )}
          />
          <span className={ONE_LINE}>
            {state === undefined || current === undefined ? (
              "Loading"
            ) : (
              <Headline state={state} current={current} />
            )}
          </span>
        </span>
      </span>
      {/* A box as tall as the header's gap and the standfirst's capitals puts the agents on the standfirst's baseline. */}
      <span className="flex min-h-[calc(var(--spacing-md)+round(down,1cap,1px))] flex-col justify-end text-body-lg">
        {state === undefined || cap === undefined ? undefined : (
          // The agents line is mostly lowercase and stands below its capitals, so the bar sits a pixel lower to look centered.
          <span className="flex grow items-center pt-0.5">
            <AgentsBar running={running} cap={cap} />
          </span>
        )}
        {state === undefined ? undefined : current === "degraded" ? (
          <span
            title={`${joinNames(failing)} failing`}
            className={cn("text-body-sm text-error", ONE_LINE)}
          >
            {failingText(failing)}
          </span>
        ) : (
          <AgentsLine running={running} cap={cap} />
        )}
      </span>
    </>
  );
}

export function StatusCard() {
  const { data } = useStateQuery();
  const failing = data === undefined ? [] : failingChecks(data);

  return (
    <Link
      to="/"
      className="flex flex-col rounded-lg bg-canvas px-sm pb-md transition-colors duration-(--duration-feedback) hover:bg-item-hover-panel focus-visible:-outline-offset-1 focus-visible:outline-2 focus-visible:outline-focus-ring"
    >
      <StatusSummary state={data} failing={failing} />
    </Link>
  );
}
