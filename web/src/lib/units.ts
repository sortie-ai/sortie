import { useSearchParams } from "react-router";

import { hasRateSource } from "@/lib/budget";

import type { StateResponse } from "@/api/types.gen";
import type { To } from "react-router";

/** What the History and Analytics screens count: token use or estimated cost. */
export type Units = "tokens" | "cost";

export interface UnitsView {
  /** The pressed option. */
  chosen: Units;
  /** What the screen draws; tokens whenever the switch is not offered. */
  shown: Units;
  isOffered: boolean;
  choose: (units: Units) => void;
}

const PARAM = "units";
const STORAGE_KEY = "sortie.units";
const DEFAULT_UNITS: Units = "cost";

function unitsOf(value: string | null): Units | undefined {
  return value === "tokens" || value === "cost" ? value : undefined;
}

function readKept(): Units | undefined {
  try {
    return unitsOf(localStorage.getItem(STORAGE_KEY));
  } catch {
    return undefined;
  }
}

/**
 * Returns the units choice: the address wins over the kept choice, which wins over the default.
 * A state that has not arrived yet counts as having a rate source, so the switch does not flicker in.
 * Only a press of the switch writes the kept choice.
 */
export function useUnits(state: StateResponse | undefined, hasReport = true): UnitsView {
  const [params, setParams] = useSearchParams();
  const chosen = unitsOf(params.get(PARAM)) ?? readKept() ?? DEFAULT_UNITS;
  const isOffered = hasReport && (state === undefined || hasRateSource(state));

  function choose(units: Units) {
    if (units === chosen) {
      return;
    }
    try {
      localStorage.setItem(STORAGE_KEY, units);
    } catch {
      // The choice still holds for this page through the address when storage throws.
    }
    setParams(
      (previous) => {
        const next = new URLSearchParams(previous);
        next.set(PARAM, units);
        return next;
      },
      { replace: true },
    );
  }

  return { chosen, shown: isOffered ? chosen : "tokens", isOffered, choose };
}

interface SplitTarget {
  path: string;
  query: string;
  hash: string;
}

function splitTarget(value: string): SplitTarget {
  const hashAt = value.indexOf("#");
  const beforeHash = hashAt < 0 ? value : value.slice(0, hashAt);
  const queryAt = beforeHash.indexOf("?");
  return {
    path: queryAt < 0 ? beforeHash : beforeHash.slice(0, queryAt),
    query: queryAt < 0 ? "" : beforeHash.slice(queryAt + 1),
    hash: hashAt < 0 ? "" : value.slice(hashAt),
  };
}

function withUnitsQuery(query: string, carried: string): string {
  if (new URLSearchParams(query).has(PARAM)) {
    return query;
  }
  const added = new URLSearchParams({ [PARAM]: carried }).toString();
  return query === "" ? added : `${query}&${added}`;
}

/** Returns a link target that keeps the address's `units` when the target names none of its own. */
export function withCarriedUnits(to: To, search: string): To {
  const carried = new URLSearchParams(search).get(PARAM);
  if (carried === null) {
    return to;
  }
  if (typeof to === "string") {
    const { path, query, hash } = splitTarget(to);
    return `${path}?${withUnitsQuery(query, carried)}${hash}`;
  }
  const query = (to.search ?? "").replace(/^\?/, "");
  return { ...to, search: `?${withUnitsQuery(query, carried)}` };
}
