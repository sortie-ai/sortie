export type ActiveView = "list" | "pipeline";

const FILTER_PARAMS = ["rule", "phase"];
const ENDED_TODAY = "ended_today";

/** The id of the Active section on Overview. */
export const ACTIVE_SECTION_ID = "active";

/** Carries only the `rule` and `phase` parameters; the list has no ended-today chip, so that phase does not travel. */
export function activeViewPath(target: ActiveView, search: URLSearchParams): string {
  const params = new URLSearchParams();
  for (const name of FILTER_PARAMS) {
    const value = search.get(name);
    if (value !== null && !(target === "list" && name === "phase" && value === ENDED_TODAY)) {
      params.set(name, value);
    }
  }
  const path = target === "list" ? "/" : "/pipeline";
  const text = params.toString();
  const hash = target === "list" ? `#${ACTIVE_SECTION_ID}` : "";
  return `${text === "" ? path : `${path}?${text}`}${hash}`;
}
