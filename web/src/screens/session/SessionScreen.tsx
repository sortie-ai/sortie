import { useMemo } from "react";
import { useLocation, useParams } from "react-router";

import { useIssueQuery, useStateQuery } from "@/api/queries";
import { useHold, useLive, useNow } from "@/app/live";
import { useRunEvents } from "@/app/run-events";
import { AgentStatement } from "@/components/AgentStatement";
import { Link } from "@/components/carried-links";
import { TriangleAlertIcon } from "@/components/icons";
import { Panel } from "@/components/Panel";
import { Parts } from "@/components/Parts";
import { ScreenHeader } from "@/components/ScreenHeader";
import { ServedText } from "@/components/ServedText";
import { StallNotice } from "@/components/StallNotice";
import { buttonStyles } from "@/components/ui/button";
import { MONO } from "@/components/ui/mono";
import { useSessionRun } from "@/components/use-session-run";
import { routeText } from "@/lib/active-rows";
import { cn } from "@/lib/cn";
import { fmtClock, fmtDuration, parseInstant, plural } from "@/lib/format";
import { issueLinkOf, latestSessionOf } from "@/lib/issue-link";
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
  return (
    <div className="mb-xl">
      <Panel title="Outcome">
        {isAlarmed ? (
          <div
            role="status"
            className="flex items-start gap-sm rounded-xl border border-error-border bg-error-container px-md py-sm text-body-md text-ink wrap-anywhere"
          >
            {/* A box one line tall centers the icon on the notice's first line. */}
            <span className="flex h-lh shrink-0 items-center">
              <TriangleAlertIcon className="text-error" />
            </span>
            {content}
          </div>
        ) : (
          <div className="rounded-xl border border-line bg-canvas px-md py-sm text-body-md text-ink-body wrap-anywhere">
            {content}
          </div>
        )}
      </Panel>
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

interface FactProps {
  mono?: boolean;
  children: string;
}

function Fact({ mono = false, children }: FactProps) {
  return <span className={cn("whitespace-nowrap", mono && MONO)}>{children}</span>;
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
    <Parts
      parts={[
        ...(source.agent_kind
          ? [
              <Fact key="agent" mono={agentLabel(source.agent_kind) === source.agent_kind}>
                {agentLabel(source.agent_kind)}
              </Fact>,
            ]
          : []),
        ...(model
          ? [
              <Fact key="model" mono>
                {modelLabel(model)}
              </Fact>,
            ]
          : []),
        ...(route === null
          ? []
          : [
              <Fact key="route" mono>
                {route}
              </Fact>,
            ]),
        ...(source.reaction_kind
          ? [<Fact key="start">{startLabel(source.reaction_kind, "origin")}</Fact>]
          : []),
        ...(source.ssh_host ? [<Fact key="ssh">{`${source.ssh_host} (SSH)`}</Fact>] : []),
        <Fact key="span">
          {`${fmtClock(source.started_at)} to ${isLive ? "now" : fmtClock(session.run.completed_at)}`}
        </Fact>,
        ...(elapsed === null ? [] : [<Fact key="elapsed">{fmtDuration(elapsed)}</Fact>]),
        <b key="status" className="font-bold whitespace-nowrap">
          {statusText(session, config)}
        </b>,
      ]}
    />
  );
}

function Actions({ identifier }: { identifier: string }) {
  const location = useLocation();
  const link = issueLinkOf(identifier, location);
  return (
    <Link
      to={link.to}
      state={link.state}
      className={buttonStyles("secondary")}
      title="Open the issue page"
    >
      {`Open ${identifier}`}
    </Link>
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
      <ScreenHeader
        title={
          session.title === null
            ? `Session ${session.number}`
            : `Session ${session.number}: ${session.title}`
        }
        standfirst={<Facts session={session} config={config} />}
        actions={<Actions identifier={identifier} />}
      />
      {stallFull !== null ? <StallNotice text={stallFull} isStalled /> : undefined}
      {stallFull === null && stallOff !== null ? (
        <StallNotice text={stallOff} isStalled={false} />
      ) : undefined}
      {session.kind === "finished" ? (
        <EndNotice run={session.run} nextStep={nextStepOf(detail, dispatchId)} />
      ) : undefined}
      <div className="flex flex-col gap-xl xl:grid xl:grid-cols-[minmax(0,1fr)_var(--size-margin)] xl:items-start xl:gap-xxxl">
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
            <Panel title="Tokens per turn" note={<TokensLegend hasInProgress={hasInProgress} />}>
              <TokensChart bars={bars} />
            </Panel>
          ) : undefined}
        </div>
        <div className="flex min-w-0 flex-col gap-xl max-xl:contents">
          <div className="empty:hidden max-xl:order-first">
            <SessionsPanel identifier={identifier} dispatchId={dispatchId} />
          </div>
          <UsagePanel session={session} />
          {modelUsage.length > 1 ? <ModelCostPanel parts={modelUsage} /> : undefined}
          {skills.length > 0 ? <SkillsPanel skills={skills} /> : undefined}
          <SessionPanel session={session} config={config} />
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
      <ScreenHeader
        title="Session"
        standfirst={
          session.kind === "no_such_session"
            ? "No such session."
            : session.kind === "unknown_issue"
              ? `Sortie no longer knows ${identifier}.`
              : session.kind === "history_down"
                ? "Run history could not be read."
                : undefined
        }
      />
    </>
  );
}

export function SessionScreen() {
  const { identifier, dispatchId } = useParams();
  return identifier === undefined || dispatchId === undefined ? undefined : (
    <SessionView key={dispatchId} identifier={identifier} dispatchId={dispatchId} />
  );
}
