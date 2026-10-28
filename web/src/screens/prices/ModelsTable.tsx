import { useState } from "react";

import { SearchIcon, XIcon } from "@/components/icons";
import { Hint } from "@/components/ui/hint";
import {
  Table,
  TableBody,
  TableCell,
  SortableHead,
  TableHeader,
  TableRow,
  TableSpanCell,
} from "@/components/ui/table";
import { useTableSort } from "@/components/use-table-sort";
import { fmtCount, fmtRate, fmtStamp } from "@/lib/format";
import { layerLabel, UNSET_RATE, UNSET_RATE_FROM_INPUT } from "@/lib/rates";

import type { ClassRateResponse, ModelPriceResponse } from "@/api/types.gen";
import type { SortColumn } from "@/components/use-table-sort";

const TABLE_LAYERS: readonly string[] = ["operator_table", "fetched", "embedded"];

const COLUMN_COUNT = 9;

const SEARCH_FIELD =
  "h-(--size-control) w-80 max-w-full rounded-lg border border-line bg-field pr-xl pl-[calc(var(--spacing-sm)+var(--size-icon)+var(--spacing-xs))] text-body-md text-ink outline-none transition-colors duration-(--duration-feedback) placeholder:text-ink-muted hover:border-line-medium focus-visible:outline-2 focus-visible:-outline-offset-1 focus-visible:outline-focus-ring";

function rateClasses(row: ModelPriceResponse): ClassRateResponse[] {
  return [row.input, row.output, row.cache_read, row.cache_write];
}

// A kind entry's key is the row's agent and a model entry's key is the row's model, so the
// cell names the layer alone; a model entry adds the table that fills the rates it leaves out.
function pricedByText(row: ModelPriceResponse): string | undefined {
  if (row.layer === null) {
    return undefined;
  }
  const table =
    row.layer === "model_entry"
      ? rateClasses(row).find((rate) => rate.layer !== null && TABLE_LAYERS.includes(rate.layer))
          ?.layer
      : undefined;
  const label = layerLabel(row.layer);
  return table === null || table === undefined ? label : `${label} and ${layerLabel(table)}`;
}

function rateWords(rate: ClassRateResponse): string {
  if (rate.from_input) {
    return UNSET_RATE_FROM_INPUT;
  }
  return rate.per_mtok === null ? UNSET_RATE : fmtRate(rate.per_mtok);
}

interface RateCellProps {
  rate: ClassRateResponse;
  /** Whether the row's classes come from more than one layer, which the hover then names. */
  isMixed: boolean;
}

function RateCell({ rate, isMixed }: RateCellProps) {
  const words = rateWords(rate);
  return (
    <TableCell look="number" isWrapping>
      {isMixed && rate.layer !== null ? <Hint hint={layerLabel(rate.layer)}>{words}</Hint> : words}
    </TableCell>
  );
}

interface ModelRowProps {
  row: ModelPriceResponse;
}

function ModelRow({ row }: ModelRowProps) {
  const layers = new Set(
    rateClasses(row).flatMap((rate) => (rate.layer === null ? [] : [rate.layer])),
  );
  const isMixed = layers.size > 1;
  return (
    <TableRow>
      <TableCell>{row.model === "" ? "Model not reported" : row.model}</TableCell>
      <TableCell>{row.agent_kind}</TableCell>
      <TableCell isWrapping>{pricedByText(row)}</TableCell>
      <RateCell rate={row.input} isMixed={isMixed} />
      <RateCell rate={row.output} isMixed={isMixed} />
      <RateCell rate={row.cache_read} isMixed={isMixed} />
      <RateCell rate={row.cache_write} isMixed={isMixed} />
      <TableCell>{row.last_used_at === null ? undefined : fmtStamp(row.last_used_at)}</TableCell>
      <TableCell look="number">{fmtCount(row.runs)}</TableCell>
    </TableRow>
  );
}

function rateValue(rate: ClassRateResponse): number | null {
  return rate.from_input ? null : rate.per_mtok;
}

const SORT_COLUMNS: readonly SortColumn<ModelPriceResponse>[] = [
  { id: "model", value: (row) => row.model },
  { id: "agent", value: (row) => row.agent_kind },
  { id: "layer", value: (row) => pricedByText(row) ?? null },
  { id: "input", value: (row) => rateValue(row.input) },
  { id: "output", value: (row) => rateValue(row.output) },
  { id: "cache_read", value: (row) => rateValue(row.cache_read) },
  { id: "cache_write", value: (row) => rateValue(row.cache_write) },
  {
    id: "last_used",
    value: (row) => (row.last_used_at === null ? null : Date.parse(row.last_used_at)),
  },
  { id: "runs", value: (row) => row.runs },
];

interface ModelSearchProps {
  query: string;
  onQueryChange: (query: string) => void;
}

function ModelSearch({ query, onQueryChange }: ModelSearchProps) {
  function handleKeyDown(event: React.KeyboardEvent<HTMLInputElement>) {
    if (event.key === "Escape" && !event.nativeEvent.isComposing) {
      onQueryChange("");
    }
  }

  return (
    <label className="relative flex items-center">
      <span className="sr-only">Search models</span>
      <SearchIcon className="pointer-events-none absolute left-sm text-ink-muted" />
      <input
        type="text"
        placeholder="Search models"
        autoComplete="off"
        value={query}
        onChange={(event) => onQueryChange(event.target.value)}
        onKeyDown={handleKeyDown}
        className={SEARCH_FIELD}
      />
      {query === "" ? undefined : (
        <button
          type="button"
          aria-label="Clear search"
          onClick={() => onQueryChange("")}
          className="absolute right-xxs inline-grid size-6 cursor-pointer place-items-center rounded text-ink-muted hover:text-ink focus-visible:outline-2 focus-visible:outline-focus-ring"
        >
          <XIcon />
        </button>
      )}
    </label>
  );
}

/** Props of {@link ModelsTable}. */
export interface ModelsTableProps {
  rows: readonly ModelPriceResponse[];
}

/** Renders the rates the next run of each agent kind and model gets, in the order served until a column is sorted. */
export function ModelsTable({ rows }: ModelsTableProps) {
  const [query, setQuery] = useState("");
  const needle = query.trim().toLowerCase();
  const { rows: ordered, sort } = useTableSort(rows, SORT_COLUMNS, "sort");
  const shown =
    needle === "" ? ordered : ordered.filter((row) => row.model.toLowerCase().includes(needle));

  return (
    <section className="mt-xl flex flex-col gap-sm">
      <div className="flex justify-end">
        <ModelSearch query={query} onQueryChange={setQuery} />
      </div>
      <Table title="Models">
        <TableHeader>
          <SortableHead columnId="model" sort={sort}>
            Model
          </SortableHead>
          <SortableHead columnId="agent" sort={sort}>
            Agent
          </SortableHead>
          <SortableHead columnId="layer" sort={sort}>
            Priced by
          </SortableHead>
          <SortableHead columnId="input" sort={sort} align="right">
            Input
          </SortableHead>
          <SortableHead columnId="output" sort={sort} align="right">
            Output
          </SortableHead>
          <SortableHead columnId="cache_read" sort={sort} align="right">
            Cache read
          </SortableHead>
          <SortableHead columnId="cache_write" sort={sort} align="right">
            Cache write
          </SortableHead>
          <SortableHead columnId="last_used" sort={sort}>
            Last used
          </SortableHead>
          <SortableHead columnId="runs" sort={sort} align="right">
            Runs
          </SortableHead>
        </TableHeader>
        <TableBody>
          {shown.map((row) => (
            <ModelRow key={`${row.agent_kind}\n${row.model}`} row={row} />
          ))}
          {shown.length === 0 && needle !== "" ? (
            <TableRow>
              <TableSpanCell colSpan={COLUMN_COUNT}>
                <span className="text-ink-muted">No model matches this search.</span>
              </TableSpanCell>
            </TableRow>
          ) : undefined}
        </TableBody>
      </Table>
      <p className="text-body-sm text-ink-muted">Prices are per 1 million tokens.</p>
    </section>
  );
}
