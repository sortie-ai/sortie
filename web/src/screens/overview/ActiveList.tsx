import { useSearchParams } from "react-router";

import { ACTIVE_SECTION_ID } from "@/components/ActiveHead";
import { Panel } from "@/components/Panel";
import { PhaseCount, PhaseFilter } from "@/components/PhaseFilter";
import { RuleFilter } from "@/components/RuleFilter";
import { Button } from "@/components/ui/button";
import { filterRows } from "@/lib/active-rows";
import { ROW_LIST } from "@/lib/rows";

import { ActiveRow } from "./ActiveRow";
import { activeNavKey } from "./rows";

import type { ConfigResponse } from "@/api/types.gen";
import type { ActiveRow as ActiveRowModel } from "@/lib/active-rows";

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
    <Panel
      id={ACTIVE_SECTION_ID}
      title="Active"
      meta={<PhaseCount rows={ruleRows} />}
      beside={<PhaseFilter rows={ruleRows} />}
      right={<RuleFilter config={config} rows={rows} />}
    >
      {visibleRows.length > 0 ? (
        <ul className={ROW_LIST}>
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
        <div className="flex flex-wrap items-center gap-sm rounded-xl border border-line bg-canvas px-md py-lg text-body-md text-ink-body">
          No active issue matches these filters.
          <Button onClick={handleShowAll}>Show all</Button>
        </div>
      )}
    </Panel>
  );
}
