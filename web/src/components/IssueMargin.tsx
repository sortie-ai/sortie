import { useId } from "react";

import { useNow } from "@/app/live";
import { ExternalLink } from "@/components/ExternalLink";
import { ExternalLinkIcon } from "@/components/icons";
import { BudgetMeter, FigureLine } from "@/components/BudgetMeter";
import { NotedFigure } from "@/components/NotedFigure";
import { Pair, Pairs, Panel } from "@/components/Panel";
import { NameText } from "@/components/NameText";
import { Parts } from "@/components/Parts";
import { ServedText } from "@/components/ServedText";
import { StateBadge } from "@/components/StateBadge";
import { TrackerLabel } from "@/components/TrackerLabel";
import { StatusMark } from "@/components/StatusMark";
import { EMPTY_KINDS, SplitBar, hitRateOfKinds, kindsOfTokenInfo } from "@/components/TokenKinds";
import { Button, buttonStyles } from "@/components/ui/button";
import { MONO } from "@/components/ui/mono";
import { Tooltip, TooltipPopup, TooltipTrigger } from "@/components/ui/tooltip";
import { linkActionOf, useAttentionActions } from "@/components/use-attention-actions";
import { useIssueActions } from "@/components/use-issue-actions";
import { rowMark } from "@/lib/active-rows";
import { budgetOf, moneyBudgetFigure } from "@/lib/budget";
import { cn } from "@/lib/cn";
import {
  LABEL_CACHE_HIT_RATE,
  NOT_MEASURED,
  TOKENS_LOWER_BOUND,
  issueCostFigure,
  liveCostFigure,
  liveTokensFigure,
  tokensKindFigure,
} from "@/lib/figures";
import {
  fmtAgo,
  fmtClock,
  fmtCount,
  fmtDue,
  fmtMoney,
  fmtPercent,
  fmtStamp,
  fmtTokens,
  parseInstant,
  plural,
} from "@/lib/format";
import { agentLabel, modelLabel, reactionLabel, ruleLabel, startLabel } from "@/lib/labels";
import { reactionRank } from "@/lib/lane";
import { safeHttpUrl } from "@/lib/links";

import type { ReactNode } from "react";

import type {
  AttentionItemResponse,
  IssueDetailResponse,
  NextOutcomeResponse,
  PendingReactionResponse,
  RunResponse,
  StateResponse,
} from "@/api/types.gen";
import type { KindValues } from "@/components/TokenKinds";
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
  const isCritical = item.severity === "critical";
  const isAcknowledged = item.acknowledged_at !== null;
  const isPending = actions.pending.has(item.id);

  return (
    <li className="grid grid-cols-[minmax(0,1fr)_auto] gap-x-md gap-y-xxs rounded-xl border border-line bg-canvas p-md">
      {/* The margin belongs to the item's issue, so the card names neither its title nor its key. */}
      <strong
        className={cn(
          "min-w-0 text-body-md font-bold",
          isAcknowledged ? "text-ink-body" : isCritical ? "text-error" : "text-warning",
        )}
      >
        <ServedText text={item.reason_text} />
      </strong>
      {/* Takes the first line's height, so the age stands on the reason's baseline. */}
      <span className="text-body-sm leading-(--text-body-md--line-height) whitespace-nowrap text-ink-muted tabular-nums">
        {item.since === null ? undefined : fmtAgo(item.since, now)}
      </span>
      {/* The lines under the head take the card's full width, the age's column included. */}
      <p className="col-span-2 text-body-md text-ink-body">
        <ServedText text={item.why_text} />
      </p>
      <p className="col-span-2 text-body-sm text-ink-muted">
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
    <Panel isRuled title={`Retry ${retry.attempt}`}>
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
        effect={`Starts retry ${retry.attempt} now instead of at ${fmtClock(retry.due_at)}.`}
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
    // The watch and its figures stack beside the check.
    <li className="flex items-center gap-x-sm border-t border-line py-xs first:border-t-0">
      <div className="flex min-w-0 grow flex-col gap-y-xxs">
        <StatusMark
          mark={watch.mark}
          label={`${label}: ${watch.word}`}
          className="items-start text-body-md font-medium text-ink"
        />
        <div className="pl-[calc(var(--size-icon)+var(--spacing-xs))]">
          <Parts
            className="text-body-sm text-ink-muted tabular-nums"
            parts={[
              ...(tries === undefined ? [] : [tries]),
              `next check ${fmtClock(row.next_poll_at)}`,
            ]}
          />
        </div>
      </div>
      <div className="flex shrink-0">
        <EffectButton
          label="Check now"
          effect={`Checks ${checked} now instead of at ${fmtClock(row.next_poll_at)}.`}
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
    <Panel isRuled title={`Pull request #${number}`}>
      <Pairs>
        <Pair label="Repository">
          {href === null ? (
            repository
          ) : (
            <ExternalLink href={href}>
              <span className="min-w-0">{repository}</span>
            </ExternalLink>
          )}
        </Pair>
        {branch ? (
          <Pair label="Branch">
            <span className={MONO}>{branch}</span>
          </Pair>
        ) : undefined}
        {head ? (
          <Pair label="Head">
            <span className={MONO}>{head.slice(0, HEAD_LENGTH)}</span>
          </Pair>
        ) : undefined}
      </Pairs>
      <ul className="flex flex-col">
        {[...rows]
          .sort((a, b) => reactionRank(a.config_key) - reactionRank(b.config_key))
          .map((row) => (
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
        // The condition takes its own line and its effect a smaller one under it.
        <li key={outcome.condition} className="text-body-md">
          <strong className="block font-medium text-pretty text-ink">
            <ServedText text={outcome.condition} />.
          </strong>
          <span className="block text-body-sm text-pretty text-ink-muted">
            <ServedText text={outcome.text} />
          </span>
        </li>
      ))}
    </ul>
  );
}

// Without a token budget the tokens read on a line of their own, as sessions without one do.
function tokensText(tokens: BudgetLine & { complete: boolean }): ReactNode {
  if (!tokens.limitKnown) {
    return undefined;
  }
  const hasBudget = tokens.limit !== null && tokens.limit > 0;
  if (tokens.used === null) {
    return hasBudget ? "not reported" : "not reported, no budget";
  }
  return hasBudget ? undefined : (
    <>
      <NotedFigure figure={liveTokensFigure(tokens.used, tokens.complete)} />, no budget
    </>
  );
}

interface ShareName {
  name: string;
  /** The rule name when the share is one rule's runs, which reads in monospace. */
  rule?: string;
}

function shareNameOf(run: RunResponse, chained: boolean, stageRuns: number): ShareName {
  if (run.reaction_kind) {
    return { name: "PR fixes" };
  }
  if (chained) {
    return { name: plural(stageRuns, "stage run", "stage runs") };
  }
  return run.rule_name ? { name: ruleLabel(run.rule_name), rule: run.rule_name } : { name: "Work" };
}

/** Folds the runs into the shares the budget splits an issue's figures by, in first-seen order. */
function foldShares<T extends ShareName>(
  runs: readonly RunResponse[],
  chained: boolean,
  start: (share: ShareName) => T,
  add: (share: T, run: RunResponse) => T,
): T[] {
  const shares = new Map<string, T>();
  const stageRuns = runs.filter((run) => !run.reaction_kind).length;
  for (const run of runs) {
    const share = shareNameOf(run, chained, stageRuns);
    shares.set(share.name, add(shares.get(share.name) ?? start(share), run));
  }
  return [...shares.values()];
}

interface CostShare extends ShareName {
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
  return foldShares<CostShare>(
    runs,
    chained,
    (share) => ({
      ...share,
      usd: 0,
      hasPriced: false,
      hasUnpriced: false,
      isIncomplete: false,
    }),
    (share, run) => {
      const usd = run.estimated_cost_usd;
      return {
        ...share,
        usd: share.usd + (usd ?? 0),
        hasPriced: share.hasPriced || usd !== null,
        hasUnpriced: share.hasUnpriced || run.cost_status === "unpriced",
        isIncomplete:
          share.isIncomplete ||
          run.cost_status === "partial" ||
          run.cost_status === "unmeasured" ||
          run.usage_completeness === "lower_bound",
      };
    },
  );
}

interface TokenShare extends ShareName {
  tokens: number;
  hasMeasured: boolean;
  /** True when a run of the share was not fully counted. */
  isIncomplete: boolean;
}

function tokenShares(runs: readonly RunResponse[], chained: boolean): TokenShare[] {
  return foldShares<TokenShare>(
    runs,
    chained,
    (share) => ({
      ...share,
      tokens: 0,
      hasMeasured: false,
      isIncomplete: false,
    }),
    (share, run) => ({
      ...share,
      tokens: share.tokens + (run.uncached_tokens ?? 0),
      hasMeasured: share.hasMeasured || run.uncached_tokens !== null,
      isIncomplete: share.isIncomplete || run.usage_completeness !== "complete",
    }),
  );
}

function shareTokensText(share: TokenShare): ReactNode {
  return share.hasMeasured ? (
    <NotedFigure figure={liveTokensFigure(share.tokens, !share.isIncomplete)} />
  ) : (
    NOT_MEASURED
  );
}

interface ShareLineProps<T extends ShareName> {
  shares: readonly T[];
  figureOf: (share: T) => ReactNode;
}

/** Splits a figure by its shares; a single share is the figure itself, so only a split of two or more shows. */
function ShareLine<T extends ShareName>({ shares, figureOf }: ShareLineProps<T>) {
  if (shares.length < 2) {
    return undefined;
  }
  return (
    <Parts
      className="text-body-sm text-ink-muted"
      parts={shares.map((share) => (
        <span key={share.name}>
          {share.rule === undefined ? (
            share.name
          ) : (
            <NameText name={share.rule} label={share.name} />
          )}{" "}
          <span className="text-ink-body">{figureOf(share)}</span>
        </span>
      ))}
    />
  );
}

/** The kinds of the issue's finished runs plus the running session; null while either is not known. */
function issueKindsOf(detail: IssueDetailResponse): KindValues | null {
  const finished =
    detail.history === null ? EMPTY_KINDS : kindsOfTokenInfo(detail.history.totals.tokens);
  const { running } = detail;
  if (
    finished === null ||
    detail.status !== "running" ||
    running === null ||
    !running.tokens_measured
  ) {
    return finished;
  }
  const live = kindsOfTokenInfo(running.tokens);
  return live === null
    ? null
    : {
        input: finished.input + live.input,
        output: finished.output + live.output,
        cache_write: finished.cache_write + live.cache_write,
        cache_read: finished.cache_read + live.cache_read,
      };
}

interface IssueKindLinesProps {
  detail: IssueDetailResponse;
  tokens: BudgetLine & { complete: boolean };
}

function IssueKindLines({ detail, tokens }: IssueKindLinesProps) {
  const kinds = issueKindsOf(detail);
  if (kinds === null || tokens.used === null || tokens.used === 0) {
    return undefined;
  }
  const rate = hitRateOfKinds(kinds);
  return (
    <div className="flex flex-col gap-xxs">
      <div className="grid grid-cols-2 items-center gap-x-sm text-body-md">
        <span className="text-ink">Split</span>
        <SplitBar
          kinds={kinds}
          figureOf={tokensKindFigure(!tokens.complete)}
          footnote={tokens.complete ? undefined : TOKENS_LOWER_BOUND}
        />
      </div>
      {rate === null ? undefined : (
        <FigureLine name={LABEL_CACHE_HIT_RATE}>{fmtPercent(rate)}</FigureLine>
      )}
    </div>
  );
}

interface BudgetPanelProps {
  detail: IssueDetailResponse;
  state: StateResponse | undefined;
  chained: boolean;
}

function BudgetPanel({ detail, state, chained }: BudgetPanelProps) {
  const figures = budgetOf(detail, state);
  const { sessions, tokens, money, cost } = figures;
  const tokensLine = tokensText(tokens);
  const moneyFigure = moneyBudgetFigure(figures);
  const moneyText =
    moneyFigure === undefined || money.limit === null ? undefined : (
      <>
        <NotedFigure figure={moneyFigure} /> of {fmtMoney(money.limit)}
      </>
    );
  const runs = detail.history?.runs ?? [];

  return (
    <Panel isRuled title={cost === null ? "Budget" : "Budget and cost"}>
      {/* Every figure heads itself alike; the figures stand apart by more than a meter and its split. */}
      <div className="flex flex-col gap-md">
        {sessions.limit === null ? (
          <FigureLine name="Sessions">{`${fmtCount(sessions.used ?? 0)}, no budget`}</FigureLine>
        ) : (
          <BudgetMeter name="Sessions" line={sessions} format={fmtCount} />
        )}
        <div className="flex flex-col gap-xxs">
          {tokensLine === undefined ? (
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
          ) : (
            <FigureLine name="Tokens">{tokensLine}</FigureLine>
          )}
          <ShareLine shares={tokenShares(runs, chained)} figureOf={shareTokensText} />
        </div>
        <IssueKindLines detail={detail} tokens={tokens} />
        {cost === null ? undefined : (
          <div className="flex flex-col gap-xxs">
            {moneyText === undefined ? (
              <FigureLine name="Estimated cost">
                <NotedFigure figure={issueCostFigure(detail, cost)} />
              </FigureLine>
            ) : (
              <BudgetMeter name="Cost" line={money} format={fmtMoney} text={moneyText} />
            )}
            <ShareLine
              shares={costShares(runs, chained)}
              figureOf={(share) => (
                <NotedFigure
                  figure={liveCostFigure(
                    shareStatus(share),
                    share.hasPriced ? share.usd : null,
                    "runs",
                  )}
                />
              )}
            />
          </div>
        )}
      </div>
    </Panel>
  );
}

function agentOf(detail: IssueDetailResponse): ReactNode {
  const kind = detail.running?.agent_kind ?? detail.history?.runs?.[0]?.agent_kind;
  return kind ? <NameText name={kind} label={agentLabel(kind)} /> : undefined;
}

function modelOf(detail: IssueDetailResponse): ReactNode {
  const { running } = detail;
  const last = detail.history?.runs?.[0];
  const model =
    running?.model_name || running?.configured_model || last?.model_name || last?.configured_model;
  return model ? <NameText name={model} label={modelLabel(model)} /> : "agent default";
}

// Only an agent that takes a reasoning effort carries one, so an empty value shows no row.
function effortOf(detail: IssueDetailResponse): string | undefined {
  return (
    detail.running?.configured_effort || detail.history?.runs?.[0]?.configured_effort || undefined
  );
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
  const effort = effortOf(detail);
  if (agent === undefined && rule === undefined && position === null) {
    return undefined;
  }
  const k = position === null ? 0 : position.chain.indexOf(position.current) + 1;
  return (
    <Panel isRuled title={rule === undefined && position === null ? "Agent" : "Route"}>
      <Pairs>
        {position === null ? (
          rule === undefined ? undefined : (
            <Pair label="Rule">
              <NameText name={rule} label={ruleLabel(rule)} />
            </Pair>
          )
        ) : (
          <>
            <Pair label="Entry rule">
              <NameText
                name={position.chain[0] ?? position.current}
                label={ruleLabel(position.chain[0] ?? position.current)}
              />
            </Pair>
            <Pair label="Stage">
              <NameText name={position.current} label={ruleLabel(position.current)} />
              {`, ${k} of ${position.chain.length}`}
            </Pair>
          </>
        )}
        {agent === undefined ? undefined : <Pair label="Agent">{agent}</Pair>}
        {agent === undefined ? undefined : <Pair label="Model">{modelOf(detail)}</Pair>}
        {agent === undefined || effort === undefined ? undefined : (
          <Pair label="Effort">{effort}</Pair>
        )}
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
    <Panel isRuled title="Issue">
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
        <Panel isRuled title="What happens next">
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
