import { Hint } from "@/components/ui/hint";
import {
  SortableHead,
  Table,
  TableBody,
  TableCell,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { useTableSort } from "@/components/use-table-sort";
import { fmtCount } from "@/lib/format";

import { COLUMN_WIDTH, ShowMore, usePaging } from "./GroupTable";

import type { SkillCallReport, SkillCount } from "@/api/types.gen";
import type { SortColumn } from "@/components/use-table-sort";
import type { ReactNode } from "react";

const LOWER_BOUND =
  "At least this many. It leaves out runs Sortie recorded before it kept skill calls.";

const SORT_COLUMNS: readonly SortColumn<SkillCount>[] = [
  { id: "name", value: (row) => row.skill },
  { id: "runs", value: (row) => row.runs },
  { id: "calls", value: (row) => row.calls },
  { id: "failed_calls", value: (row) => row.failed_calls },
];

interface BoundedProps {
  hint: string | undefined;
  children: ReactNode;
}

// A count that may fall short is underlined; an exact one stays plain.
function Bounded({ hint, children }: BoundedProps) {
  return hint === undefined ? children : <Hint hint={hint}>{children}</Hint>;
}

/** Props of {@link SkillTable}. */
export interface SkillTableProps {
  report: SkillCallReport;
}

/**
 * Renders the skill calls of a report: per skill, the runs that called it, its
 * calls, and its failed calls, 20 rows at a time, sortable by any column. Mount
 * it with a key that changes with the range.
 */
export function SkillTable({ report }: SkillTableProps) {
  const rows = report.by_skill ?? [];
  const hint = report.unrecorded_runs > 0 ? LOWER_BOUND : undefined;
  const { rows: ordered, sort } = useTableSort(rows, SORT_COLUMNS, "by-skill");
  const { visible, hasMore, attachBody, handleShowMore } = usePaging(ordered);

  return (
    <section className="flex flex-col gap-sm">
      <Table title="By skill" className="table-fixed min-w-208" frozenColumns={1}>
        <TableHeader>
          <SortableHead columnId="name" sort={sort} className={COLUMN_WIDTH.name}>
            Skill
          </SortableHead>
          <SortableHead
            columnId="runs"
            sort={sort}
            title="A run counts once however often it called the skill."
            className={COLUMN_WIDTH.runs}
          >
            Runs
          </SortableHead>
          <SortableHead columnId="calls" sort={sort} className={COLUMN_WIDTH.measure}>
            Calls
          </SortableHead>
          <SortableHead columnId="failed_calls" sort={sort}>
            Failed calls
          </SortableHead>
        </TableHeader>
        <TableBody ref={attachBody}>
          {visible.map((row) => (
            <TableRow key={row.skill} tabIndex={-1}>
              <TableCell>
                <span title={row.skill} className={`block truncate ${COLUMN_WIDTH.nameText}`}>
                  {row.skill}
                </span>
              </TableCell>
              <TableCell>
                <Bounded hint={hint}>{fmtCount(row.runs)}</Bounded>
              </TableCell>
              <TableCell>
                <Bounded hint={hint}>{fmtCount(row.calls)}</Bounded>
              </TableCell>
              <TableCell look={row.failed_calls === 0 ? "muted" : "text"}>
                <Bounded hint={hint}>{fmtCount(row.failed_calls)}</Bounded>
              </TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>
      <ShowMore hasMore={hasMore} onShowMore={handleShowMore} />
    </section>
  );
}
