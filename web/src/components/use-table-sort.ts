import { useSearchParams } from "react-router";

/** What a column sorts by; `null` is a value Sortie does not know, which sorts last in both directions. */
export type SortValue = number | string | null;

export type SortDirection = "ascending" | "descending";

export interface SortColumn<Row> {
  id: string;
  value: (row: Row) => SortValue;
}

export interface TableSort {
  directionOf: (id: string) => SortDirection | undefined;
  toggle: (id: string) => void;
}

const SEPARATOR = ".";
const DIRECTION_TOKEN: Record<SortDirection, string> = { ascending: "asc", descending: "desc" };

interface ActiveSort {
  id: string;
  direction: SortDirection;
}

function activeSort(value: string | null, ids: readonly string[]): ActiveSort | undefined {
  const at = value?.lastIndexOf(SEPARATOR) ?? -1;
  if (value === null || at < 0) {
    return undefined;
  }
  const id = value.slice(0, at);
  const token = value.slice(at + 1);
  if (!ids.includes(id)) {
    return undefined;
  }
  if (token === DIRECTION_TOKEN.ascending) {
    return { id, direction: "ascending" };
  }
  return token === DIRECTION_TOKEN.descending ? { id, direction: "descending" } : undefined;
}

function compareKnown(a: number | string, b: number | string): number {
  if (typeof a === "number" && typeof b === "number") {
    return a - b;
  }
  return String(a).localeCompare(String(b));
}

function sorted<Row>(
  rows: readonly Row[],
  column: SortColumn<Row>,
  direction: SortDirection,
): Row[] {
  const factor = direction === "ascending" ? 1 : -1;
  return [...rows].sort((a, b) => {
    const x = column.value(a);
    const y = column.value(b);
    if (x === null || y === null) {
      return x === y ? 0 : x === null ? 1 : -1;
    }
    return factor * compareKnown(x, y);
  });
}

/** Keeps the choice in the address under `param`, as `<id>.asc` or `<id>.desc`, so it survives a reload and the screen's other params. The first press on a column sorts ascending, the next descending; until a column is pressed the rows keep their order. */
export function useTableSort<Row>(
  rows: readonly Row[],
  columns: readonly SortColumn<Row>[],
  param: string,
): { rows: readonly Row[]; sort: TableSort } {
  const [params, setParams] = useSearchParams();
  const active = activeSort(
    params.get(param),
    columns.map((column) => column.id),
  );
  const column = columns.find((candidate) => candidate.id === active?.id);

  function directionOf(id: string): SortDirection | undefined {
    return active?.id === id ? active.direction : undefined;
  }

  function toggle(id: string) {
    const direction = directionOf(id) === "ascending" ? "descending" : "ascending";
    setParams(
      (previous) => {
        const next = new URLSearchParams(previous);
        next.set(param, `${id}${SEPARATOR}${DIRECTION_TOKEN[direction]}`);
        return next;
      },
      { replace: true },
    );
  }

  return {
    rows:
      column === undefined || active === undefined ? rows : sorted(rows, column, active.direction),
    sort: { directionOf, toggle },
  };
}
