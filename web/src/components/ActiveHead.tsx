import { Link, useSearchParams } from "react-router";

import { cn } from "@/lib/cn";

export type ActiveView = "list" | "pipeline";

export interface ActiveHeadProps {
  view: ActiveView;
  meta: string;
  tally?: string;
}

const VIEWS: readonly { view: ActiveView; label: string }[] = [
  { view: "list", label: "List" },
  { view: "pipeline", label: "Pipeline" },
];

const FILTER_PARAMS = ["rule", "phase"];
const ENDED_TODAY = "ended_today";

/** Carries only the `rule` and `phase` parameters of `search`; the list has no ended-today chip, so `phase=ended_today` does not travel to it. */
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
  return text === "" ? path : `${path}?${text}`;
}

export function ActiveHead({ view, meta, tally }: ActiveHeadProps) {
  const [search] = useSearchParams();

  return (
    <div className="flex flex-wrap items-baseline gap-x-sm">
      <h2 className="font-heading text-h7 font-semibold text-ink">Active</h2>
      <span className="text-body-md text-ink-muted">{meta}</span>
      <span className="ml-auto flex items-center gap-md self-center">
        {tally ? (
          <span className="text-body-md text-ink-muted max-md:hidden">{tally}</span>
        ) : undefined}
        <span className="inline-flex gap-xxs rounded-xl bg-surface-sunken p-xxs">
          {VIEWS.map((option) => (
            <Link
              key={option.view}
              to={activeViewPath(option.view, search)}
              aria-current={option.view === view ? "page" : undefined}
              className={cn(
                "inline-flex h-6 items-center rounded-lg px-sm text-body-md font-medium transition-colors duration-(--duration-feedback) focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-focus-ring",
                option.view === view
                  ? "bg-canvas text-ink"
                  : "text-ink-body hover:bg-state-hover hover:text-ink active:bg-state-pressed",
              )}
            >
              {option.label}
            </Link>
          ))}
        </span>
      </span>
    </div>
  );
}
