import { useCallback, useEffect, useRef, useState } from "react";

import { NotedFigure } from "@/components/NotedFigure";
import { SplitBar, cacheHitRate, kindsOfStats } from "@/components/TokenKinds";
import { Button } from "@/components/ui/button";
import {
  SortableHead,
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { useTableSort } from "@/components/use-table-sort";
import {
  CACHE_HIT_RATE_HINT,
  LABEL_CACHE_HIT_RATE,
  TOKENS_LOWER_BOUND,
  statsCostFigure,
  statsCostPerSucceededFigure,
  statsTokensFigure,
} from "@/lib/figures";
import { fmtCount, fmtDuration, fmtMean, fmtPercent, fmtTokens, fmtTokensCut } from "@/lib/format";
import { Panel } from "@/components/Panel";

import type { Duration, Group } from "@/api/types.gen";
import type { SortColumn } from "@/components/use-table-sort";
import type { Figure } from "@/lib/figures";
import type { Units } from "@/lib/units";
import type { ReactNode } from "react";

const PAGE_ROWS = 20;

export interface Paging<Row> {
  visible: readonly Row[];
  hasMore: boolean;
  /** Goes on the table body's `ref`, so focus can move to the first row a page adds. */
  attachBody: (element: HTMLTableSectionElement | null) => void;
  handleShowMore: () => void;
}

/** The shown count lives in state, so mount the table with a key that changes with the range. */
export function usePaging<Row>(rows: readonly Row[]): Paging<Row> {
  const [shown, setShown] = useState(PAGE_ROWS);
  const body = useRef<HTMLTableSectionElement | null>(null);
  const focusRow = useRef<number | null>(null);
  const attachBody = useCallback((element: HTMLTableSectionElement | null) => {
    body.current = element;
  }, []);

  useEffect(() => {
    if (focusRow.current !== null) {
      body.current?.rows[focusRow.current]?.focus();
      focusRow.current = null;
    }
  }, [shown]);

  function handleShowMore() {
    const next = shown + PAGE_ROWS;
    // The button leaves with the last rows, so focus moves to the first row added.
    if (next >= rows.length) {
      focusRow.current = shown;
    }
    setShown(next);
  }

  return {
    visible: rows.slice(0, shown),
    hasMore: shown < rows.length,
    attachBody,
    handleShowMore,
  };
}

export interface ShowMoreProps {
  hasMore: boolean;
  onShowMore: () => void;
}

export function ShowMore({ hasMore, onShowMore }: ShowMoreProps) {
  return hasMore ? (
    <div>
      <Button onClick={onShowMore}>Show more</Button>
    </div>
  ) : undefined;
}

/** Column widths every Analytics table shares, so the columns line up from table to table; Duration takes the rest. A chain table splits the name width into its stage number and stage name, so its Runs column lines up too. */
export const COLUMN_WIDTH = {
  name: "w-nav-rail",
  nameText: "max-w-[208px]",
  stageNumber: "w-xxl",
  stage: "w-[192px]",
  stageText: "max-w-[160px]",
  runs: "w-[96px]",
  measure: "w-[112px]",
  turns: "w-[120px]",
  // Room for a worded figure such as "not measured".
  figure: "w-[128px]",
} as const;

function durationText(duration: Duration): string {
  if (duration.samples === 0) {
    return "unknown";
  }
  const low = fmtDuration(duration.p50);
  const high = fmtDuration(duration.p95);
  return low === high ? low : `${low} to ${high}`;
}

export interface NameColumn<Row> {
  header: string;
  cell: (row: Row) => ReactNode;
  /** The words the cell shows, which the column sorts by. */
  sortText: (row: Row) => string;
}

export interface GroupTableProps<Row extends Group> {
  title: string;
  sortParam: string;
  nameColumns: readonly NameColumn<Row>[];
  rows: readonly Row[];
  tier: string;
  /** Which figure columns the table shows after the turns. */
  units: Units;
  /** `share` swaps the success column for the share of runs, as a breakdown by run status needs. */
  measure?: "success" | "share";
}

function hitRateOf(row: Group, tier: string): number | null {
  return tier === "base" || row.tokens === null
    ? null
    : cacheHitRate(row.tokens.input, row.tokens.cache_read);
}

function sortColumns<Row extends Group>(
  nameColumns: readonly NameColumn<Row>[],
  tier: string,
  measure: "success" | "share",
  units: Units,
): SortColumn<Row>[] {
  const isRecorded = tier !== "base";
  const unitColumns: SortColumn<Row>[] =
    units === "tokens"
      ? [
          {
            id: "tokens",
            value: (row: Row) => (isRecorded && row.tokens !== null ? row.tokens.uncached : null),
          },
          { id: "hit-rate", value: (row: Row) => hitRateOf(row, tier) },
        ]
      : [
          { id: "cost", value: (row: Row) => (isRecorded ? row.cost_usd : null) },
          {
            id: "cost-per-succeeded",
            value: (row: Row) => (isRecorded ? row.cost_per_succeeded_run_usd : null),
          },
        ];
  return [
    ...nameColumns.map((column, index) => ({
      id: `name${index}`,
      value: column.sortText,
    })),
    { id: "runs", value: (row: Row) => row.runs },
    {
      id: measure,
      value: (row: Row) => (measure === "share" ? row.share : row.success_rate),
    },
    {
      id: "duration",
      value: (row: Row) => (row.duration_seconds.samples === 0 ? null : row.duration_seconds.p50),
    },
    { id: "turns", value: (row: Row) => row.mean_turns },
    ...unitColumns,
  ];
}

function splitFigureOf(isLowerBound: boolean): (n: number) => Figure {
  return (n) =>
    isLowerBound ? { text: fmtTokensCut(n), note: TOKENS_LOWER_BOUND } : { text: fmtTokens(n) };
}

interface RowCellProps {
  row: Group;
  tier: string;
}

function SplitCell({ row, tier }: RowCellProps) {
  if (tier === "base" || row.tokens === null || row.tokens.uncached === 0) {
    return <TableCell />;
  }
  return (
    <TableCell>
      <SplitBar
        kinds={kindsOfStats(row.tokens)}
        figureOf={splitFigureOf(row.tokens_lower_bound)}
        footnote={row.tokens_lower_bound ? TOKENS_LOWER_BOUND : undefined}
      />
    </TableCell>
  );
}

function HitRateCell({ row, tier }: RowCellProps) {
  const rate = hitRateOf(row, tier);
  return <TableCell look="number">{rate === null ? undefined : fmtPercent(rate)}</TableCell>;
}

/** The shown count lives in state, so mount the table with a key that changes with the range. */
export function GroupTable<Row extends Group>({
  title,
  sortParam,
  nameColumns,
  rows,
  tier,
  units,
  measure = "success",
}: GroupTableProps<Row>) {
  const { rows: ordered, sort } = useTableSort(
    rows,
    sortColumns(nameColumns, tier, measure, units),
    sortParam,
  );
  const { visible, hasMore, attachBody, handleShowMore } = usePaging(ordered);

  return (
    <Panel title={title}>
      <Table
        className={units === "tokens" ? "table-fixed min-w-274" : "table-fixed min-w-244"}
        frozenColumns={1}
      >
        <TableHeader>
          {nameColumns.map((column, index) => (
            <SortableHead
              key={column.header}
              columnId={`name${index}`}
              sort={sort}
              className={COLUMN_WIDTH.name}
            >
              {column.header}
            </SortableHead>
          ))}
          <SortableHead columnId="runs" sort={sort} align="right" className={COLUMN_WIDTH.runs}>
            Runs
          </SortableHead>
          <SortableHead
            columnId={measure}
            sort={sort}
            align="right"
            className={COLUMN_WIDTH.measure}
          >
            {measure === "share" ? "Share" : "Success"}
          </SortableHead>
          <SortableHead
            columnId="duration"
            sort={sort}
            align="right"
            title="From the typical succeeded run to a slow one."
          >
            Duration
          </SortableHead>
          <SortableHead columnId="turns" sort={sort} align="right" className={COLUMN_WIDTH.turns}>
            Mean turns
          </SortableHead>
          {units === "tokens" ? (
            <>
              <SortableHead
                columnId="tokens"
                sort={sort}
                align="right"
                className={COLUMN_WIDTH.figure}
              >
                Tokens
              </SortableHead>
              <TableHead className={COLUMN_WIDTH.turns}>Split</TableHead>
              <SortableHead
                columnId="hit-rate"
                sort={sort}
                align="right"
                title={CACHE_HIT_RATE_HINT}
                className={COLUMN_WIDTH.figure}
              >
                {LABEL_CACHE_HIT_RATE}
              </SortableHead>
            </>
          ) : (
            <>
              <SortableHead
                columnId="cost"
                sort={sort}
                align="right"
                className={COLUMN_WIDTH.figure}
              >
                Est. cost
              </SortableHead>
              <SortableHead
                columnId="cost-per-succeeded"
                sort={sort}
                align="right"
                title="Estimated cost per succeeded run."
                className={COLUMN_WIDTH.figure}
              >
                Per run
              </SortableHead>
            </>
          )}
        </TableHeader>
        <TableBody ref={attachBody}>
          {visible.map((row, index) => (
            <TableRow key={index} tabIndex={-1}>
              {nameColumns.map((column) => (
                <TableCell key={column.header}>
                  <span
                    title={column.sortText(row)}
                    className={`block truncate ${COLUMN_WIDTH.nameText}`}
                  >
                    {column.cell(row)}
                  </span>
                </TableCell>
              ))}
              <TableCell look="number">{fmtCount(row.runs)}</TableCell>
              <TableCell look="number">
                {fmtPercent(measure === "share" ? row.share : row.success_rate)}
              </TableCell>
              <TableCell look="number">{durationText(row.duration_seconds)}</TableCell>
              <TableCell look="number">
                {row.mean_turns === null ? "not recorded" : fmtMean(row.mean_turns)}
              </TableCell>
              {units === "tokens" ? (
                <>
                  <TableCell look="number">
                    <NotedFigure figure={statsTokensFigure(row, tier)} />
                  </TableCell>
                  <SplitCell row={row} tier={tier} />
                  <HitRateCell row={row} tier={tier} />
                </>
              ) : (
                <>
                  <TableCell look="number">
                    <NotedFigure figure={statsCostFigure(row, tier)} />
                  </TableCell>
                  <TableCell look="number">
                    <NotedFigure figure={statsCostPerSucceededFigure(row, tier)} />
                  </TableCell>
                </>
              )}
            </TableRow>
          ))}
        </TableBody>
      </Table>
      <ShowMore hasMore={hasMore} onShowMore={handleShowMore} />
    </Panel>
  );
}
