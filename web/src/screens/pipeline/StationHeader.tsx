import { useRef } from "react";

import { STATION_NAME_BOX_PX, stationPlace, stationSpan, stationX } from "@/components/Lane";
import { useWidth } from "@/components/use-width";
import { cn } from "@/lib/cn";
import { fmtCount } from "@/lib/format";
import { stationName } from "@/lib/lane";

import type { Station } from "@/lib/lane";

const NAME_BOX_SHARE = 0.9;
const SPACING_XS_PX = 8;
// Below this a detail truncates to a few letters that run into its neighbor's.
const DETAIL_BOX_MIN_PX = 56;

// Their full labels do not fit a name box at any width.
const ALWAYS_SHORT = new Set(["merge_completion", "label_commands"]);

export interface StationHeaderProps {
  stations: readonly Station[];
  /** The lanes per station key; a station without an entry holds none. */
  tally: ReadonlyMap<string, { count: number; hot: boolean }>;
}

interface StationNameProps {
  station: Station;
  isNarrow: boolean;
}

function StationName({ station, isNarrow }: StationNameProps) {
  if (station.short === undefined) {
    return station.label;
  }
  return isNarrow || ALWAYS_SHORT.has(station.key) ? station.short : station.label;
}

/** Stations are placed with {@link stationX}, as the lane tracks do, so the names stand over the dots. Each circle holds its station's count; the name and detail sit under it, flush with the edge at either end. */
export function StationHeader({ stations, tally }: StationHeaderProps) {
  const count = stations.length;
  const reactions = stations.flatMap((station, index) =>
    station.kind === "reaction" ? [index] : [],
  );
  const first = reactions[0];
  const last = reactions.at(-1);
  // Brackets over the work up to the handoff, over the PR loop the reactions keep, and over the end.
  const phases =
    first === undefined || last === undefined || first === 0
      ? []
      : [
          { label: "Work", from: 0, to: first - 1 },
          { label: "PR loop", from: first, to: last },
          { label: "End", from: count - 1, to: count - 1 },
        ];
  // A bracket reaches a third of the way to the next station, so a third stays open between two.
  const reach = stationSpan(1 / 3, count);
  const shareExpression = `calc(${NAME_BOX_SHARE * 100}cqw / ${Math.max(1, count - 1)} - var(--spacing-xs))`;
  const narrowBelowPx = Math.ceil(
    ((STATION_NAME_BOX_PX + SPACING_XS_PX) * Math.max(1, count - 1)) / NAME_BOX_SHARE,
  );
  // The threshold depends on the station count, so the header measures itself; a container query
  // would need an inline style element the CSP refuses.
  const boxRef = useRef<HTMLDivElement>(null);
  const width = useWidth(boxRef);
  const isNarrow = width <= narrowBelowPx;
  const detailsAboveWidthPx = Math.ceil(
    ((DETAIL_BOX_MIN_PX + SPACING_XS_PX) * Math.max(1, count - 1)) / NAME_BOX_SHARE,
  );
  const hasDetails =
    width > detailsAboveWidthPx && stations.some((station) => station.detail !== undefined);
  const label = `Stations: ${stations
    .map((station) => {
      const total = tally.get(station.key)?.count ?? 0;
      return total > 0 ? `${stationName(station)} ${total}` : stationName(station);
    })
    .join(", ")}`;

  return (
    <div className="sticky top-(--size-header) z-(--z-sticky) border-b border-line bg-canvas px-md pt-lg pb-md max-md:hidden">
      <div
        ref={boxRef}
        role="img"
        aria-label={label}
        className={cn("@container relative", hasDetails ? "h-20" : "h-16")}
      >
        {phases.map((phase) => (
          <div
            key={phase.label}
            className="absolute top-0 h-3 border-x border-t border-line-strong text-center text-label-caps font-medium text-ink-muted uppercase"
            style={{
              // A phase at either end reaches the track's edge, as the words under it do.
              left: phase.from === 0 ? 0 : `calc(${stationX(phase.from, count)} - ${reach})`,
              right:
                phase.to === count - 1 ? 0 : `calc(100% - ${stationX(phase.to, count)} - ${reach})`,
            }}
          >
            <span className="relative -top-xs bg-canvas px-xs">{phase.label}</span>
          </div>
        ))}
        {stations.map((station, index) => {
          const entry = tally.get(station.key);
          const total = entry?.count ?? 0;
          const place = stationPlace(index, count);
          return (
            <div
              key={station.key}
              title={
                station.short === undefined && station.detail === undefined
                  ? undefined
                  : stationName(station)
              }
              className={cn(
                "absolute bottom-0 flex flex-col whitespace-nowrap",
                place.className,
                index === 0 ? "items-start" : index === count - 1 ? "items-end" : "items-center",
              )}
              style={place.style}
            >
              {/* The count is set apart by weight, not hue alone, while one needs attention. Proportional
                  digits center in the circle; a tabular 1 leans left. */}
              <div
                aria-hidden="true"
                className={cn(
                  "mb-xxs grid h-5 min-w-5 place-items-center rounded-full border-[1.5px] bg-canvas px-xxs text-body-sm",
                  entry?.hot === true
                    ? "border-warning-fill font-semibold text-warning"
                    : "border-ink-faint text-ink-body",
                )}
              >
                {total === 0 ? undefined : fmtCount(total)}
              </div>
              <div
                className="truncate text-body-sm font-medium text-ink-body"
                style={{ maxWidth: `min(${STATION_NAME_BOX_PX}px, ${shareExpression})` }}
              >
                <StationName station={station} isNarrow={isNarrow} />
              </div>
              {hasDetails ? (
                <div
                  className="h-4 truncate text-body-sm text-ink-muted"
                  style={{ maxWidth: shareExpression }}
                >
                  {station.detail}
                </div>
              ) : undefined}
            </div>
          );
        })}
      </div>
    </div>
  );
}
