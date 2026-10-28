import {
  awaitsApproval,
  isRowKind,
  routeText,
  rowMark,
  timeBaseOf,
  timeText,
} from "@/lib/active-rows";
import { fmtStamp, parseInstant, upperFirst } from "@/lib/format";
import { reactionLabel, reactionShortLabel } from "@/lib/labels";
import { endingWord, hasSeveralTerminalStates } from "@/lib/ended";
import { runStatus, stateWord } from "@/lib/run-status";

import type { Deciding } from "@/lib/active-rows";
import type {
  ActiveIssueResponse,
  AttentionItemResponse,
  ConfigResponse,
  IssueDetailResponse,
  IssueOutcomeResponse,
  PendingReactionResponse,
  PrLoopStationResponse,
  RunResponse,
  ShapeReactions,
} from "@/api/types.gen";
import type { Mark, StateMark } from "@/lib/run-status";

export type StationKind = "tracker" | "agent" | "handoff" | "reaction" | "end";

/** One place an issue can stand; stations depend on the configuration alone, never on an issue. */
export interface Station {
  key: string;
  kind: StationKind;
  label: string;
  /** The form a header too narrow for the label uses; absent when the label is already short. */
  short?: string;
  /** Set on the end station of a workflow that lists two or more terminal states. */
  labelFromEnding?: true;
}

const AGENT_KEY = "agent";

const REACTION_ORDER = [
  "ci_failure",
  "review_comments",
  "bot_review",
  "merge_conflicts",
  "auto_merge",
  "merge_completion",
  "label_commands",
] as const satisfies readonly (keyof ShapeReactions)[];

/** One agent station holds all agent work; stage names never become stations. */
export function stationsOf(config: ConfigResponse): Station[] {
  const stations: Station[] = [
    { key: "tracker", kind: "tracker", label: "Tracker" },
    { key: AGENT_KEY, kind: "agent", label: "Agent" },
  ];
  const handoff = config.tracker.handoff_state;
  if (handoff !== undefined && handoff !== "") {
    stations.push({ key: "handoff", kind: "handoff", label: upperFirst(handoff) });
  }
  const reactions = REACTION_ORDER.filter((key) => config.reactions[key] !== undefined);
  for (const key of reactions) {
    const short = reactionShortLabel(key);
    stations.push({
      key,
      kind: "reaction",
      label: reactionLabel(key),
      ...(short === undefined ? {} : { short }),
    });
  }
  if (reactions.length > 0) {
    stations.push({
      key: "end",
      kind: "end",
      label: endLabelOf(config),
      ...(hasSeveralTerminalStates(config.tracker.terminal_states)
        ? { labelFromEnding: true as const }
        : {}),
    });
  }
  return stations;
}

// Sortie records a terminal ending without a merge when an issue closes under an open pull
// request, so a terminal state's name is true of the end station only when every merge Sortie
// records moves the issue into it, which `merge_completion` does and `auto_merge` alone does not.
function endLabelOf(config: ConfigResponse): string {
  const terminal = config.tracker.terminal_states ?? [];
  const isEveryMergeTerminal =
    config.reactions.auto_merge === undefined || config.reactions.merge_completion !== undefined;
  return terminal.length === 1 && isEveryMergeTerminal ? upperFirst(terminal[0] ?? "End") : "End";
}

function pendingEntryOf(
  rows: readonly PendingReactionResponse[],
  active: ActiveIssueResponse | undefined,
): PendingReactionResponse | undefined {
  return (
    rows.find((row) => row.kind === active?.reaction_kind) ??
    rows.find((row) => row.escalated_at !== null) ??
    rows.at(0)
  );
}

/** A finished issue, an unknown status, and a status whose row is absent decide nothing. */
export function decidingOf(
  detail: IssueDetailResponse,
  active: ActiveIssueResponse | undefined,
): Deciding | null {
  switch (detail.status) {
    case "running":
      return detail.running === null ? null : { kind: "running", entry: detail.running };
    case "retrying":
      return detail.retry === null ? null : { kind: "retrying", entry: detail.retry };
    case "budget_exhausted":
      return detail.budget_exhausted === null
        ? null
        : { kind: "budget_exhausted", entry: detail.budget_exhausted };
    case "parked":
      return detail.parked === null ? null : { kind: "parked", entry: detail.parked };
    case "between_stages":
      return detail.between_stages === null
        ? null
        : { kind: "between_stages", entry: detail.between_stages };
    case "pending_reactions": {
      const entry = pendingEntryOf(detail.pending_reactions ?? [], active);
      return entry === undefined ? null : { kind: "pending_reactions", entry };
    }
    default:
      return null;
  }
}

export interface StandingInput {
  deciding: Deciding | null;
  outcome: IssueOutcomeResponse | null;
  issueId: string;
  attention: readonly AttentionItemResponse[];
  /** The server clock as epoch milliseconds. */
  now: number;
}

/** What a lane draws at the station it stands on: a status mark, or the ring of an issue that ended today. */
export type LaneMarker = StateMark | "ended";

export interface Standing {
  station: string;
  mark: LaneMarker | null;
  time: string | null;
  /** The name the station takes in this issue's lane only; null keeps the station's own. */
  label: string | null;
}

function reactionKeyOf(deciding: Deciding): string | undefined {
  switch (deciding.kind) {
    case "running":
    case "retrying":
      return deciding.entry.reaction_config_key;
    case "pending_reactions":
      return deciding.entry.config_key;
    default:
      return undefined;
  }
}

type PendingReading = Pick<PendingReactionResponse, "config_key" | "escalated_at" | "last_check">;

// A handled triage closes the work its station stands for, so the row waits on nothing there.
function holdsStation(row: PendingReading): boolean {
  return row.escalated_at !== null || row.last_check?.handled !== true;
}

function decidingStation(deciding: Deciding, stations: readonly Station[]): string {
  const reactionKey = reactionKeyOf(deciding);
  const keyed =
    deciding.kind === "pending_reactions" && !holdsStation(deciding.entry)
      ? undefined
      : stations.find((s) => s.kind === "reaction" && s.key === reactionKey);
  if (keyed !== undefined) {
    return keyed.key;
  }
  if (deciding.kind === "pending_reactions") {
    return stations.find((s) => s.kind === "handoff")?.key ?? AGENT_KEY;
  }
  return AGENT_KEY;
}

function outcomeStation(kind: string, stations: readonly Station[]): Station | undefined {
  const wanted: StationKind | undefined =
    kind === "handed_off"
      ? "handoff"
      : kind === "merged" || kind === "terminal"
        ? "end"
        : undefined;
  return wanted === undefined
    ? undefined
    : (stations.find((s) => s.kind === wanted) ?? stations.at(-1));
}

function endingStationLabel(outcome: IssueOutcomeResponse): string | null {
  if (outcome.kind === "merged") {
    return "Merged";
  }
  return outcome.kind === "terminal" && outcome.state !== undefined && outcome.state !== ""
    ? upperFirst(outcome.state)
    : null;
}

/** Returns `null` when neither exists, so a finished issue without a recorded ending has no marker. The `label` of the standing renames the station for this issue only, and only when the station takes its name from the ending. */
export function standingOf(input: StandingInput, stations: readonly Station[]): Standing | null {
  const { deciding, outcome, issueId, attention, now } = input;
  if (deciding !== null) {
    return {
      station: decidingStation(deciding, stations),
      mark: rowMark(deciding.kind, issueId, attention, awaitsApproval(deciding)),
      time: timeText(deciding.kind, timeBaseOf(deciding), now),
      label: null,
    };
  }
  if (outcome === null) {
    return null;
  }
  const station = outcomeStation(outcome.kind, stations);
  return station === undefined
    ? null
    : {
        station: station.key,
        mark: endingMarker(outcome.kind),
        time: fmtStamp(outcome.observed_at),
        label: station.labelFromEnding === true ? endingStationLabel(outcome) : null,
      };
}

export function endingMarker(kind: string | undefined): LaneMarker | null {
  switch (kind) {
    case "handed_off":
      return "handed";
    case "merged":
    case "terminal":
      return "ended";
    default:
      return null;
  }
}

export function issueStanding(
  issueId: string,
  status: string,
  outcome: IssueOutcomeResponse | null,
  attention: readonly AttentionItemResponse[],
  awaitingApproval: boolean,
): { marker: LaneMarker | null; word: string } {
  if (status === "history") {
    return {
      marker: endingMarker(outcome?.kind),
      word: outcome === null ? "Ended" : endingWord(outcome),
    };
  }
  if (!isRowKind(status)) {
    return { marker: null, word: "" };
  }
  const mark = rowMark(status, issueId, attention, awaitingApproval);
  return { marker: mark, word: stateWord(mark) };
}

export type StationState =
  | "passed"
  | "current"
  | "ahead"
  | "waiting"
  | "approval"
  | "needs"
  | "escalated"
  | "acted"
  | "quiet"
  | "idle";

/** On the current station, `label` is the standing's label when it has one. */
export interface LaneStation extends Station {
  state: StationState;
  mark: LaneMarker | null;
  time: string | null;
  /** The continuations scheduled at an `acted` reaction station; zero elsewhere. */
  continuations: number;
}

type PrLoop = Record<string, PrLoopStationResponse> | null;

type Reading = Pick<LaneStation, "state" | "continuations">;

function holdingStateOf(rows: readonly PendingReading[]): StationState {
  if (rows.some((r) => r.escalated_at !== null)) {
    return "needs";
  }
  return rows.some((r) => r.last_check?.awaiting_approval === true) ? "approval" : "waiting";
}

function reactionReadingOf(
  station: Station,
  pending: readonly PendingReading[],
  prLoop: PrLoop,
): Reading {
  const rows = pending.filter((r) => r.config_key === station.key && holdsStation(r));
  if (rows.length > 0) {
    return { state: holdingStateOf(rows), continuations: 0 };
  }
  const served: PrLoopStationResponse | undefined =
    prLoop !== null && Object.hasOwn(prLoop, station.key) ? prLoop[station.key] : undefined;
  switch (served?.result) {
    case "escalated":
      return { state: "escalated", continuations: 0 };
    case "acted":
      return { state: "acted", continuations: served.continuations };
    case "quiet":
      return { state: "quiet", continuations: 0 };
    default:
      return { state: "idle", continuations: 0 };
  }
}

function readingOf(
  station: Station,
  index: number,
  current: number,
  pending: readonly PendingReading[],
  prLoop: PrLoop,
): Reading {
  if (station.kind === "reaction") {
    return reactionReadingOf(station, pending, prLoop);
  }
  if (current < 0) {
    return { state: "idle", continuations: 0 };
  }
  return { state: index < current ? "passed" : "ahead", continuations: 0 };
}

/** The current station takes the standing's label over its own; a reaction station reads from the pending pull request row while one exists, else from the served `prLoop`. */
export function laneOf(
  stations: readonly Station[],
  standing: Standing | null,
  pending: readonly PendingReading[],
  prLoop: PrLoop,
): LaneStation[] {
  const current = standing === null ? -1 : stations.findIndex((s) => s.key === standing.station);
  return stations.map((station, index) =>
    standing !== null && index === current
      ? {
          ...station,
          label: standing.label ?? station.label,
          state: "current",
          mark: standing.mark,
          time: standing.time,
          continuations: 0,
        }
      : {
          ...station,
          ...readingOf(station, index, current, pending, prLoop),
          mark: null,
          time: null,
        },
  );
}

export interface StagePosition {
  chain: readonly string[];
  current: string;
}

function chainSourceOf(
  detail: IssueDetailResponse,
  deciding: Deciding | null,
): { chain: readonly string[] | null; current: string } | null {
  if (deciding !== null) {
    return deciding.entry.stage_position;
  }
  const last = detail.history?.last_stage;
  if (detail.status !== "history" || last === null || last === undefined) {
    return null;
  }
  return { chain: last.chain, current: last.rule };
}

/** Returns `null` when there is no source, no chain, or the current stage is not on the chain. */
export function positionFrom(
  source: { chain: readonly string[] | null; current: string } | null,
): StagePosition | null {
  if (source === null || source.chain === null || !source.chain.includes(source.current)) {
    return null;
  }
  return { chain: source.chain, current: source.current };
}

/** Reads from the deciding row while one exists, from `history.last_stage` for a finished issue; `null` when the issue has no chain or its current stage is not on it. */
export function positionOf(
  detail: IssueDetailResponse,
  deciding: Deciding | null,
): StagePosition | null {
  return positionFrom(chainSourceOf(detail, deciding));
}

/** Picks the newest finished run of a stage that was not started by a pull request reaction, from runs in any order. */
export function lastStageRunOf(
  runs: readonly RunResponse[],
  current: string,
): RunResponse | undefined {
  return runs
    .filter((run) => run.rule_name === current && !run.reaction_kind)
    .reduce<RunResponse | undefined>(
      (newest, run) =>
        newest === undefined ||
        (parseInstant(run.started_at) ?? 0) > (parseInstant(newest.started_at) ?? 0) ||
        ((parseInstant(run.started_at) ?? 0) === (parseInstant(newest.started_at) ?? 0) &&
          run.run_id > newest.run_id)
          ? run
          : newest,
      undefined,
    );
}

/** Runs are served newest first; `null` when the issue has no stage run, or that run's chain is absent or does not hold its rule. */
export function lastStagePositionOf(runs: readonly RunResponse[]): StagePosition | null {
  const run = runs.find((candidate) => !candidate.reaction_kind && candidate.rule_name);
  return run === undefined
    ? null
    : positionFrom({ chain: run.stage_chain, current: run.rule_name ?? "" });
}

export function runRouteText(run: RunResponse, config: ConfigResponse | undefined): string | null {
  const position = lastStagePositionOf([run]);
  return routeText({ stage_position: position, rule_name: run.rule_name }, config);
}

export type StageFocus =
  | { kind: "current"; mark: Mark | null }
  | { kind: "hop_not_made"; target: string; result: string };

export interface StageProgress {
  /** The 1-based place of the current stage. */
  k: number;
  n: number;
  done: number;
  focus: StageFocus | null;
  remaining: number;
  remainingWord: "ahead" | "not run";
}

function hopNotMade(run: RunResponse | undefined): StageFocus | null {
  if (run === undefined || (run.stage_result !== "failed" && run.stage_result !== "ceiling")) {
    return null;
  }
  return {
    kind: "hop_not_made",
    target: run.stage_target ?? "",
    result: run.stage_result,
  };
}

/** `lastStageRun` is the newest finished non-reaction run of the current stage; a `failed` or `ceiling` result on it means the hop to the next stage was not made. */
export function stageProgressOf(
  position: StagePosition,
  live: boolean,
  station: StationKind | null,
  lastStageRun: RunResponse | undefined,
): StageProgress {
  const k = position.chain.indexOf(position.current) + 1;
  const n = position.chain.length;
  const remaining = n - k;
  const hop = hopNotMade(lastStageRun);

  if (live && station === "agent") {
    return {
      k,
      n,
      done: k - 1,
      focus: { kind: "current", mark: "current" },
      remaining,
      remainingWord: "ahead",
    };
  }
  if (live) {
    return {
      k,
      n,
      done: k,
      focus: hop,
      remaining,
      remainingWord: hop === null ? "ahead" : "not run",
    };
  }
  if (hop !== null || lastStageRun?.status === "succeeded") {
    return { k, n, done: k, focus: hop, remaining, remainingWord: "not run" };
  }
  const mark = lastStageRun === undefined ? null : runStatus(lastStageRun.status).mark;
  return {
    k,
    n,
    done: k - 1,
    focus: { kind: "current", mark },
    remaining,
    remainingWord: "not run",
  };
}
