import { useMemo } from "react";
import { useSearchParams } from "react-router";

import { ApiError } from "@/api/client";
import { useHistoryQuery, useStateQuery } from "@/api/queries";
import { Button } from "@/components/ui/button";
import { Table } from "@/components/ui/table";

import { filterFromParams, HistoryFilters } from "./HistoryFilters";
import { HistoryTableHead, IssueGroupRows } from "./IssueGroupRows";

import type { HistoryFilter } from "@/api/client";

const FILTER_PARAMS = ["issue", "rule", "status", "stage"];
const MESSAGE = "rounded-lg border border-line bg-canvas px-md py-lg text-body-md text-ink-body";

function isUnavailable(error: unknown): boolean {
  return error instanceof ApiError && error.status === 503 && error.code === "history_unavailable";
}

function isFiltered(filter: HistoryFilter): boolean {
  return Object.keys(filter).length > 0;
}

export function HistoryScreen() {
  const { data: state } = useStateQuery();
  const config = state?.config;
  const [params, setParams] = useSearchParams();
  const filter = useMemo(() => filterFromParams(params), [params]);
  const history = useHistoryQuery(filter);

  const groups = history.data?.pages.flatMap((page) => page.groups ?? []);
  const filterKey = JSON.stringify(filter);

  function handleClearFilters() {
    setParams(
      (previous) => {
        const next = new URLSearchParams(previous);
        for (const name of FILTER_PARAMS) {
          next.delete(name);
        }
        return next;
      },
      { replace: true },
    );
  }

  return (
    <div className="flex flex-col gap-lg">
      <h1 className="font-heading text-h4 font-semibold text-ink">History</h1>
      <HistoryFilters config={config} />
      {groups === undefined ? (
        history.isError ? (
          <p className={MESSAGE}>
            {isUnavailable(history.error)
              ? "Run history is not available."
              : "Run history could not be read."}
          </p>
        ) : undefined
      ) : groups.length === 0 ? (
        <div className={`${MESSAGE} flex flex-wrap items-center gap-sm`}>
          {isFiltered(filter) ? (
            <>
              <span>No finished run matches these filters.</span>
              <Button onClick={handleClearFilters}>Clear filters</Button>
            </>
          ) : (
            <span>No run has finished yet.</span>
          )}
        </div>
      ) : (
        <>
          <Table isWide frozenColumns={1}>
            <HistoryTableHead state={state} />
            {groups.map((group) => (
              <IssueGroupRows
                key={`${filterKey}|${group.issue_id}`}
                group={group}
                filter={filter}
                state={state}
              />
            ))}
          </Table>
          {history.hasNextPage ? (
            <div className="flex items-center gap-sm">
              <Button
                onClick={() => void history.fetchNextPage()}
                disabled={history.isFetchingNextPage}
              >
                Show more issues
              </Button>
              {history.isFetchNextPageError ? (
                <span className="text-body-sm text-error">Run history could not be read.</span>
              ) : undefined}
            </div>
          ) : undefined}
        </>
      )}
    </div>
  );
}
