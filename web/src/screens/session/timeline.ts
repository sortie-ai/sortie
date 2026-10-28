import { parseInstant } from "@/lib/format";

import type { RunEventResponse, RunningEntryResponse, TokenInfo } from "@/api/types.gen";
import type { RunItem } from "@/stream/core";

export type TimelineLine =
  | { kind: "event"; key: string; event: RunEventResponse }
  | { kind: "tools"; key: string; calls: RunEventResponse[] }
  | {
      kind: "turn";
      key: string;
      event: RunEventResponse;
      /** The 1-based turn number, `null` when an event it counts is missing. */
      turn: number | null;
      /** The turn's own tokens, `null` when not known. */
      tokens: number | null;
      /** Time since the previous turn end or the session start, `null` when not known. */
      seconds: number | null;
    }
  | { kind: "gap"; key: string; missing: number };

export interface Timeline {
  lines: TimelineLine[];
  /** The run's uncached tokens as of the last turn end with nothing missing after it. */
  baseline: number | null;
  /** The number the next turn takes, `null` when not known. */
  nextTurn: number | null;
}

export interface TurnBar {
  turn: number | null;
  at: string | null;
  tokens: number;
  seconds: number | null;
  inProgress: boolean;
}

const TURN_END: ReadonlySet<string> = new Set([
  "turn_completed",
  "turn_failed",
  "turn_cancelled",
  "turn_ended_with_error",
  "turn_input_required",
]);

/** Events carry no served uncached figure, so it is the total less the cache reads. */
function uncachedOf(usage: TokenInfo | null): number | null {
  if (usage === null || usage.total_tokens === null || usage.cache_read_tokens === null) {
    return null;
  }
  return Math.max(0, usage.total_tokens - usage.cache_read_tokens);
}

/** A figure is set only when no event it counts is missing, so nothing is attributed across a gap. */
export function timelineOf(items: readonly RunItem[]): Timeline {
  const lines: TimelineLine[] = [];
  let group: RunEventResponse[] = [];

  const first = items[0];
  const isFromStart = first?.kind === "event" && first.event.seq === 1;
  let baseline: number | null = isFromStart ? 0 : null;
  let turn: number | null = isFromStart ? 0 : null;
  let since: number | null = null;

  const flush = (): void => {
    if (group.length >= 2) {
      lines.push({ kind: "tools", key: `t${group[0].seq}`, calls: group });
    } else if (group.length === 1) {
      lines.push({ kind: "event", key: `e${group[0].seq}`, event: group[0] });
    }
    group = [];
  };

  for (const item of items) {
    if (item.kind === "gap") {
      flush();
      lines.push({ kind: "gap", key: `g${item.fromSeq}`, missing: item.toSeq - item.fromSeq + 1 });
      baseline = null;
      since = null;
      turn = null;
      continue;
    }

    const { event } = item;
    const key = `e${event.seq}`;
    if (event.event === "tool_result" && event.tool_kind !== "skill") {
      group.push(event);
      continue;
    }
    flush();

    if (event.event === "session_started") {
      since = parseInstant(event.at);
      lines.push({ kind: "event", key, event });
    } else if (TURN_END.has(event.event)) {
      const usage = event.usage ?? null;
      const figure = uncachedOf(usage);
      const difference = figure !== null && baseline !== null ? figure - baseline : null;
      const tokens = difference !== null && difference >= 0 ? difference : null;
      baseline = usage === null ? 0 : figure;

      const at = parseInstant(event.at);
      const seconds = since !== null && at !== null ? (at - since) / 1000 : null;
      since = at;

      turn = turn === null ? null : turn + 1;
      lines.push({ kind: "turn", key, event, turn, tokens, seconds });
    } else {
      lines.push({ kind: "event", key, event });
    }
  }
  flush();

  return { lines, baseline, nextTurn: turn === null ? null : turn + 1 };
}

/** Returns one bar per turn whose tokens are known, plus a bar for the turn in progress of a live session whose usage arrives as it happens. */
export function turnBarsOf(timeline: Timeline, running: RunningEntryResponse | null): TurnBar[] {
  const bars: TurnBar[] = [];
  for (const line of timeline.lines) {
    if (line.kind === "turn" && line.tokens !== null) {
      bars.push({
        turn: line.turn,
        at: line.event.at,
        tokens: line.tokens,
        seconds: line.seconds,
        inProgress: false,
      });
    }
  }

  const figure = running?.uncached_tokens ?? null;
  const isMeasuring =
    running !== null &&
    running.tokens_measured &&
    !running.tokens_pending &&
    running.usage_arrival === "incremental";
  if (isMeasuring && figure !== null && timeline.baseline !== null && figure > timeline.baseline) {
    bars.push({
      turn: timeline.nextTurn,
      at: null,
      tokens: figure - timeline.baseline,
      seconds: null,
      inProgress: true,
    });
  }
  return bars;
}
