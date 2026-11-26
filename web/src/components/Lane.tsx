import { useRef } from "react";

import { CircleAlertFilledIcon } from "@/components/icons";
import { Panel } from "@/components/Panel";
import { markWord } from "@/components/StatusMark";
import { useWidth } from "@/components/use-width";
import { cn } from "@/lib/cn";
import { plural } from "@/lib/format";
import { reactionCountLabel, ruleLabel } from "@/lib/labels";
import { stationName } from "@/lib/lane";

import type {
  LaneMarker,
  LaneStation,
  StagePosition,
  StageProgress,
  StationState,
} from "@/lib/lane";
import type { CSSProperties } from "react";

// A ring broken at north, east, south, and west marks a station that waits. A mask cuts the gaps from a
// border, so the ring draws as smoothly as the outlined marks beside it and the track never shows inside.
const GAPS =
  "before:absolute before:inset-0 before:rounded-full before:border-current before:[mask:repeating-conic-gradient(from_20deg,#000_0_50deg,transparent_0_90deg)]";
const WAITING_MARK = `size-3.5 bg-(--ground) text-ink-muted before:border-2 ${GAPS}`;

// The alert icon's circle stops short of its box, so the mark fills its ground behind it.
function markGlyph(mark: LaneMarker) {
  return mark === "needs" ? <CircleAlertFilledIcon className="size-4" /> : undefined;
}

const MARKER: Record<LaneMarker, string> = {
  running: "size-3.5 bg-ink-body",
  current: "size-3.5 bg-ink-body",
  waiting: WAITING_MARK,
  approval: WAITING_MARK,
  retry: "size-3.5 border-2 border-ink-muted bg-(--ground)",
  held: WAITING_MARK,
  stopped: WAITING_MARK,
  needs: "size-4 bg-(--ground) text-warning-fill",
  handed: WAITING_MARK,
  succeeded: "size-3.5 bg-ink-body",
  failed: "size-3.5 border-2 border-error-fill bg-(--ground)",
  degraded: "size-3.5 border-2 border-error-fill bg-(--ground)",
  clear: "size-3.5 bg-ink-body",
  ended:
    "size-3.5 border-2 border-ink-body bg-[radial-gradient(circle,var(--color-ink-body)_0_3px,var(--ground)_3px)]",
};

export const WAITING_DOT = `bg-(--ground) text-ink-muted before:border-2 ${GAPS}`;

// A reaction station reads the same filled dot whatever Sortie did there; only a standing
// escalation, which needs a person, takes the attention fill.
const DOT: Record<StationState, string> = {
  passed: "border-[1.5px] border-ink-body bg-ink-body",
  acted: "border-[1.5px] border-ink-body bg-ink-body",
  quiet: "border-[1.5px] border-ink-body bg-ink-body",
  escalated: "border-[1.5px] border-ink-body bg-ink-body",
  needs: "border-[1.5px] border-warning-fill bg-warning-fill",
  current: "border-[1.5px] border-ink-body bg-ink-body",
  waiting: WAITING_DOT,
  approval: WAITING_DOT,
  ahead: "border-[1.5px] border-ink-faint bg-(--ground)",
  idle: "border-[1.5px] border-ink-faint bg-(--ground)",
};

/** The widest box a station's name takes under its station, on the issue page and in the Pipeline's header. */
export const STATION_NAME_BOX_PX = 88;

// The end stations stand in from the track's ends by half the header's station circle (`size-5`).
const TRACK_INSET_PX = 10;

/** How far words under an end station stand in from the track's end: to the outer edge of its `size-3.5` marker. */
const LANE_TEXT_INSET_PX = TRACK_INSET_PX - 7;

/** The length, as a CSS `calc()`, that `steps` gaps between `count` stations take along the track. */
export function stationSpan(steps: number, count: number): string {
  return `calc((100% - ${2 * TRACK_INSET_PX}px) * ${steps} / ${Math.max(1, count - 1)})`;
}

/** Places words under or over the station at `index` of `count`: centered on it, or flush with the end station's mark, `edgePx` in from the track's edge, at either end. */
export function stationPlace(
  index: number,
  count: number,
  edgePx = 0,
): { className: string; style: CSSProperties } {
  if (count > 1 && index === 0) {
    return { className: "text-left", style: { left: edgePx } };
  }
  if (count > 1 && index === count - 1) {
    return { className: "text-right", style: { right: edgePx } };
  }
  return {
    className: "-translate-x-1/2 text-center",
    style: { left: stationX(index, count) },
  };
}

/** The offset from the track's left edge, as a CSS length, of the station at `index` of `count`, on a whole pixel so every circle draws alike. */
export function stationX(index: number, count: number): string {
  return `round(calc(${TRACK_INSET_PX}px + ${stationSpan(index, count)}), 1px)`;
}

function laneMarkerWord(mark: LaneMarker): string {
  return mark === "ended" ? "Ended today" : markWord(mark);
}

const STATE_WORD: Partial<Record<StationState, string>> = {
  acted: "Sortie acted",
  quiet: "nothing to act on",
  needs: "needs attention",
  approval: "waiting on approval",
};

// Sortie keeps working an issue after its handoff, so the track runs on through every
// reaction station it watched in the pass, not only to where the issue stands.
function isWorkedReaction(station: LaneStation): boolean {
  return (
    station.kind === "reaction" &&
    (station.state === "acted" ||
      station.state === "quiet" ||
      station.state === "escalated" ||
      station.state === "needs")
  );
}

function stateWord(state: StationState): string {
  return STATE_WORD[state] ?? state;
}

function countLineOf(station: LaneStation, variant: LaneVariant): string {
  if (station.state !== "acted" || (variant !== "page" && variant !== "lane")) {
    return "";
  }
  return reactionCountLabel(station.key, station.continuations);
}

export interface StageNoteProps {
  position: StagePosition;
  progress: StageProgress;
  /** Whether the issue stands at the agent station; elsewhere the note counts the stages. */
  atAgent: boolean;
  className?: string;
  style?: CSSProperties;
}

/** Says where the issue stands on its stage chain, in the type of the station detail. */
export function StageNote({ position, progress, atAgent, className, style }: StageNoteProps) {
  const { k, n, done } = progress;
  const text = atAgent
    ? `${ruleLabel(position.current)} (${k} of ${n})`
    : done === n
      ? plural(n, "stage", "stages")
      : `${done} of ${n} stages`;
  return (
    <span className={cn("text-body-sm whitespace-nowrap text-ink-muted", className)} style={style}>
      {text}
    </span>
  );
}

/** How much of the lane a {@link Lane} draws: the issue page card, the peek track, a Pipeline lane, or a compressed Pipeline lane. */
export type LaneVariant = "page" | "peek" | "lane" | "dense";

export interface LaneProps {
  stations: LaneStation[];
  position: StagePosition | null;
  progress: StageProgress | null;
  /** Whether a state row decides where the issue stands; a finished issue shows when it ended. */
  live: boolean;
  /** Defaults to `page`, the bordered card with the station names; the other variants draw the bare track. */
  variant?: LaneVariant;
  /** The pull request number the `lane` variant prints under the handoff station once the issue stands there or past it. */
  pullRequestNumber?: number | null;
}

// The air is the gap between the line and the text beside it, over and under. The line starts below the time
// over a marker and ends where the text under it ends, so the track is as tall with or without that text; the
// peek, which has no room to spare, ends at the line unless a stage note stands under it.
const AIR = "[--air:--spacing(3)] [--line:var(--text-body-sm--line-height)]";
const TIME_ROOM = `${AIR} [--mid:calc(var(--line)+var(--air))]`;
const BELOW = "top-[calc(var(--mid)+var(--air))]";
const BELOW_SECOND = "top-[calc(var(--mid)+var(--air)+var(--line))]";

const TRACK_SIZE: Record<LaneVariant, string> = {
  page: `${TIME_ROOM} h-[calc(var(--mid)+var(--air)+2*var(--line))]`,
  lane: `${TIME_ROOM} h-[calc(var(--mid)+var(--air)+var(--line))]`,
  peek: `${TIME_ROOM} h-[calc(var(--mid)+var(--air))]`,
  dense: `${AIR} h-7 [--mid:50%]`,
};

/** The stage note sits under the agent station, which holds all agent work whatever stage the issue is on; positions are shares of the width, so the track never scrolls sideways. */
export function Lane({
  stations,
  position,
  progress,
  live,
  variant = "page",
  pullRequestNumber = null,
}: LaneProps) {
  const count = stations.length;
  const current = stations.findIndex((station) => station.state === "current");
  const standing = current < 0 ? undefined : stations[current];
  const agent = stations.findIndex((station) => station.kind === "agent");
  const handoff = stations.findIndex((station) => station.kind === "handoff");
  const hasStages = position !== null && progress !== null && agent >= 0;
  const hasNote = hasStages && variant !== "dense";
  const hasPullRequest =
    variant === "lane" && pullRequestNumber !== null && handoff >= 0 && current >= handoff;
  const furthest = stations.reduce(
    (last, station, index) => (isWorkedReaction(station) && index > last ? index : last),
    current,
  );
  const reached = furthest < 0 ? "0px" : stationSpan(furthest, count);
  const headWord =
    standing === undefined
      ? undefined
      : !live
        ? `ended ${standing.time ?? ""}`.trim()
        : standing.mark === null
          ? undefined
          : laneMarkerWord(standing.mark);

  // Names stand under every station only while each fits its box; narrower, only the standing station keeps its words.
  const trackRef = useRef<HTMLDivElement>(null);
  const isCrowded = useWidth(trackRef) < (STATION_NAME_BOX_PX + 8) * Math.max(1, count - 1);

  const track = (
    <div
      ref={trackRef}
      aria-hidden="true"
      className={cn(
        "relative",
        TRACK_SIZE[variant],
        variant === "peek" && hasNote && "h-[calc(var(--mid)+var(--air)+var(--line))]",
      )}
    >
      <div
        className="absolute top-(--mid) h-px bg-ink-faint"
        style={{ left: `${TRACK_INSET_PX}px`, right: `${TRACK_INSET_PX}px` }}
      />
      <div
        className="absolute top-[calc(var(--mid)-1px)] h-0.75 rounded-full bg-ink-body"
        style={{ left: `${TRACK_INSET_PX}px`, width: reached }}
      />
      {stations.map((station, index) => {
        const left = stationX(index, count);
        const place = stationPlace(index, count, LANE_TEXT_INSET_PX);
        const isCurrent = index === current;
        const countLine = countLineOf(station, variant);
        return (
          <div key={station.key}>
            {isCurrent ? (
              <CurrentMarker station={station} left={left} place={place} variant={variant} />
            ) : (
              <StationDot station={station} left={left} countLine={countLine} />
            )}
            {variant === "page" ? (
              <span
                className={cn(
                  `absolute ${BELOW} whitespace-nowrap text-body-sm text-ink-body`,
                  place.className,
                  !isCurrent && (isCrowded ? "hidden" : "max-md:hidden"),
                )}
                style={place.style}
              >
                {station.label}
              </span>
            ) : undefined}
            {/* The count and the stage note take the detail's line where they stand. */}
            {variant === "page" &&
            station.detail !== undefined &&
            countLine === "" &&
            !(hasNote && index === agent) ? (
              <span
                className={cn(
                  `absolute ${BELOW_SECOND} text-body-sm whitespace-nowrap text-ink-muted`,
                  place.className,
                  !isCurrent && (isCrowded ? "hidden" : "max-md:hidden"),
                )}
                style={place.style}
              >
                {station.detail}
              </span>
            ) : undefined}
            {countLine === "" ? undefined : (
              <span
                className={cn(
                  "absolute text-body-sm whitespace-nowrap text-ink-muted tabular-nums max-md:hidden",
                  place.className,
                  variant === "page" ? BELOW_SECOND : BELOW,
                )}
                style={place.style}
              >
                {countLine}
              </span>
            )}
          </div>
        );
      })}
      {hasPullRequest ? (
        <span
          className={cn(
            `absolute ${BELOW} -translate-x-1/2 text-body-sm whitespace-nowrap text-ink-muted max-md:hidden`,
          )}
          style={{ left: stationX(handoff, count) }}
        >
          {`#${pullRequestNumber}`}
        </span>
      ) : undefined}
      {hasNote ? (
        <StageNote
          position={position}
          progress={progress}
          atAgent={current === agent}
          className={cn("absolute -translate-x-1/2", variant === "page" ? BELOW_SECOND : BELOW)}
          style={{ left: stationX(agent, count) }}
        />
      ) : undefined}
    </div>
  );

  const list = (
    <ol className="sr-only" aria-label="Stations">
      {stations.map((station) => (
        <li key={station.key}>
          {[
            stationName(station),
            station.state === "idle" ? undefined : stateWord(station.state),
            countLineOf(station, variant) || undefined,
            station.time,
          ]
            .filter((part) => part !== undefined && part !== null)
            .join(", ")}
        </li>
      ))}
      {hasStages ? (
        <li>{`${ruleLabel(position.current)}, ${progress.k} of ${progress.n}`}</li>
      ) : undefined}
    </ol>
  );

  if (variant !== "page") {
    return (
      <div className="min-w-0">
        {list}
        {track}
      </div>
    );
  }
  return (
    <Panel title="Pipeline" meta={headWord}>
      <div className="min-w-0 rounded-xl border border-line bg-canvas p-md">
        {list}
        {track}
      </div>
    </Panel>
  );
}

export interface StandingMarkProps {
  marker: LaneMarker;
  className?: string;
}

/** Draws where an issue stands in the shape its lane draws there, so every list marks an issue alike. */
export function StandingMark({ marker, className }: StandingMarkProps) {
  return (
    <span
      role="img"
      aria-label={laneMarkerWord(marker)}
      title={laneMarkerWord(marker)}
      className={cn(
        "relative inline-grid shrink-0 place-items-center rounded-full",
        MARKER[marker],
        className,
      )}
    >
      {markGlyph(marker)}
    </span>
  );
}

export interface MarkSlotProps {
  marker: LaneMarker | null;
}

/** Holds a row's {@link StandingMark} in a box one title line tall, so the mark centers on the title. */
export function MarkSlot({ marker }: MarkSlotProps) {
  return (
    <span className="flex h-lh w-(--size-icon) shrink-0 items-center justify-center text-body-md">
      {marker === null ? undefined : <StandingMark marker={marker} />}
    </span>
  );
}

interface StationMarkProps {
  station: LaneStation;
  left: string;
}

interface StationDotProps extends StationMarkProps {
  countLine: string;
}

interface CurrentMarkerProps extends StationMarkProps {
  /** Where the time over the marker stands. */
  place: ReturnType<typeof stationPlace>;
  variant: LaneVariant;
}

function CurrentMarker({ station, left, place, variant }: CurrentMarkerProps) {
  const mark = station.mark ?? "current";
  return (
    <>
      <span
        className={cn(
          "absolute top-(--mid) z-(--z-raised) grid -translate-1/2 place-items-center rounded-full shadow-[0_0_0_2px_var(--ground)]",
          MARKER[mark],
        )}
        style={{ left }}
        title={`${stationName(station)}: ${laneMarkerWord(mark)}`}
      >
        {markGlyph(mark)}
      </span>
      {station.time === null || variant === "dense" ? undefined : (
        <span
          className={cn(
            "absolute bottom-[calc(100%-var(--mid)+var(--air))] whitespace-nowrap text-body-sm text-ink-body tabular-nums",
            place.className,
            variant === "lane" && "max-md:hidden",
          )}
          style={place.style}
        >
          {station.time}
        </span>
      )}
    </>
  );
}

function StationDot({ station, left, countLine }: StationDotProps) {
  // A broken ring keeps a gap to the thick line it stands on, as the standing marker does, so it does not melt into it.
  const isWaiting = station.state === "waiting" || station.state === "approval";
  const word = [station.state === "idle" ? "" : stateWord(station.state), countLine]
    .filter((part) => part !== "")
    .join(", ");
  return (
    <span
      className={cn(
        "absolute top-(--mid) size-2.75 -translate-1/2 rounded-full",
        DOT[station.state],
        isWaiting && "shadow-[0_0_0_2px_var(--ground)]",
      )}
      style={{ left }}
      title={
        station.kind === "reaction" && word !== "" ? `${stationName(station)}: ${word}` : undefined
      }
    />
  );
}
