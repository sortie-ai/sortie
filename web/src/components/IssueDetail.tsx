import { Link, useLocation } from "react-router";

import { ApiError } from "@/api/client";
import { useHistoryRunsQuery, useIssueQuery, useStateQuery } from "@/api/queries";
import { keyTip } from "@/app/key-tips";
import { useNow } from "@/app/live";
import { ExternalLinkIcon } from "@/components/icons";
import { IssueMargin, NextOutcomes } from "@/components/IssueMargin";
import { IssueStory, storyRunsOf } from "@/components/IssueStory";
import { Lane } from "@/components/Lane";
import { NotedFigure } from "@/components/NotedFigure";
import { Parts } from "@/components/Parts";
import { ServedText } from "@/components/ServedText";
import { StallNotice } from "@/components/StallNotice";
import { StateBadge } from "@/components/StateBadge";
import { Button, buttonStyles } from "@/components/ui/button";
import { DrawerClose, DrawerTitle } from "@/components/ui/drawer";
import { useAttentionActions } from "@/components/use-attention-actions";
import { budgetOf, moneyBudgetFigure } from "@/lib/budget";
import { cn } from "@/lib/cn";
import {
  NOT_MEASURED,
  issueCostFigure,
  liveCostFigure,
  liveTokensFigure,
  runningTokensText,
} from "@/lib/figures";
import { closingClock } from "@/lib/ended";
import { fmtClock, fmtMoney, lowerFirst, plural, upperFirst } from "@/lib/format";
import { issueLinkOf, latestSessionOf, sessionLinkOf } from "@/lib/issue-link";
import { agentLabel, hopLabel, modelLabel, ruleLabel } from "@/lib/labels";
import {
  decidingOf,
  laneOf,
  lastStageRunOf,
  positionOf,
  stageProgressOf,
  standingOf,
  stationsOf,
} from "@/lib/lane";
import { safeHttpUrl } from "@/lib/links";
import { numberedRunsOf } from "@/lib/run-number";
import { runOutcome } from "@/lib/run-status";

import type { ReactNode } from "react";

import type {
  AttentionItemResponse,
  IssueDetailResponse,
  PullRequestResponse,
  RunsResponse,
  StateResponse,
} from "@/api/types.gen";
import type { LaneStation, StagePosition, StageProgress } from "@/lib/lane";

const PEEK_RUNS = 4;

/** What a list already knows of an issue; shown until the issue's own response arrives. */
export interface IssueDetailRow {
  displayKey: string;
  title: string | null;
  statusTextFull: string;
}

export interface IssueDetailProps {
  identifier: string;
  row?: IssueDetailRow;
  /** Renders the drawer layout with "Open page" and "Close" instead of the page. */
  peek?: boolean;
}

interface IssueView {
  lane: LaneStation[] | undefined;
  position: StagePosition | null;
  progress: StageProgress | null;
  isLive: boolean;
}

function viewOf(
  detail: IssueDetailResponse,
  state: StateResponse | undefined,
  now: number,
  olderPages: readonly RunsResponse[],
): IssueView {
  const active = (state?.active ?? []).find((entry) => entry.issue_id === detail.issue_id);
  const deciding = decidingOf(detail, active);
  const position = positionOf(detail, deciding);
  const stations = state?.config === undefined ? undefined : stationsOf(state.config);
  const standing =
    stations === undefined
      ? null
      : standingOf(
          {
            deciding,
            outcome: detail.history?.issue_outcome ?? null,
            issueId: detail.issue_id,
            attention: state?.attention ?? [],
            now,
          },
          stations,
        );
  const station = stations?.find((candidate) => candidate.key === standing?.station)?.kind ?? null;
  return {
    lane:
      stations === undefined
        ? undefined
        : laneOf(stations, standing, detail.pending_reactions ?? [], detail.pr_loop),
    position,
    progress:
      position === null
        ? null
        : stageProgressOf(
            position,
            deciding !== null,
            station,
            lastStageRunOf(storyRunsOf(detail.history?.runs ?? [], olderPages), position.current),
          ),
    isLive: deciding !== null,
  };
}

function pullRequestOf(detail: IssueDetailResponse): PullRequestResponse | undefined {
  return detail.pending_reactions?.[0]?.pull_request ?? detail.history?.issue_outcome?.pull_request;
}

interface ExternalButtonProps {
  href: string;
  title?: string;
  children: ReactNode;
}

function ExternalButton({ href, title, children }: ExternalButtonProps) {
  return (
    <a
      href={href}
      target="_blank"
      rel="noopener noreferrer"
      title={title}
      className={buttonStyles("secondary")}
    >
      <ExternalLinkIcon />
      {children}
    </a>
  );
}

interface ActionsProps {
  detail: IssueDetailResponse;
  /** The open attention items the peek acknowledges from its action row; the page acknowledges them in its margin. */
  attention?: readonly AttentionItemResponse[];
}

function Actions({ detail, attention = [] }: ActionsProps) {
  const actions = useAttentionActions();
  const location = useLocation();
  const trackerUrl = safeHttpUrl(detail.issue.url);
  const pullRequest = pullRequestOf(detail);
  const pullRequestUrl = safeHttpUrl(pullRequest?.url);
  const session = latestSessionOf(detail);
  if (
    trackerUrl === null &&
    pullRequestUrl === null &&
    attention.length === 0 &&
    session === null
  ) {
    return undefined;
  }
  const sessionLink =
    session === null
      ? undefined
      : sessionLinkOf(detail.issue_identifier, session.dispatchId, location);
  return (
    <div className="flex flex-wrap gap-xs">
      {attention.map((item) => (
        <Button
          key={item.id}
          variant="primary"
          disabled={actions.pending.has(item.id)}
          title={keyTip("Acknowledge", "E")}
          onClick={() => void actions.acknowledge(item)}
        >
          {actions.pending.has(item.id) ? "Acknowledging" : "Acknowledge"}
        </Button>
      ))}
      {session === null || sessionLink === undefined ? undefined : (
        <Link
          to={sessionLink.to}
          state={sessionLink.state}
          className={buttonStyles("secondary")}
          title={keyTip("Open session", "O")}
        >
          {session.isLive ? "Open live session" : "Last session"}
        </Link>
      )}
      {trackerUrl === null ? undefined : (
        <ExternalButton href={trackerUrl}>Open issue</ExternalButton>
      )}
      {pullRequest === undefined || pullRequestUrl === null ? undefined : (
        <ExternalButton href={pullRequestUrl}>{`PR #${pullRequest.number}`}</ExternalButton>
      )}
    </div>
  );
}

interface KickerProps {
  detail: IssueDetailResponse | undefined;
}

function Kicker({ detail }: KickerProps) {
  const state = detail?.issue.state;
  const type = detail?.issue.issue_type;
  if (!state && !type) {
    return undefined;
  }
  return (
    <p className="flex flex-wrap items-center gap-x-sm gap-y-xxs text-body-sm text-ink-muted">
      {state ? <StateBadge state={state} /> : undefined}
      {type ? <span>{type}</span> : undefined}
    </p>
  );
}

function agentParts(detail: IssueDetailResponse): string[] {
  const { running } = detail;
  const last = detail.history?.runs?.[0];
  const kind = running?.agent_kind ?? last?.agent_kind;
  const model =
    running?.model_name || running?.configured_model || last?.model_name || last?.configured_model;
  if (!kind) {
    return [];
  }
  return model ? [agentLabel(kind), modelLabel(model)] : [agentLabel(kind)];
}

interface PeekFactsProps {
  detail: IssueDetailResponse;
  state: StateResponse | undefined;
  position: StagePosition | null;
}

function PeekFacts({ detail, state, position }: PeekFactsProps) {
  const config = state?.config;
  const figures = budgetOf(detail, state);
  const { sessions, tokens, money, uncachedTokens, cost } = figures;
  const moneyFigure = moneyBudgetFigure(figures);
  const { running } = detail;
  const pullRequest = pullRequestOf(detail);
  const agent = agentParts(detail);
  const facts: [string, ReactNode][] = [];
  if (position !== null) {
    const k = position.chain.indexOf(position.current) + 1;
    facts.push([
      "Route",
      <Parts
        key="route"
        parts={[ruleLabel(position.current), `${k} of ${position.chain.length}`]}
      />,
    ]);
  }
  if (agent.length > 0) {
    facts.push(["Agent", <Parts key="agent" parts={agent} />]);
  }
  if (running !== null) {
    const tokensText = runningTokensText(running);
    const sessionCost = liveCostFigure(running.cost_status, running.estimated_cost_usd, "session");
    const parts = [
      config === undefined
        ? `turn ${running.turn_count}`
        : `turn ${running.turn_count} of ${config.agent.max_turns}`,
      tokensText === NOT_MEASURED ? "tokens not measured" : `${tokensText} tokens`,
    ];
    facts.push([
      "This session",
      <Parts
        key="session"
        parts={[
          ...parts,
          ...(cost === null || sessionCost.text === NOT_MEASURED
            ? []
            : [
                <span key="cost">
                  <NotedFigure figure={sessionCost} /> est.
                </span>,
              ]),
        ]}
      />,
    ]);
  }
  facts.push([
    "Sessions",
    sessions.limit === null
      ? String(sessions.used ?? 0)
      : `${sessions.used ?? 0} of ${sessions.limit}`,
  ]);
  if (uncachedTokens === null) {
    facts.push([
      "Tokens",
      <span key="tokens" className="text-ink-muted italic">
        {NOT_MEASURED}
      </span>,
    ]);
  } else if (cost !== null) {
    facts.push([
      "Spent",
      <>
        <NotedFigure figure={issueCostFigure(detail, cost)} />
        {moneyFigure === undefined || money.limit === null
          ? undefined
          : ` of ${fmtMoney(money.limit)}`}
        {" estimated, "}
        <NotedFigure figure={liveTokensFigure(uncachedTokens, tokens.complete)} />
        {" tokens"}
      </>,
    ]);
  } else {
    facts.push([
      "Tokens",
      <NotedFigure key="tokens" figure={liveTokensFigure(uncachedTokens, tokens.complete)} />,
    ]);
  }
  if (pullRequest !== undefined) {
    facts.push([
      "Pull request",
      pullRequest.branch ? (
        <Parts
          key="pull-request"
          parts={[
            `#${pullRequest.number}`,
            <span key="branch" className="font-mono">
              {pullRequest.branch}
            </span>,
          ]}
        />
      ) : (
        `#${pullRequest.number}`
      ),
    ]);
  }
  return (
    <dl className="grid grid-cols-[120px_minmax(0,1fr)] gap-x-md gap-y-xs text-body-md">
      {facts.map(([label, value]) => (
        <div key={label} className="contents">
          <dt className="text-ink-muted">{label}</dt>
          <dd className="text-ink">{value}</dd>
        </div>
      ))}
    </dl>
  );
}

interface PeekRunsProps {
  detail: IssueDetailResponse;
  now: number;
}

function PeekRuns({ detail, now }: PeekRunsProps) {
  const served = detail.history?.runs ?? [];
  const total = detail.history?.totals.run_count ?? served.length;
  const shown = numberedRunsOf(served, total).slice(-PEEK_RUNS);
  const earlier = total - shown.length;
  const node = "relative z-(--z-raised) mt-xs ml-xxs size-2.25 rounded-full border-[1.5px]";
  return (
    <ul className="relative before:absolute before:top-xs before:bottom-xs before:left-15 before:w-px before:bg-line">
      {earlier > 0 ? (
        <li className="grid grid-cols-[48px_20px_minmax(0,1fr)] gap-x-sm py-xs text-body-md">
          <span />
          <span aria-hidden="true" className={cn(node, "border-ink-muted bg-canvas")} />
          <span className="text-ink-muted">{plural(earlier, "earlier run", "earlier runs")}</span>
        </li>
      ) : undefined}
      {shown.map(({ run, number }) => (
        <li
          key={run.run_id}
          className="grid grid-cols-[48px_20px_minmax(0,1fr)] gap-x-sm py-xs text-body-md"
        >
          <span className="pt-xxs text-right text-body-sm text-ink-muted tabular-nums">
            {fmtClock(run.started_at)}
          </span>
          <span aria-hidden="true" className={cn(node, "border-ink-body bg-ink-body")} />
          <span className="text-ink-body">
            <Parts
              parts={[
                `Run ${number}`,
                ...(run.rule_name ? [ruleLabel(run.rule_name)] : []),
                `${lowerFirst(runOutcome(run).word)} at ${fmtClock(run.completed_at)}`,
              ]}
            />
            {run.stage_target && run.stage_result ? (
              <span className="block text-body-sm text-ink-muted">
                {upperFirst(hopLabel(run.stage_target, run.stage_result))}
              </span>
            ) : undefined}
          </span>
        </li>
      ))}
      <li className="grid grid-cols-[48px_20px_minmax(0,1fr)] gap-x-sm py-xs text-body-md">
        <span className="pt-xxs text-right text-body-sm text-ink-muted tabular-nums">
          {closingClock(detail, now)}
        </span>
        <span
          aria-hidden="true"
          className={cn(
            node,
            "border-primary bg-primary shadow-[0_0_0_3px_var(--color-surface-sunken)]",
          )}
        />
        <span className="text-ink">
          <ServedText text={detail.status_text_full} />
        </span>
      </li>
    </ul>
  );
}

function SubHeading({ children }: { children: ReactNode }) {
  return <h3 className="mt-lg mb-sm text-body-sm font-medium text-ink-muted">{children}</h3>;
}

export function IssueDetail({ identifier, row, peek = false }: IssueDetailProps) {
  const { data, error } = useIssueQuery(identifier);
  const { data: state } = useStateQuery();
  const now = useNow();
  const location = useLocation();
  // Disabled here: IssueStory enables the same cached query, and this reads the pages it loaded.
  const { data: older } = useHistoryRunsQuery(data?.issue_id ?? "", {}, false);
  const key = row?.displayKey ?? identifier;
  const failure = error instanceof ApiError ? error : undefined;
  const isGone = failure?.status === 404;
  const isHistoryDown = failure?.status === 503 && failure.code === "history_unavailable";
  const title = data?.issue.title || row?.title;
  const sentence = data?.status_text_full || row?.statusTextFull;
  const view = data === undefined ? undefined : viewOf(data, state, now, older?.pages ?? []);
  const needsAttention = (state?.attention ?? []).some(
    (item) => item.issue_id === data?.issue_id && item.acknowledged_at === null,
  );
  const link = issueLinkOf(identifier, location);
  const stallText = data?.running?.stall_text_full ?? null;

  if (peek) {
    return (
      <div className="flex flex-col p-lg max-md:p-md">
        <div className="mb-md flex items-center gap-sm text-body-sm text-ink-muted">
          <DrawerTitle className="text-body-sm font-normal text-ink-body tabular-nums">
            {key}
          </DrawerTitle>
          {data?.issue.state ? <StateBadge state={data.issue.state} /> : undefined}
          <span className="flex-1" />
          <Link
            to={link.to}
            state={link.state}
            className={buttonStyles("secondary")}
            title={keyTip("Open page", "Enter")}
          >
            Open page
          </Link>
          <DrawerClose title={keyTip("Close", "Esc")}>Close</DrawerClose>
        </div>
        {isGone ? (
          <p className="text-body-md text-ink-body">Sortie no longer knows {key}.</p>
        ) : (
          <>
            {title ? (
              <p className="mb-xs font-heading text-h6 font-semibold text-ink">{title}</p>
            ) : undefined}
            {sentence ? (
              <p className="mb-md text-body-md text-ink-body">
                <ServedText text={sentence} />
              </p>
            ) : undefined}
            {view?.lane === undefined ? undefined : (
              <div className="mb-md">
                <Lane
                  stations={view.lane}
                  position={view.position}
                  progress={view.progress}
                  live={view.isLive}
                  variant="peek"
                />
              </div>
            )}
            {data === undefined ? undefined : (
              <>
                <div className="mb-lg">
                  <Actions
                    detail={data}
                    attention={(state?.attention ?? []).filter(
                      (item) => item.issue_id === data.issue_id && item.acknowledged_at === null,
                    )}
                  />
                </div>
                <PeekFacts detail={data} state={state} position={view?.position ?? null} />
                {(data.next_outcomes ?? []).length === 0 ? undefined : (
                  <>
                    <SubHeading>What happens next</SubHeading>
                    <NextOutcomes outcomes={data.next_outcomes ?? []} />
                  </>
                )}
                <SubHeading>So far</SubHeading>
                <PeekRuns detail={data} now={now} />
              </>
            )}
          </>
        )}
      </div>
    );
  }

  return (
    <div className="flex flex-col">
      <Kicker detail={data} />
      {isGone ? (
        <p className="mt-xs text-body-md text-ink-body">Sortie no longer knows {key}.</p>
      ) : (
        <>
          <h1 className="mt-xs mb-xs font-heading text-h5 font-semibold text-ink">
            {title ?? key}
          </h1>
          {sentence ? (
            <p className="mb-lg text-body-lg text-ink-body">
              <ServedText text={sentence} />
            </p>
          ) : undefined}
          {stallText === null ? undefined : <StallNotice text={stallText} isStalled />}
          {isHistoryDown ? (
            <p className="mb-lg text-body-md text-ink-body">Run history could not be read.</p>
          ) : undefined}
          {data === undefined ? undefined : (
            <div className="mb-lg">
              <Actions detail={data} />
            </div>
          )}
          {view?.lane === undefined ? undefined : (
            <div className="mb-xl">
              <Lane
                stations={view.lane}
                position={view.position}
                progress={view.progress}
                live={view.isLive}
              />
            </div>
          )}
          {data === undefined || view === undefined ? undefined : (
            <div className="flex flex-col gap-xl xl:grid xl:grid-cols-[minmax(0,1fr)_320px] xl:items-start xl:gap-xxxl">
              <div className="min-w-0">
                <IssueStory
                  detail={data}
                  position={view.position}
                  progress={view.progress}
                  needsAttention={needsAttention}
                />
              </div>
              <div className="flex min-w-0 flex-col gap-xl">
                <IssueMargin detail={data} state={state} position={view.position} />
              </div>
            </div>
          )}
        </>
      )}
    </div>
  );
}
