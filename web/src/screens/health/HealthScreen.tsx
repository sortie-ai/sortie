import { Link } from "react-router";

import { useStateQuery } from "@/api/queries";
import { Pair, Pairs, Panel } from "@/components/Panel";
import { ServedText } from "@/components/ServedText";
import { StatusMark } from "@/components/StatusMark";
import { scmProviderName } from "@/lib/condition";
import {
  fmtCount,
  fmtDuration,
  fmtStamp,
  fmtStampSec,
  fmtVersion,
  parseInstant,
} from "@/lib/format";

import type { HealthCheckResponse, StateHealthResponse, StateResponse } from "@/api/types.gen";
import type { Mark } from "@/lib/run-status";
import type { ReactNode } from "react";

const HEADING = "font-heading text-h5 font-semibold text-ink";
const LINK =
  "text-link underline underline-offset-2 hover:text-link-hover focus-visible:outline-2 focus-visible:outline-focus-ring active:text-link-active";

function markOf(check: HealthCheckResponse): Mark {
  if (check.checked_at === null) {
    return "waiting";
  }
  return check.status === "fail" ? "failed" : "succeeded";
}

function lastSuccessText(check: HealthCheckResponse): string {
  return check.last_success_at === null
    ? "none since Sortie started"
    : `at ${fmtStamp(check.last_success_at)}`;
}

function failureText(check: HealthCheckResponse, one: string, many: string): string {
  const since = check.failing_since === null ? "" : ` since ${fmtStamp(check.failing_since)}`;
  const streak =
    check.consecutive_failures > 1
      ? `${fmtCount(check.consecutive_failures)} ${many} in a row failed${since}`
      : `The last ${one} failed`;
  return `${streak}; last success ${lastSuccessText(check)}.`;
}

interface CheckRowProps {
  name: ReactNode;
  check: HealthCheckResponse;
  /** The line once the check has run; the check's error, when it has one, follows it. */
  line: ReactNode;
  unrunLine?: ReactNode;
}

function CheckRow({ name, check, line, unrunLine = "Not run yet." }: CheckRowProps) {
  const hasRun = check.checked_at !== null;
  const hasError = hasRun && check.error !== undefined && check.error !== "";
  return (
    <li className="grid grid-cols-[var(--size-icon)_minmax(0,1fr)] items-baseline gap-x-md gap-y-xxs border-t border-line py-sm text-body-md first:border-t-0 sm:grid-cols-[var(--size-icon)_168px_minmax(0,1fr)]">
      <StatusMark mark={markOf(check)} className="self-start pt-0.75" />
      <span className="font-medium text-ink">{name}</span>
      <div className="col-start-2 flex min-w-0 flex-col gap-xs text-ink-body sm:col-start-3">
        <span>{hasRun ? line : unrunLine}</span>
        {hasError ? (
          <pre className="rounded-lg bg-error-container p-sm font-mono text-body-sm whitespace-pre-wrap text-ink wrap-anywhere">
            {check.error}
          </pre>
        ) : undefined}
      </div>
    </li>
  );
}

interface ChecksProps {
  health: StateHealthResponse;
}

function Checks({ health }: ChecksProps) {
  const { workflow, preflight, poll, database, scm } = health;
  const sentence = <ServedText text={poll.summary_text} />;

  let workflowLine: string;
  if (workflow.status !== "fail") {
    workflowLine = `Loaded ${lastSuccessText(workflow)}.`;
  } else if (workflow.last_success_at === null) {
    workflowLine = "The file failed to load; no new issue starts until it loads.";
  } else {
    const reload =
      workflow.consecutive_failures > 1
        ? `${fmtCount(workflow.consecutive_failures)} reloads in a row failed`
        : `Reload at ${fmtStamp(workflow.checked_at ?? workflow.last_success_at)} failed`;
    workflowLine = `${reload}; running on the file loaded at ${fmtStamp(workflow.last_success_at)}. No new issue starts until it loads.`;
  }

  return (
    <Panel title="Checks">
      <ul>
        <CheckRow
          name="Database"
          check={database}
          line={
            database.status === "fail"
              ? failureText(database, "check", "checks")
              : "SQLite is reachable."
          }
        />
        <CheckRow
          name="Dispatch preflight"
          check={preflight}
          line={
            preflight.status === "fail"
              ? `${failureText(preflight, "tick", "ticks")} No new issue starts until preflight passes.`
              : "The last tick passed preflight."
          }
        />
        <CheckRow name={workflow.file ?? "Workflow file"} check={workflow} line={workflowLine} />
        <CheckRow name="Tracker poll" check={poll} line={sentence} unrunLine={sentence} />
        {scm === null ? undefined : (
          <CheckRow
            name={scmProviderName(scm.provider)}
            check={scm}
            unrunLine="No pull request checked since Sortie started."
            line={
              scm.status === "fail" ? (
                failureText(scm, "read", "reads")
              ) : (
                <>
                  Reachable, used by the reactions on{" "}
                  <Link to="/workflow" className={LINK}>
                    Workflow
                  </Link>
                  .
                </>
              )
            }
          />
        )}
      </ul>
    </Panel>
  );
}

function stampOrDash(at: string | null, format: (at: string) => string): string {
  return at === null ? "–" : format(at);
}

function nextPollText(nextPollAt: string | null, generatedAt: string): string {
  if (nextPollAt === null) {
    return "none scheduled";
  }
  const next = parseInstant(nextPollAt);
  const generated = parseInstant(generatedAt);
  return next !== null && generated !== null && next <= generated
    ? "due now"
    : fmtStampSec(nextPollAt);
}

interface AsideProps {
  state: StateResponse;
  health: StateHealthResponse;
}

function PollLoopPanel({ state, health }: AsideProps) {
  const { poll } = health;
  const interval = state.config?.polling.interval_ms;
  return (
    <Panel title="Poll loop">
      <Pairs>
        {interval === undefined ? undefined : (
          <Pair label="Interval">every {fmtDuration(interval / 1000)}</Pair>
        )}
        <Pair label="Last poll">{stampOrDash(poll.checked_at, fmtStampSec)}</Pair>
        <Pair label="Last good poll">{stampOrDash(poll.last_success_at, fmtStampSec)}</Pair>
        <Pair label="Next poll">{nextPollText(poll.next_poll_at, state.generated_at)}</Pair>
        <Pair label="Failures in a row">{fmtCount(poll.consecutive_failures)}</Pair>
      </Pairs>
    </Panel>
  );
}

function ProcessPanel({ state, health }: AsideProps) {
  return (
    <Panel title="Process">
      <Pairs>
        <Pair label="Version">{fmtVersion(state.version)}</Pair>
        <Pair label="Started">{stampOrDash(state.started_at, fmtStamp)}</Pair>
        <Pair label="Agents">
          {state.counts.running === 0 ? "None" : fmtCount(state.counts.running)} running
        </Pair>
        <Pair label="Config loaded">{stampOrDash(health.workflow.last_success_at, fmtStamp)}</Pair>
      </Pairs>
    </Panel>
  );
}

export function HealthScreen(): ReactNode {
  const { data } = useStateQuery();
  if (data === undefined) {
    return <h1 className={HEADING}>Health</h1>;
  }
  const { health } = data;
  if (health === undefined) {
    return (
      <div className="flex flex-col gap-xs">
        <h1 className={HEADING}>Health</h1>
        <p className="text-body-lg text-ink-body">Sortie serves no health checks.</p>
      </div>
    );
  }

  return (
    <div className="flex flex-col gap-lg">
      <h1 className={HEADING}>Health</h1>
      <div className="grid grid-cols-1 items-start gap-x-xxxl gap-y-xl min-[1024px]:grid-cols-[minmax(0,1fr)_320px]">
        <Checks health={health} />
        <div className="flex min-w-0 flex-col gap-xl">
          <PollLoopPanel state={data} health={health} />
          <ProcessPanel state={data} health={health} />
        </div>
      </div>
    </div>
  );
}
