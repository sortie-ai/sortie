import { useId } from "react";

import { useNow } from "@/app/live";
import { ExternalLinkIcon } from "@/components/icons";
import { BudgetMeter } from "@/components/BudgetMeter";
import { NotedFigure } from "@/components/NotedFigure";
import { Pair, Pairs, Panel } from "@/components/Panel";
import { ServedText } from "@/components/ServedText";
import { StateBadge } from "@/components/StateBadge";
import { TrackerLabel } from "@/components/TrackerLabel";
import { StatusMark } from "@/components/StatusMark";
import { Button, buttonStyles } from "@/components/ui/button";
import { Tooltip, TooltipPopup, TooltipTrigger } from "@/components/ui/tooltip";
import { linkActionOf, useAttentionActions } from "@/components/use-attention-actions";
import { useIssueActions } from "@/components/use-issue-actions";
import { rowMark } from "@/lib/active-rows";
import { budgetOf, moneyBudgetFigure } from "@/lib/budget";
import { cn } from "@/lib/cn";
import { issueCostFigure, liveCostFigure, liveTokensFigure } from "@/lib/figures";
import {
  fmtAgo,
  fmtClock,
  fmtCount,
  fmtDue,
  fmtMoney,
  fmtStamp,
  fmtTokens,
  parseInstant,
  plural,
} from "@/lib/format";
import { agentLabel, modelLabel, reactionLabel, ruleLabel, startLabel } from "@/lib/labels";
import { safeHttpUrl } from "@/lib/links";

import type {
  AttentionItemResponse,
  IssueDetailResponse,
  NextOutcomeResponse,
  PendingReactionResponse,
  RunResponse,
  StateResponse,
} from "@/api/types.gen";
import type { BudgetLine } from "@/lib/budget";
import type { StagePosition } from "@/lib/lane";
import type { Mark } from "@/lib/run-status";

const HEAD_LENGTH = 7;

function dueText(at: string, now: number): string {
  const instant = parseInstant(at);
  return instant === null ? "unknown" : fmtDue((instant - now) / 1000);
}

interface EffectButtonProps {
  label: string;
  /** The effect the control has, shown before activation and read as its description. */
  effect: string;
  isDisabled: boolean;
  onActivate: () => void;
}

function EffectButton({ label, effect, isDisabled, onActivate }: EffectButtonProps) {
  const descriptionId = useId();
  return (
    <>
      <Tooltip>
        <TooltipTrigger
          render={
            <Button
              className="self-start"
              disabled={isDisabled}
              aria-describedby={descriptionId}
              onClick={onActivate}
            />
          }
        >
          {label}
        </TooltipTrigger>
        <TooltipPopup>{effect}</TooltipPopup>
      </Tooltip>
      <span id={descriptionId} className="sr-only">
        {effect}
      </span>
    </>
  );
}

interface AttentionCardProps {
  item: AttentionItemResponse;
  now: number;
}

function AttentionCard({ item, now }: AttentionCardProps) {
  const actions = useAttentionActions();
  const link = linkActionOf(item);
  const key = item.display_identifier || item.issue_identifier;
  const isCritical = item.severity === "critical";
  const isAcknowledged = item.acknowledged_at !== null;
  const isPending = actions.pending.has(item.id);

  return (
    <li className="grid grid-cols-[minmax(0,1fr)_auto] gap-x-md gap-y-xxs rounded-xl border border-line bg-canvas p-md">
      <p className="flex min-w-0 flex-wrap items-baseline gap-x-sm gap-y-xxs">
        <strong
          className={cn(
            "text-body-md font-bold",
            isAcknowledged ? "text-ink-body" : isCritical ? "text-error" : "text-warning",
          )}
        >
          <ServedText text={item.reason_text} />
        </strong>
        {item.issue_title ? (
          <span className="text-body-md font-medium text-ink">{item.issue_title}</span>
        ) : undefined}
        {key ? (
          <span className="text-body-sm whitespace-nowrap text-ink-muted tabular-nums">{key}</span>
        ) : undefined}
      </p>
      <span className="text-body-sm whitespace-nowrap text-ink-muted tabular-nums">
        {item.since === null ? undefined : fmtAgo(item.since, now)}
      </span>
      <p className="col-start-1 text-body-md text-ink-body">
        <ServedText text={item.why_text} />
      </p>
      <p className="col-start-1 text-body-sm text-ink-muted">
        <ServedText text={item.clears_text} />
      </p>
      <div className="col-span-2 flex flex-wrap items-center gap-xs pt-xs">
        {link === null ? undefined : (
          <a
            href={link.url}
            target="_blank"
            rel="noopener noreferrer"
            title={link.title}
            className={buttonStyles(isAcknowledged ? "secondary" : "primary")}
          >
            <ExternalLinkIcon />
            {link.label}
          </a>
        )}
        {isAcknowledged ? (
          <span className="text-body-sm text-ink-muted">
            Acknowledged {fmtStamp(item.acknowledged_at ?? "")}
          </span>
        ) : (
          <Button
            variant={link === null ? "primary" : "secondary"}
            disabled={isPending}
            onClick={() => void actions.acknowledge(item)}
          >
            {isPending ? "Acknowledging" : "Acknowledge"}
          </Button>
        )}
      </div>
    </li>
  );
}

interface RetryPanelProps {
  detail: IssueDetailResponse;
  now: number;
}

function RetryPanel({ detail, now }: RetryPanelProps) {
  const actions = useIssueActions();
  const { retry } = detail;
  if (detail.status !== "retrying" || retry === null) {
    return undefined;
  }
  const isDue = (parseInstant(retry.due_at) ?? 0) <= now;
  const isUsageLimit = retry.trigger === "usage_limit";
  return (
    <Panel title={`Retry ${retry.attempt}`}>
      <Pairs>
        <Pair label="Due">{dueText(retry.due_at, now)}</Pair>
        {isUsageLimit ? (
          <Pair label="Waiting for">the usage limit to reset</Pair>
        ) : (
          <>
            {retry.trigger ? (
              <Pair label="Started by">{startLabel(retry.trigger, "trigger")}</Pair>
            ) : undefined}
            {retry.error === "" ? undefined : <Pair label="Error">{retry.error}</Pair>}
          </>
        )}
      </Pairs>
      <EffectButton
        label="Retry now"
        effect={`Starts retry ${retry.attempt} now instead of at ${fmtClock(retry.due_at)}`}
        isDisabled={isDue || actions.isRetrying(detail.issue_identifier)}
        onActivate={() => actions.retryNow(detail.issue_identifier)}
      />
    </Panel>
  );
}

function triesText(row: PendingReactionResponse): string | undefined {
  if (row.used_attempts === null) {
    return undefined;
  }
  return row.max_attempts === null
    ? `${row.used_attempts} fixes used`
    : `${row.used_attempts} of ${row.max_attempts} fixes used`;
}

function watchState(
  row: PendingReactionResponse,
  detail: IssueDetailResponse,
): { word: string; mark: Mark } {
  const check = row.last_check;
  if (detail.running?.reaction_kind === row.kind) {
    return { word: "fixing", mark: "running" };
  }
  switch (row.kind) {
    case "ci":
      if (check?.status === "failing") return { word: "checks failing", mark: "failed" };
      if (check?.status === "passing") return { word: "checks passing", mark: "succeeded" };
      break;
    case "review":
    case "bot-review":
      if (row.wait === "debounce") return { word: "comments arriving", mark: "waiting" };
      if (check?.handled === true) return { word: "handled", mark: "succeeded" };
      if ((check?.new_comments ?? 0) > 0) return { word: "comments to answer", mark: "waiting" };
      if (check?.awaiting_approval === true)
        return { word: "waiting on approval", mark: "waiting" };
      break;
    case "merge-conflict":
      if (check?.mergeable === false) return { word: "conflict", mark: "failed" };
      break;
  }
  return { word: "waiting", mark: "waiting" };
}

interface ReactionLineProps {
  row: PendingReactionResponse;
  detail: IssueDetailResponse;
  state: StateResponse | undefined;
}

function ReactionLine({ row, detail, state }: ReactionLineProps) {
  const actions = useIssueActions();
  const label = reactionLabel(row.config_key);
  // The check reads the CI signal, not the fix Sortie makes, so the CI station's new name does not apply here.
  const checked = row.config_key === "ci_failure" ? "CI" : label;
  const tries = triesText(row);
  const watch =
    row.escalated_at === null
      ? watchState(row, detail)
      : {
          word: "escalated",
          mark: rowMark("pending_reactions", detail.issue_id, state?.attention ?? []),
        };

  return (
    <li className="grid grid-cols-[minmax(0,1fr)_auto] gap-x-sm gap-y-xxs border-t border-line py-xs first:border-t-0">
      <StatusMark
        mark={watch.mark}
        label={`${label}: ${watch.word}`}
        className="items-start text-body-md font-medium text-ink"
      />
      <span className="text-body-sm whitespace-nowrap text-ink-muted tabular-nums">
        {fmtClock(row.next_poll_at)}
      </span>
      {tries === undefined ? undefined : (
        <p className="col-start-1 pl-[calc(var(--size-icon)+var(--spacing-xs))] text-body-sm text-ink-muted">
          {tries}
        </p>
      )}
      <div className="col-span-2 pl-[calc(var(--size-icon)+var(--spacing-xs))]">
        <EffectButton
          label="Check now"
          effect={`Checks ${checked} now instead of at ${fmtClock(row.next_poll_at)}`}
          isDisabled={actions.isChecking(detail.issue_identifier, row.kind)}
          onActivate={() => actions.checkNow(detail.issue_identifier, row.kind)}
        />
      </div>
    </li>
  );
}

function pullRequestGroups(rows: readonly PendingReactionResponse[]): PendingReactionResponse[][] {
  const groups = new Map<string, PendingReactionResponse[]>();
  for (const row of rows) {
    const key = JSON.stringify([row.pull_request.repository, row.pull_request.number]);
    groups.set(key, [...(groups.get(key) ?? []), row]);
  }
  return [...groups.values()];
}

interface PullRequestPanelProps {
  rows: readonly PendingReactionResponse[];
  detail: IssueDetailResponse;
  state: StateResponse | undefined;
}

function PullRequestPanel({ rows, detail, state }: PullRequestPanelProps) {
  const [first] = rows;
  if (first === undefined) {
    return undefined;
  }
  const { number, repository, url, branch, head_sha: head } = first.pull_request;
  const href = safeHttpUrl(url);

  return (
    <Panel title={`Pull request #${number}`}>
      <Pairs>
        <Pair label="Repository">
          {href === null ? (
            repository
          ) : (
            <a href={href} target="_blank" rel="noopener noreferrer" className="underline">
              {repository}
            </a>
          )}
        </Pair>
        {branch ? (
          <Pair label="Branch">
            <span className="font-mono text-body-sm">{branch}</span>
          </Pair>
        ) : undefined}
        {head ? (
          <Pair label="Head">
            <span className="font-mono text-body-sm">{head.slice(0, HEAD_LENGTH)}</span>
          </Pair>
        ) : undefined}
      </Pairs>
      <ul className="flex flex-col">
        {rows.map((row) => (
          <ReactionLine key={row.kind} row={row} detail={detail} state={state} />
        ))}
      </ul>
    </Panel>
  );
}

interface NextOutcomesProps {
  outcomes: readonly NextOutcomeResponse[];
}

export function NextOutcomes({ outcomes }: NextOutcomesProps) {
  return (
    <ul className="flex flex-col gap-xs">
      {outcomes.map((outcome) => (
        <li key={outcome.condition} className="text-body-md text-ink-body">
          <strong className="font-medium text-ink">
            <ServedText text={outcome.condition} />.
          </strong>{" "}
          <ServedText text={outcome.text} />
        </li>
      ))}
    </ul>
  );
}

function noTokenBudgetText(tokens: BudgetLine & { complete: boolean }): string | undefined {
  if (!tokens.limitKnown) {
    return undefined;
  }
  const hasBudget = tokens.limit !== null && tokens.limit > 0;
  if (tokens.used === null) {
    return hasBudget ? "Tokens not reported." : "No token budget: tokens not reported.";
  }
  return hasBudget ? undefined : "No token budget.";
}

interface CostShare {
  name: string;
  usd: number;
  hasPriced: boolean;
  hasUnpriced: boolean;
  /** True when a run of the share is a lower bound or reported no usage. */
  isIncomplete: boolean;
}

/** Folds the runs of a share as the issue's own cost folds them. */
function shareStatus(share: CostShare): string {
  if (share.hasPriced) {
    return share.hasUnpriced || share.isIncomplete ? "partial" : "estimated";
  }
  return share.hasUnpriced ? "unpriced" : "unmeasured";
}

function costShares(runs: readonly RunResponse[], chained: boolean): CostShare[] {
  const shares = new Map<string, CostShare>();
  const stageRuns = runs.filter((run) => !run.reaction_kind).length;
  for (const run of runs) {
    const name = run.reaction_kind
      ? "PR fixes"
      : chained
        ? plural(stageRuns, "stage run", "stage runs")
        : run.rule_name
          ? ruleLabel(run.rule_name)
          : "Work";
    const before = shares.get(name) ?? {
      name,
      usd: 0,
      hasPriced: false,
      hasUnpriced: false,
      isIncomplete: false,
    };
    const usd = run.estimated_cost_usd;
    shares.set(name, {
      name,
      usd: before.usd + (usd ?? 0),
      hasPriced: before.hasPriced || usd !== null,
      hasUnpriced: before.hasUnpriced || run.cost_status === "unpriced",
      isIncomplete:
        before.isIncomplete ||
        run.cost_status === "partial" ||
        run.cost_status === "unmeasured" ||
        run.usage_completeness === "lower_bound",
    });
  }
  return [...shares.values()];
}

interface BudgetPanelProps {
  detail: IssueDetailResponse;
  state: StateResponse | undefined;
  chained: boolean;
}

function BudgetPanel({ detail, state, chained }: BudgetPanelProps) {
  const figures = budgetOf(detail, state);
  const { sessions, tokens, money, cost } = figures;
  const note = noTokenBudgetText(tokens);
  const moneyFigure = moneyBudgetFigure(figures);
  const moneyText =
    moneyFigure === undefined || money.limit === null ? undefined : (
      <>
        <NotedFigure figure={moneyFigure} /> of {fmtMoney(money.limit)}
      </>
    );
  const shares = cost === null ? [] : costShares(detail.history?.runs ?? [], chained);
  const hasPairs = cost !== null && (moneyText === undefined || shares.length > 0);

  return (
    <Panel title={cost === null ? "Budget" : "Budget and cost"}>
      <div className="flex flex-col gap-sm">
        {sessions.limit === null ? (
          <p className="text-body-md text-ink-body">
            {`${plural(sessions.used ?? 0, "session", "sessions")}, no session budget.`}
          </p>
        ) : (
          <BudgetMeter name="Sessions" line={sessions} format={fmtCount} />
        )}
        <BudgetMeter
          name="Tokens"
          line={tokens}
          format={fmtTokens}
          text={
            tokens.used === null || tokens.limit === null || tokens.complete ? undefined : (
              <>
                <NotedFigure figure={liveTokensFigure(tokens.used, false)} /> of{" "}
                {fmtTokens(tokens.limit)}
              </>
            )
          }
        />
        {note === undefined ? undefined : <p className="text-body-sm text-ink-muted">{note}</p>}
        {moneyText === undefined ? undefined : (
          <BudgetMeter name="Cost" line={money} format={fmtMoney} text={moneyText} />
        )}
      </div>
      {cost === null || !hasPairs ? undefined : (
        <Pairs>
          {moneyText === undefined ? (
            <Pair label="Estimated cost">
              <strong className="font-bold">
                <NotedFigure figure={issueCostFigure(detail, cost)} />
              </strong>
            </Pair>
          ) : undefined}
          {shares.map((share) => (
            <Pair key={share.name} label={share.name}>
              <NotedFigure
                figure={liveCostFigure(
                  shareStatus(share),
                  share.hasPriced ? share.usd : null,
                  "runs",
                )}
              />
            </Pair>
          ))}
        </Pairs>
      )}
    </Panel>
  );
}

function agentOf(detail: IssueDetailResponse): string | undefined {
  const kind = detail.running?.agent_kind ?? detail.history?.runs?.[0]?.agent_kind;
  return kind ? agentLabel(kind) : undefined;
}

function modelOf(detail: IssueDetailResponse): string {
  const { running } = detail;
  const last = detail.history?.runs?.[0];
  const model =
    running?.model_name || running?.configured_model || last?.model_name || last?.configured_model;
  return model ? modelLabel(model) : "agent default";
}

function ruleOf(detail: IssueDetailResponse): string | undefined {
  return (
    detail.running?.rule_name ||
    detail.retry?.rule_name ||
    detail.parked?.rule_name ||
    detail.history?.runs?.[0]?.rule_name ||
    undefined
  );
}

interface RoutePanelProps {
  detail: IssueDetailResponse;
  position: StagePosition | null;
}

function RoutePanel({ detail, position }: RoutePanelProps) {
  const agent = agentOf(detail);
  const rule = ruleOf(detail);
  if (agent === undefined && rule === undefined && position === null) {
    return undefined;
  }
  const k = position === null ? 0 : position.chain.indexOf(position.current) + 1;
  return (
    <Panel title={rule === undefined && position === null ? "Agent" : "Route"}>
      <Pairs>
        {position === null ? (
          rule === undefined ? undefined : (
            <Pair label="Rule">{ruleLabel(rule)}</Pair>
          )
        ) : (
          <>
            <Pair label="Entry rule">{ruleLabel(position.chain[0] ?? position.current)}</Pair>
            <Pair label="Stage">{`${ruleLabel(position.current)}, ${k} of ${position.chain.length}`}</Pair>
          </>
        )}
        {agent === undefined ? undefined : <Pair label="Agent">{agent}</Pair>}
        {agent === undefined ? undefined : <Pair label="Model">{modelOf(detail)}</Pair>}
      </Pairs>
    </Panel>
  );
}

interface IssuePanelProps {
  detail: IssueDetailResponse;
}

function IssuePanel({ detail }: IssuePanelProps) {
  const { state, issue_type: type, labels = [] } = detail.issue;
  if (!state && !type && labels.length === 0) {
    return undefined;
  }
  return (
    <Panel title="Issue">
      <Pairs>
        {state ? (
          <Pair label="Status">
            <StateBadge state={state} />
          </Pair>
        ) : undefined}
        {type ? <Pair label="Type">{type}</Pair> : undefined}
        <Pair label="Labels">
          {labels.length === 0 ? (
            "none"
          ) : (
            <span className="flex flex-wrap gap-xxs">
              {labels.map((label) => (
                <TrackerLabel key={label}>{label}</TrackerLabel>
              ))}
            </span>
          )}
        </Pair>
      </Pairs>
    </Panel>
  );
}

export interface IssueMarginProps {
  detail: IssueDetailResponse;
  state: StateResponse | undefined;
  position: StagePosition | null;
}

export function IssueMargin({ detail, state, position }: IssueMarginProps) {
  const now = useNow();
  const attention = (state?.attention ?? []).filter((item) => item.issue_id === detail.issue_id);
  const pullRequests = pullRequestGroups(detail.pending_reactions ?? []);
  const nextOutcomes = detail.next_outcomes ?? [];

  return (
    <>
      {attention.length === 0 ? undefined : (
        <ul className="flex flex-col gap-sm">
          {attention.map((item) => (
            <AttentionCard key={item.id} item={item} now={now} />
          ))}
        </ul>
      )}
      <RetryPanel detail={detail} now={now} />
      {nextOutcomes.length === 0 ? undefined : (
        <Panel title="What happens next">
          <NextOutcomes outcomes={nextOutcomes} />
        </Panel>
      )}
      {pullRequests.map((rows) => (
        <PullRequestPanel
          key={JSON.stringify([rows[0]?.pull_request.repository, rows[0]?.pull_request.number])}
          rows={rows}
          detail={detail}
          state={state}
        />
      ))}
      <BudgetPanel detail={detail} state={state} chained={position !== null} />
      <RoutePanel detail={detail} position={position} />
      <IssuePanel detail={detail} />
    </>
  );
}
