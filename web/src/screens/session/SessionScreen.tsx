import { useMemo } from "react";
import { useParams } from "react-router";

import { useIssueQuery, useStateQuery } from "@/api/queries";
import { useHold, useLive, useNow } from "@/app/live";
import { useRunEvents } from "@/app/run-events";
import { AgentStatement } from "@/components/AgentStatement";
import { TriangleAlertIcon } from "@/components/icons";
import { Panel } from "@/components/Panel";
import { ServedText } from "@/components/ServedText";
import { StallNotice } from "@/components/StallNotice";
import { useSessionRun } from "@/components/use-session-run";
import { routeText } from "@/lib/active-rows";
import { fmtClock, fmtDuration, parseInstant, plural } from "@/lib/format";
import { latestSessionOf } from "@/lib/issue-link";
import { agentLabel, modelLabel, startLabel } from "@/lib/labels";
import { runRouteText } from "@/lib/lane";
import { agentStatement, runOutcome } from "@/lib/run-status";

import { EventPane } from "./EventPane";
import { ModelCostPanel, SessionPanel, SkillsPanel, UsagePanel } from "./SessionPanels";
import { SessionsPanel } from "./SessionsPanel";
import { timelineOf, turnBarsOf } from "./timeline";
import { TokensChart, TokensLegend } from "./TokensChart";

import type { ConfigResponse, IssueDetailResponse, RunResponse } from "@/api/types.gen";
import type { RunLog } from "@/stream/core";
import type { ResolvedSession } from "./SessionPanels";

interface EndNoticeProps {
  run: RunResponse;
  /** The issue's served sentence on what happens next, given only for the issue's newest session. */
  nextStep: string | null;
}

function EndNotice({ run, nextStep }: EndNoticeProps) {
  const statement = agentStatement(run);
  const hasError = !!run.error;
  if (!hasError && statement === null && nextStep === null) {
    return undefined;
  }
  const isAlarmed = runOutcome(run).mark === "failed";
  const content = (
    <div className="flex min-w-0 flex-col gap-xs">
      {hasError ? <span>{run.error}</span> : undefined}
      {statement === null ? undefined : <AgentStatement text={statement} />}
      {nextStep === null ? undefined : (
        <span>
          <ServedText text={nextStep} />
        </span>
      )}
    </div>
  );
  if (isAlarmed) {
    return (
      <div
        role="status"
        className="mb-lg flex items-start gap-sm rounded-xl bg-error-container p-md text-body-md text-ink wrap-anywhere"
      >
        <TriangleAlertIcon className="mt-xxs shrink-0 text-error" />
        {content}
      </div>
    );
  }
  return (
    <div className="mb-lg rounded-xl border border-solid border-line-strong bg-surface px-md py-sm text-body-md text-ink-body wrap-anywhere">
      {content}
    </div>
  );
}

function nextStepOf(detail: IssueDetailResponse | undefined, dispatchId: string): string | null {
  if (detail === undefined || detail.status_text_full === "") {
    return null;
  }
  const latest = latestSessionOf(detail);
  return latest?.dispatchId === dispatchId ? detail.status_text_full : null;
}

interface KickerProps {
  displayKey: string;
  title: string | null;
}

function Kicker({ displayKey, title }: KickerProps) {
  return (
    <p className="mb-xs flex flex-wrap items-baseline gap-sm text-body-sm text-ink-muted">
      <span className="whitespace-nowrap tabular-nums">{displayKey}</span>
      {title === null ? undefined : <span>{title}</span>}
    </p>
  );
}

interface FactProps {
  mono?: boolean;
  children: string;
}

function Fact({ mono = false, children }: FactProps) {
  return (
    <span className={mono ? "font-mono whitespace-nowrap" : "whitespace-nowrap"}>{children}</span>
  );
}

function statusText(session: ResolvedSession, config: ConfigResponse | undefined): string {
  if (session.kind === "finished") {
    const { run } = session;
    const { word } = runOutcome(run);
    return run.turns_completed < 1
      ? word
      : `${word}, ${plural(run.turns_completed, "turn", "turns")}`;
  }
  const turns = session.running.turn_count;
  if (turns < 1) {
    return "Running";
  }
  return config === undefined
    ? `Running, turn ${turns}`
    : `Running, turn ${turns} of ${config.agent.max_turns}`;
}

interface FactsProps {
  session: ResolvedSession;
  config: ConfigResponse | undefined;
}

function Facts({ session, config }: FactsProps) {
  const now = useNow();
  const isLive = session.kind === "live";
  const source = isLive ? session.running : session.run;
  const model = source.model_name || source.configured_model;
  const route = isLive ? routeText(session.running, config) : runRouteText(session.run, config);
  const startedMs = parseInstant(source.started_at);
  const endedMs = isLive ? now : parseInstant(session.run.completed_at);
  const elapsed = startedMs === null || endedMs === null ? null : (endedMs - startedMs) / 1000;

  return (
    <div className="mb-lg flex flex-wrap items-center gap-md text-body-md text-ink-body">
      {source.agent_kind ? <Fact>{agentLabel(source.agent_kind)}</Fact> : undefined}
      {model ? <Fact>{modelLabel(model)}</Fact> : undefined}
      {route === null ? undefined : <Fact mono>{route}</Fact>}
      {source.reaction_kind ? <Fact>{startLabel(source.reaction_kind, "origin")}</Fact> : undefined}
      {source.ssh_host ? <Fact>{`${source.ssh_host} (SSH)`}</Fact> : undefined}
      <Fact>
        {`${fmtClock(source.started_at)} to ${isLive ? "now" : fmtClock(session.run.completed_at)}`}
      </Fact>
      {elapsed === null ? undefined : <Fact>{fmtDuration(elapsed)}</Fact>}
      <b className="font-bold whitespace-nowrap">{statusText(session, config)}</b>
    </div>
  );
}

interface SessionBodyProps {
  identifier: string;
  dispatchId: string;
  session: ResolvedSession;
  log: RunLog;
  config: ConfigResponse | undefined;
}

function SessionBody({ identifier, dispatchId, session, log, config }: SessionBodyProps) {
  const { paused } = useLive();
  const { data: detail } = useIssueQuery(identifier);
  const held = useHold(log, paused);
  const timeline = useMemo(() => timelineOf(held.items), [held.items]);
  const isLive = session.kind === "live";
  const bars = turnBarsOf(timeline, isLive ? session.running : null);
  const hasInProgress = bars.some((bar) => bar.inProgress);
  const isChartShown = bars.filter((bar) => !bar.inProgress).length >= 2;
  const stallFull = isLive ? session.running.stall_text_full : null;
  const stallOff = isLive ? session.running.stall_off_text : null;
  const modelUsage = session.kind === "finished" ? (session.run.model_usage ?? []) : [];
  const skills = (session.kind === "live" ? session.running.skills : session.run.skills) ?? [];

  return (
    <>
      <Kicker displayKey={session.key} title={session.title} />
      <h1 className="mb-xs font-heading text-h5 font-semibold text-ink">
        {`Session ${session.number} of ${session.key}`}
      </h1>
      <Facts session={session} config={config} />
      {stallFull !== null ? <StallNotice text={stallFull} isStalled /> : undefined}
      {stallFull === null && stallOff !== null ? (
        <StallNotice text={stallOff} isStalled={false} />
      ) : undefined}
      {session.kind === "finished" ? (
        <EndNotice run={session.run} nextStep={nextStepOf(detail, dispatchId)} />
      ) : undefined}
      <div className="flex flex-col gap-xl xl:grid xl:grid-cols-[minmax(0,1fr)_320px] xl:items-start xl:gap-xxxl">
        <div className="flex min-w-0 flex-col gap-xl">
          <Panel title="Events">
            <EventPane
              timeline={timeline}
              end={
                session.kind === "finished"
                  ? {
                      status: session.run.status,
                      declared_outcome: session.run.declared_outcome,
                    }
                  : null
              }
              isNotRetained={held.notRetained}
              isPaused={paused}
              lastEventAt={isLive ? session.running.last_event_at : null}
            />
          </Panel>
          {isChartShown ? (
            <Panel title="Tokens per turn" right={<TokensLegend hasInProgress={hasInProgress} />}>
              <TokensChart bars={bars} />
            </Panel>
          ) : undefined}
        </div>
        <div className="flex min-w-0 flex-col gap-xl">
          <UsagePanel session={session} />
          {modelUsage.length > 0 ? <ModelCostPanel parts={modelUsage} /> : undefined}
          {skills.length > 0 ? <SkillsPanel skills={skills} /> : undefined}
          <SessionPanel session={session} config={config} />
          <SessionsPanel identifier={identifier} dispatchId={dispatchId} />
        </div>
      </div>
    </>
  );
}

interface SessionViewProps {
  identifier: string;
  dispatchId: string;
}

function SessionView({ identifier, dispatchId }: SessionViewProps) {
  const session = useSessionRun(identifier, dispatchId, { drivePages: true });
  const { data: state } = useStateQuery();
  const isUnknown = session.kind === "no_such_session" || session.kind === "unknown_issue";
  const log = useRunEvents(isUnknown ? null : dispatchId);

  if (session.kind === "live" || session.kind === "finished") {
    return (
      <SessionBody
        identifier={identifier}
        dispatchId={dispatchId}
        session={session}
        log={log}
        config={state?.config}
      />
    );
  }
  return (
    <>
      <Kicker displayKey={identifier} title={null} />
      {session.kind === "no_such_session" ? (
        <h1 className="mb-xs font-heading text-h5 font-semibold text-ink">No such session</h1>
      ) : undefined}
      {session.kind === "unknown_issue" ? (
        <p className="text-body-md text-ink-body">{`Sortie no longer knows ${identifier}.`}</p>
      ) : undefined}
      {session.kind === "history_down" ? (
        <p className="text-body-md text-ink-body">Run history could not be read.</p>
      ) : undefined}
    </>
  );
}

export function SessionScreen() {
  const { identifier, dispatchId } = useParams();
  return identifier === undefined || dispatchId === undefined ? undefined : (
    <SessionView key={dispatchId} identifier={identifier} dispatchId={dispatchId} />
  );
}
