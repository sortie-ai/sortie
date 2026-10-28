import { useSearchParams } from "react-router";

import { ActiveHead } from "@/components/ActiveHead";
import { PhaseFilter } from "@/components/PhaseFilter";
import { RuleFilter } from "@/components/RuleFilter";
import { Button } from "@/components/ui/button";
import { filterRows } from "@/lib/active-rows";
import { plural } from "@/lib/format";

import { ActiveRow } from "./ActiveRow";
import { activeNavKey } from "./rows";

import type { ConfigResponse } from "@/api/types.gen";
import type { ActiveRow as ActiveRowModel, Phase } from "@/lib/active-rows";

const TALLY: readonly { word: string; phases: readonly Phase[] }[] = [
  { word: "running", phases: ["running"] },
  { word: "waiting", phases: ["pull_request", "between_stages"] },
  { word: "retrying", phases: ["retry"] },
  { word: "held", phases: ["needs_attention"] },
];

function tallyOf(rows: readonly ActiveRowModel[]): string {
  return TALLY.map(({ word, phases }) => ({
    word,
    count: rows.filter((row) => phases.includes(row.phase)).length,
  }))
    .filter(({ count }) => count > 0)
    .map(({ word, count }) => `${count} ${word}`)
    .join(", ");
}

export interface ActiveListProps {
  /** The served `active` rows in served order, before any filter. */
  rows: readonly ActiveRowModel[];
  /** The rows the `phase` and `rule` search parameters leave. */
  visibleRows: readonly ActiveRowModel[];
  config: ConfigResponse | undefined;
  now: number;
  selectedKey: string | undefined;
  showSelection: boolean;
  onOpen: (row: ActiveRowModel) => void;
}

/** The rows keep the order they arrive in. */
export function ActiveList({
  rows,
  visibleRows,
  config,
  now,
  selectedKey,
  showSelection,
  onOpen,
}: ActiveListProps) {
  const [params, setParams] = useSearchParams();
  const ruleRows = filterRows(rows, null, params.get("rule"), config);

  function handleShowAll() {
    setParams(
      (previous) => {
        const next = new URLSearchParams(previous);
        next.delete("phase");
        next.delete("rule");
        return next;
      },
      { replace: true },
    );
  }

  return (
    <section className="flex flex-col gap-sm">
      <ActiveHead
        view="list"
        meta={`${plural(rows.length, "issue", "issues")}, dispatch order`}
        tally={tallyOf(rows)}
      />
      <div className="flex flex-wrap items-center justify-between gap-xs empty:hidden">
        <PhaseFilter rows={ruleRows} />
        <RuleFilter config={config} rows={rows} />
      </div>
      {visibleRows.length > 0 ? (
        <ul className="divide-y divide-line overflow-hidden rounded-lg border border-line bg-canvas">
          {visibleRows.map((row) => (
            <ActiveRow
              key={row.issueId}
              row={row}
              now={now}
              isSelected={selectedKey === activeNavKey(row.issueId)}
              showSelection={showSelection}
              onOpen={onOpen}
            />
          ))}
        </ul>
      ) : rows.length === 0 ? (
        // Holds the list's place so the sections below do not shift when work starts.
        <div aria-hidden="true" className="min-h-lh border border-transparent py-lg text-body-md" />
      ) : (
        <div className="flex flex-wrap items-center gap-sm rounded-lg border border-line bg-canvas px-md py-lg text-body-md text-ink-body">
          No active issue matches these filters.
          <Button onClick={handleShowAll}>Show all</Button>
        </div>
      )}
    </section>
  );
}
