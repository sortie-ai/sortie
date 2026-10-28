import { stationX } from "@/components/Lane";
import { cn } from "@/lib/cn";
import { fmtCount } from "@/lib/format";

import type { Station } from "@/lib/lane";

const FULL_NAME_BOX_PX = 88;
const NAME_BOX_SHARE = 0.9;
const SPACING_XS_PX = 8;
const LOOP_MARGIN = 3;

// Their full labels do not fit a name box at any width.
const ALWAYS_SHORT = new Set(["merge_completion", "label_commands"]);

export interface StationHeaderProps {
  stations: readonly Station[];
  /** The lanes per station key; a station without an entry holds none. */
  tally: ReadonlyMap<string, { count: number; hot: boolean }>;
}

interface StationNameProps {
  station: Station;
}

function StationName({ station }: StationNameProps) {
  if (station.short === undefined) {
    return station.label;
  }
  if (ALWAYS_SHORT.has(station.key)) {
    return station.short;
  }
  return (
    <>
      <span data-station-full="">{station.label}</span>
      <span data-station-short="" hidden>
        {station.short}
      </span>
    </>
  );
}

/** Stations are placed with {@link stationX}, as the lane tracks do, so the names stand over the dots; the count sits under the name so a count of any length fits. */
export function StationHeader({ stations, tally }: StationHeaderProps) {
  const count = stations.length;
  const reactions = stations.flatMap((station, index) =>
    station.kind === "reaction" ? [index] : [],
  );
  const first = reactions[0];
  const last = reactions.at(-1);
  const shareExpression = `calc(${NAME_BOX_SHARE * 100}cqw / ${Math.max(1, count - 1)} - var(--spacing-xs))`;
  const narrowBelowPx = Math.ceil(
    ((FULL_NAME_BOX_PX + SPACING_XS_PX) * Math.max(1, count - 1)) / NAME_BOX_SHARE,
  );
  const label = `Stations: ${stations
    .map((station) => {
      const total = tally.get(station.key)?.count ?? 0;
      return total > 0 ? `${station.label} ${total}` : station.label;
    })
    .join(", ")}`;

  return (
    <div className="sticky top-(--size-header) z-(--z-sticky) border-b border-line bg-canvas px-md pt-sm pb-xs max-md:hidden">
      {/* The query threshold depends on the station count, which a static class cannot carry. */}
      <style>{`@container (max-width: ${narrowBelowPx}px) { [data-station-full] { display: none } [data-station-short] { display: inline } }`}</style>
      <div role="img" aria-label={label} className="@container relative h-20">
        {first === undefined || last === undefined ? undefined : (
          <div
            className="absolute top-0 h-3 border-x border-t border-line-strong text-center text-label-caps font-medium text-ink-muted uppercase"
            style={{
              left: `${stationX(first, count) - LOOP_MARGIN}%`,
              width: `${stationX(last, count) - stationX(first, count) + 2 * LOOP_MARGIN}%`,
            }}
          >
            <span className="relative -top-xs bg-canvas px-xs">PR loop</span>
          </div>
        )}
        {stations.map((station, index) => {
          const entry = tally.get(station.key);
          const total = entry?.count ?? 0;
          return (
            <div
              key={station.key}
              title={station.short === undefined ? undefined : station.label}
              className="absolute bottom-0 -translate-x-1/2 text-center whitespace-nowrap"
              style={{ left: `${stationX(index, count)}%` }}
            >
              <div
                aria-hidden="true"
                className="mx-auto mb-xxs size-5 rounded-full border-[1.5px] border-ink-faint bg-canvas"
              />
              <div
                className="mx-auto truncate text-body-sm font-medium text-ink-body"
                style={{ maxWidth: `min(${FULL_NAME_BOX_PX}px, ${shareExpression})` }}
              >
                <StationName station={station} />
              </div>
              <div
                className={cn(
                  "h-5 text-body-sm tabular-nums",
                  entry?.hot === true ? "font-medium text-warning" : "text-ink-muted",
                )}
              >
                {total === 0 ? undefined : fmtCount(total)}
              </div>
            </div>
          );
        })}
      </div>
    </div>
  );
}
