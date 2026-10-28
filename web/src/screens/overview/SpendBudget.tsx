import { BudgetMeter } from "@/components/BudgetMeter";
import { costTitle } from "@/lib/figures";
import { fmtMoney, fmtMoneyAtLeast, fmtUtcStamp } from "@/lib/format";

import type { SpendCeilingResponse, SpendWindowResponse } from "@/api/types.gen";

interface SpendMeterProps {
  name: string;
  window: SpendWindowResponse;
}

function SpendMeter({ name, window }: SpendMeterProps) {
  const { used_usd: used, budget_usd: limit, resets_at: resetsAt } = window;
  const isComplete = window.used_usd_complete;
  const text = `${isComplete ? fmtMoney(used) : fmtMoneyAtLeast(used)} of ${fmtMoney(limit)}`;
  return (
    <div className="flex flex-col gap-xxs">
      <BudgetMeter
        name={name}
        line={{ used, limit, limitKnown: true }}
        format={fmtMoney}
        text={text}
        title={costTitle(isComplete ? "estimated" : "partial", used)}
      />
      <p className="text-body-sm text-ink-muted">
        {window.holds_dispatch ? "Holding dispatch, resets " : "Resets "}
        {fmtUtcStamp(resetsAt)}
      </p>
    </div>
  );
}

// Record forces a label for every window the API serves, so a new member fails tsc here.
// Key order is the render order.
const SPEND_LABELS: Record<keyof SpendCeilingResponse, string> = {
  daily: "Daily spend",
  weekly: "Weekly spend",
  monthly: "Monthly spend",
};

// Object.keys widens to string[]; the keys are exactly those of SPEND_LABELS.
const SPEND_WINDOWS = Object.keys(SPEND_LABELS) as (keyof SpendCeilingResponse)[];

export function spendWindows(
  ceiling: SpendCeilingResponse | undefined,
): (keyof SpendCeilingResponse)[] {
  return ceiling === undefined ? [] : SPEND_WINDOWS.filter((key) => ceiling[key] !== null);
}

export interface SpendBudgetProps {
  ceiling: SpendCeilingResponse;
  /** Whether to say the figures are estimated at API prices, which matters once an agent reports a plan's limits. */
  isEstimateNoted: boolean;
}

export function SpendBudget({ ceiling, isEstimateNoted }: SpendBudgetProps) {
  return (
    <div className="flex flex-col gap-sm rounded-lg border border-line bg-canvas p-md">
      <div className="grid grid-cols-[minmax(0,1fr)_auto] gap-x-sm text-body-md">
        <h3 className="font-medium text-ink">Budget</h3>
        {isEstimateNoted ? (
          <span className="text-body-sm text-ink-muted">Estimated at API prices</span>
        ) : undefined}
      </div>
      {spendWindows(ceiling).map((key) => {
        const window = ceiling[key];
        return window === null ? undefined : (
          <SpendMeter key={key} name={SPEND_LABELS[key]} window={window} />
        );
      })}
    </div>
  );
}
