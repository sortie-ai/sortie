import { useSearchParams } from "react-router";

import { ChipGroup } from "@/components/ui/chip-group";
import { PHASES, chipCounts, isPhase } from "@/lib/active-rows";
import { plural } from "@/lib/format";

import type { ChipGroupItem } from "@/components/ui/chip-group";
import type { Filterable, Phase } from "@/lib/active-rows";

const PHASE_PARAM = "phase";

const LABELS: Record<Phase, string> = {
  running: "Agent running",
  pull_request: "Waiting on PR",
  between_stages: "Between stages",
  retry: "Retry scheduled",
  needs_attention: "Needs attention",
  ended_today: "Ended today",
};

export interface PhaseFilterProps {
  /** The rows the rule menu already narrowed; the counts and the "All" chip read them. */
  rows: readonly Filterable[];
}

function useLifecycle(rows: readonly Filterable[]) {
  const [params, setParams] = useSearchParams();
  const raw = params.get(PHASE_PARAM);
  const chosen = isPhase(raw) ? raw : "";
  const counts = chipCounts(rows);
  // With fewer than two phases present there is nothing to choose between.
  const present = PHASES.filter((phase) => counts[phase] > 0);
  return { chosen, counts, isShown: present.length >= 2 || chosen !== "", setParams };
}

/** The count of the rows while {@link PhaseFilter} shows no chips, which then count them. */
export function PhaseCount({ rows }: PhaseFilterProps) {
  const { isShown } = useLifecycle(rows);
  return isShown ? undefined : plural(rows.length, "issue", "issues");
}

export function PhaseFilter({ rows }: PhaseFilterProps) {
  const { chosen, counts, isShown, setParams } = useLifecycle(rows);
  if (!isShown) {
    return undefined;
  }

  const items: ChipGroupItem[] = PHASES.filter(
    (phase) => counts[phase] > 0 || phase === chosen,
  ).map((phase) => ({
    value: phase,
    label: `${LABELS[phase]} ${counts[phase]}`,
  }));

  function handleValueChange(value: string) {
    setParams(
      (previous) => {
        const next = new URLSearchParams(previous);
        if (value === "") {
          next.delete(PHASE_PARAM);
        } else {
          next.set(PHASE_PARAM, value);
        }
        return next;
      },
      { replace: true },
    );
  }

  return (
    <ChipGroup
      label="Lifecycle"
      allLabel={`All ${rows.length}`}
      items={items}
      value={chosen}
      onValueChange={handleValueChange}
    />
  );
}
