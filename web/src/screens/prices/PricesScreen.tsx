import { ApiError } from "@/api/client";
import { usePricesQuery } from "@/api/queries";
import { ScreenHeader } from "@/components/ScreenHeader";
import { fmtStamp } from "@/lib/format";
import { layerLabel } from "@/lib/rates";

import { ModelsTable } from "./ModelsTable";
import {
  DownloadedTablePanel,
  EmbeddedTablePanel,
  OperatorTablePanel,
  PricesInForcePanel,
  TokenRatesPanel,
} from "./SourcePanels";

import type { TableInForceResponse } from "@/api/types.gen";

const MESSAGE = "text-body-md text-ink-body";

function standfirstOf(table: TableInForceResponse | null): string {
  return table === null
    ? "No price table is in force."
    : `${layerLabel(table.layer)} as of ${fmtStamp(table.as_of)}.`;
}

function isUnavailable(error: unknown): boolean {
  return error instanceof ApiError && error.status === 503 && error.code === "history_unavailable";
}

export function PricesScreen() {
  const prices = usePricesQuery();
  const { data } = prices;

  return (
    <div className="flex flex-col">
      <ScreenHeader
        title="Prices"
        standfirst={data === undefined ? undefined : standfirstOf(data.table_in_force)}
      />
      {data === undefined ? (
        prices.isError ? (
          <p className={MESSAGE}>
            {isUnavailable(prices.error)
              ? "Run history is not available."
              : "Prices could not be read."}
          </p>
        ) : (
          <p className={MESSAGE}>Loading</p>
        )
      ) : (
        <div className="flex flex-col gap-xl">
          <div className="grid grid-cols-1 items-start gap-x-xxxl gap-y-xl min-[1024px]:grid-cols-2">
            <PricesInForcePanel table={data.table_in_force} />
            <TokenRatesPanel source={data.token_rates} />
            {data.operator === null ? undefined : <OperatorTablePanel source={data.operator} />}
            <DownloadedTablePanel source={data.fetched} />
            <EmbeddedTablePanel source={data.embedded} />
          </div>
          <ModelsTable rows={data.models ?? []} />
        </div>
      )}
    </div>
  );
}
