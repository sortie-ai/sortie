import { useSearchParams } from "react-router";

import { ChipGroup } from "@/components/ui/chip-group";
import { PHASES, chipCounts, isPhase } from "@/lib/active-rows";

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

export function PhaseFilter({ rows }: PhaseFilterProps) {
  const [params, setParams] = useSearchParams();
  const raw = params.get(PHASE_PARAM);
  const chosen = isPhase(raw) ? raw : "";
  const counts = chipCounts(rows);
  const present = PHASES.filter((phase) => counts[phase] > 0);

  if (present.length < 2 && chosen === "") {
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
