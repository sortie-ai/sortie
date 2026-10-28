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

const CLASS_WORDS: Readonly<Record<string, string>> = {
  input: "input",
  output: "output",
  cache_read: "cache read",
  cache_write: "cache write",
};

/** Words the token classes a cost has no rate for, joined by commas; a class the client has no wording for reads as served. */
export function unratedText(classes: readonly string[]): string {
  return classes.map((name) => CLASS_WORDS[name] ?? name).join(", ");
}
