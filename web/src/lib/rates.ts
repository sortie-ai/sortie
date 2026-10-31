import { fmtRate } from "@/lib/format";

export const UNSET_RATE = "not set";

/** The wording of a kind entry's cache rate that is left unset and priced at its input rate. */
export const UNSET_RATE_FROM_INPUT = "input rate (not set)";

/** Words a rate per one million tokens, so `0` reads as an amount; `unset` words a rate that is `null`. */
export function rateText(perMtok: number | null, unset: string): string {
  return perMtok === null ? unset : `${fmtRate(perMtok)} per 1M`;
}

const LAYER_LABELS: Readonly<Record<string, string>> = {
  kind_entry: "Kind entry",
  model_entry: "Model entry",
  operator_table: "Custom prices",
  fetched: "Downloaded prices",
  embedded: "Built-in prices",
};

export function layerLabel(layer: string): string {
  return LAYER_LABELS[layer] ?? layer;
}
