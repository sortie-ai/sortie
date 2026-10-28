import { useSearchParams } from "react-router";

import type { AnalyticsRange } from "@/api/queries";

const RANGE_PARAM = "range";
const DEFAULT_RANGE: AnalyticsRange = "7d";

export const RANGE_OPTIONS: readonly { value: AnalyticsRange; label: string }[] = [
  { value: "24h", label: "Last 24 hours" },
  { value: "7d", label: "Last 7 days" },
  { value: "30d", label: "Last 30 days" },
  { value: "90d", label: "Last 90 days" },
  { value: "all", label: "All time" },
];

/** Returns the range a URL parameter names, and the default range for any other value. */
export function rangeOf(value: string | null): AnalyticsRange {
  return RANGE_OPTIONS.find((option) => option.value === value)?.value ?? DEFAULT_RANGE;
}

export function useAnalyticsRange(): [AnalyticsRange, (range: AnalyticsRange) => void] {
  const [params, setParams] = useSearchParams();

  function setRange(range: AnalyticsRange) {
    setParams(
      (previous) => {
        const next = new URLSearchParams(previous);
        next.set(RANGE_PARAM, range);
        return next;
      },
      { replace: true },
    );
  }

  return [rangeOf(params.get(RANGE_PARAM)), setRange];
}
