import { fmtMoney, fmtMoneyAtLeast } from "@/lib/format";

import type {
  ConfigResponse,
  IssueDetailResponse,
  IssueHistoryResponse,
  StateResponse,
} from "@/api/types.gen";

export interface CostFigure {
  usd: number | null;
  status: string;
}

export interface BudgetLine {
  used: number | null;
  limit: number | null;
  /** False only while the limit would come from a `config` that has not loaded. */
  limitKnown: boolean;
}

export interface BudgetFigures {
  sessions: BudgetLine;
  tokens: BudgetLine & { complete: boolean };
  /** The estimated cost against the money limit; `complete` is false while the figure is a lower bound. */
  money: BudgetLine & { complete: boolean };
  /** The issue's tokens as the peek shows them; null while the spend is not known. */
  uncachedTokens: number | null;
  /** Null while no rate source is available, and when the issue has no recorded run. */
  cost: CostFigure | null;
}

function totalsCost(history: IssueHistoryResponse | null): CostFigure | null {
  return history === null
    ? null
    : { usd: history.totals.estimated_cost_usd, status: history.totals.cost_status };
}

function moneyLine(
  cost: CostFigure | null,
  limit: number | null,
  limitKnown: boolean,
): BudgetFigures["money"] {
  return { used: cost?.usd ?? null, limit, limitKnown, complete: cost?.status === "estimated" };
}

function uncachedTotal(history: IssueHistoryResponse | null): number | null {
  return history === null ? 0 : history.totals.uncached_tokens;
}

function figuresOf(detail: IssueDetailResponse, config: ConfigResponse | undefined): BudgetFigures {
  const { running, budget_exhausted: exhausted, history } = detail;
  if (detail.status === "running" && running !== null) {
    const cost = { usd: running.issue_estimated_cost_usd, status: running.issue_cost_status };
    return {
      sessions: { used: running.used_sessions, limit: running.budget_sessions, limitKnown: true },
      tokens: {
        used: running.used_tokens,
        limit: running.budget_tokens,
        limitKnown: true,
        complete: running.used_tokens_complete,
      },
      money: moneyLine(cost, running.budget_usd, true),
      uncachedTokens: running.used_tokens,
      cost,
    };
  }
  const totals = totalsCost(history);
  if (detail.status === "budget_exhausted" && exhausted !== null) {
    return {
      sessions: {
        used: exhausted.used_sessions,
        limit: exhausted.budget_sessions,
        limitKnown: true,
      },
      tokens: {
        // A hold by the session budget carries no token figure of its own; History's figure keeps the note honest.
        used: exhausted.budget_tokens === null ? uncachedTotal(history) : exhausted.used_tokens,
        limit: exhausted.budget_tokens,
        limitKnown: true,
        complete: exhausted.unmeasured_sessions === 0,
      },
      money: moneyLine(totals, exhausted.budget_usd, true),
      uncachedTokens: uncachedTotal(history),
      cost: totals,
    };
  }
  const isKnown = config !== undefined;
  return {
    sessions: {
      used: history === null ? 0 : history.totals.run_count,
      limit: config?.agent.max_sessions ?? null,
      limitKnown: isKnown,
    },
    tokens: {
      used: uncachedTotal(history),
      limit: config?.agent.max_tokens ?? null,
      limitKnown: isKnown,
      complete: history === null ? true : history.totals.tokens_complete,
    },
    money: moneyLine(totals, config?.agent.max_cost_usd ?? null, isKnown),
    uncachedTokens: uncachedTotal(history),
    cost: totals,
  };
}

/** Reports whether a rate source is available, which the state response says by carrying `cost_unpriced_running`. */
export function hasRateSource(state: StateResponse | undefined): boolean {
  return state?.cost_unpriced_running !== undefined;
}

/** A figure Sortie does not know stays null, never zero. */
export function budgetOf(
  detail: IssueDetailResponse,
  state: StateResponse | undefined,
): BudgetFigures {
  const figures = figuresOf(detail, state?.config);
  return hasRateSource(state) ? figures : { ...figures, cost: null };
}

/** Marked "at least" while the cost is a lower bound; undefined when no cost meter applies. */
export function moneyBudgetText({ cost, money }: BudgetFigures): string | undefined {
  if (cost === null || money.limit === null || money.limit <= 0 || money.used === null) {
    return undefined;
  }
  const used = money.complete ? fmtMoney(money.used) : fmtMoneyAtLeast(money.used);
  return `${used} of ${fmtMoney(money.limit)}`;
}
