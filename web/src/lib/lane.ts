import {
  awaitsApproval,
  isRowKind,
  routeText,
  rowMark,
  timeBaseOf,
  timeText,
} from "@/lib/active-rows";
import { fmtCount, fmtStamp, parseInstant, plural } from "@/lib/format";
import { reactionLabel, reactionShortLabel, reactionWatchLabel } from "@/lib/labels";
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
  /** The station's role, the same in every workflow. */
  label: string;
  /**
   * What the station stands for in this workflow: the tracker state it configures, as configured,
   * or what a reaction watches; absent when none applies.
   */
  detail?: string;
  /** The form a header too narrow for the label uses; absent when the label is already short. */
  short?: string;
  /** Set on the end station of a workflow that lists two or more terminal states. */
  detailFromEnding?: true;
}

const AGENT_KEY = "agent";

/** Words a station with its detail, as a title or a screen reader reads it. */
export function stationName(station: Pick<Station, "label" | "detail">): string {
  return station.detail === undefined ? station.label : `${station.label} (${station.detail})`;
}

/** Places a reaction as its station stands on the pipeline; a key outside the order comes after the others. */
export function reactionRank(configKey: string): number {
  const rank = (REACTION_ORDER as readonly string[]).indexOf(configKey);
  return rank === -1 ? REACTION_ORDER.length : rank;
}

const REACTION_ORDER = [
  "ci_failure",
  "bot_review",
  "review_comments",
  "merge_conflicts",
  "auto_merge",
  "merge_completion",
  "label_commands",
] as const satisfies readonly (keyof ShapeReactions)[];

/** One agent station holds all agent work; stage names never become stations. Each station is named by its role and carries the state or watch it stands for as its detail. */
export function stationsOf(config: ConfigResponse): Station[] {
  const { in_progress_state: inProgress } = config.tracker;
  const queued = (config.tracker.active_states ?? []).find(
    (state) => state !== "" && state !== inProgress,
  );
  const stations: Station[] = [
    { key: "tracker", kind: "tracker", label: "Tracker", ...detailOf(queued) },
    { key: AGENT_KEY, kind: "agent", label: "Agent", ...detailOf(inProgress) },
  ];
  const handoff = config.tracker.handoff_state;
  if (handoff !== undefined && handoff !== "") {
    stations.push({ key: "handoff", kind: "handoff", label: "Handoff", detail: handoff });
  }
  const reactions = REACTION_ORDER.filter((key) => config.reactions[key] !== undefined);
  for (const key of reactions) {
    const short = reactionShortLabel(key);
    stations.push({
      key,
      kind: "reaction",
      label: reactionLabel(key),
      ...(short === undefined ? {} : { short }),
      ...detailOf(
        key === "merge_completion"
          ? config.reactions.merge_completion?.target_state
          : reactionWatchLabel(key),
      ),
    });
  }
  if (reactions.length > 0) {
    const detail = endDetailOf(config);
    stations.push({
      key: "end",
      kind: "end",
      label: "Terminal",
      ...(detail === undefined ? {} : { detail }),
      ...(hasSeveralTerminalStates(config.tracker.terminal_states)
        ? { detailFromEnding: true as const }
        : {}),
    });
  }
  return stations;
}

function detailOf(detail: string | undefined): Pick<Station, "detail"> {
  return detail === undefined || detail === "" ? {} : { detail };
}

// Sortie records a terminal ending without a merge when an issue closes under an open pull
// request, so a terminal state's name is true of the end station only when every merge Sortie
// records moves the issue into it, which `merge_completion` does and `auto_merge` alone does not.
function endDetailOf(config: ConfigResponse): string | undefined {
  const target = config.reactions.merge_completion?.target_state;
  if (target !== undefined && target !== "") {
    return target;
  }
  const terminal = (config.tracker.terminal_states ?? []).filter((state) => state !== "");
  return terminal.length === 1 && config.reactions.auto_merge === undefined
    ? terminal[0]
    : undefined;
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
  /** The detail the station takes in this issue's lane only; null keeps the station's own. */
  detail: string | null;
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

function endingStationDetail(outcome: IssueOutcomeResponse): string | null {
  if (outcome.kind === "merged") {
    return "merged";
  }
  return outcome.kind === "terminal" && outcome.state !== undefined && outcome.state !== ""
    ? outcome.state
    : null;
}

/** Returns `null` when neither a state row nor an ending exists, so a finished issue without a recorded ending has no marker. The standing's `detail` applies to this issue only. */
export function standingOf(input: StandingInput, stations: readonly Station[]): Standing | null {
  const { deciding, outcome, issueId, attention, now } = input;
  if (deciding !== null) {
    return {
      station: decidingStation(deciding, stations),
      mark: rowMark(deciding.kind, issueId, attention, awaitsApproval(deciding)),
      time: timeText(deciding.kind, timeBaseOf(deciding), now),
      detail: null,
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
        detail: station.detailFromEnding === true ? endingStationDetail(outcome) : null,
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

/**
 * Words what the newest check of a reaction found, in the noun the header gives the station:
 * failures, comments, or conflicts, counted or "no", or checks still pending. Returns `undefined`
 * when no row holds a check the station can word.
 */
function checkDetailOf(key: string, pending: readonly PendingReading[]): string | undefined {
  const check = pending.find(
    (row) => row.config_key === key && row.last_check !== null,
  )?.last_check;
  if (check === undefined || check === null) {
    return undefined;
  }
  switch (key) {
    case "ci_failure":
      if (check.failing !== undefined && check.failing > 0) {
        return plural(check.failing, "failure", "failures");
      }
      if (check.pending !== undefined && check.pending > 0) {
        return `${fmtCount(check.pending)} pending`;
      }
      return check.failing === undefined ? undefined : "no failures";
    case "bot_review":
    case "review_comments":
      if (check.actionable_comments === undefined) {
        return undefined;
      }
      return check.actionable_comments > 0
        ? plural(check.actionable_comments, "comment", "comments")
        : "no comments";
    case "merge_conflicts":
      if (check.mergeable === undefined || check.mergeable === null) {
        return undefined;
      }
      return check.mergeable ? "no conflicts" : "conflicts";
    default:
      return undefined;
  }
}

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

/** The current station takes the standing's detail, and a reaction station what its newest check found, read from the pending pull request row while one exists, else from `prLoop`. */
export function laneOf(
  stations: readonly Station[],
  standing: Standing | null,
  pending: readonly PendingReading[],
  prLoop: PrLoop,
): LaneStation[] {
  const current = standing === null ? -1 : stations.findIndex((s) => s.key === standing.station);
  return stations.map((station, index) => ({
    ...(standing !== null && index === current
      ? {
          ...station,
          ...(standing.detail === null ? {} : { detail: standing.detail }),
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
        }),
    ...(station.kind === "reaction" ? detailOf(checkDetailOf(station.key, pending)) : {}),
  }));
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
