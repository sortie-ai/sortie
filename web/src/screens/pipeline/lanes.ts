import { activeRowOf, decidingRowOf } from "@/lib/active-rows";
import { buildEnded, endingWord } from "@/lib/ended";
import { parseInstant } from "@/lib/format";
import { stateWord } from "@/lib/run-status";
import {
  laneOf,
  lastStagePositionOf,
  lastStageRunOf,
  positionFrom,
  stageProgressOf,
  standingOf,
  stationsOf,
} from "@/lib/lane";

import type { PullRequestResponse, RunsResponse, StateResponse } from "@/api/types.gen";
import type { Deciding, Filterable } from "@/lib/active-rows";
import type { EndedIssue } from "@/lib/ended";
import type { LaneMarker, LaneStation, StagePosition, StageProgress, Station } from "@/lib/lane";

export const PIPELINE_COMPRESS_AFTER = 24;

export interface PipelineLane extends Filterable {
  issueId: string;
  identifier: string;
  displayKey: string;
  title: string | null;
  /** The rule the lane names when it draws no stage meter, or `null`. */
  route: string | null;
  agentKind: string | null;
  word: string;
  statusTextFull: string;
  marker: LaneMarker;
  stations: LaneStation[];
  station: string;
  position: StagePosition | null;
  progress: StageProgress | null;
  pullRequestNumber: number | null;
  /** Whether a state row decides where the issue stands; an issue that ended today has none. */
  live: boolean;
}

/** A station is `hot` while a lane at it is marked as needing attention; a station no lane stands at has no entry. */
export function stationTally(
  lanes: readonly PipelineLane[],
): Map<string, { count: number; hot: boolean }> {
  const tally = new Map<string, { count: number; hot: boolean }>();
  for (const lane of lanes) {
    const entry = tally.get(lane.station) ?? { count: 0, hot: false };
    tally.set(lane.station, {
      count: entry.count + 1,
      hot: entry.hot || lane.marker === "needs",
    });
  }
  return tally;
}

function kindOf(stations: readonly Station[], key: string) {
  return stations.find((station) => station.key === key)?.kind ?? null;
}

function ownPullRequestOf(deciding: Deciding): PullRequestResponse | undefined {
  switch (deciding.kind) {
    case "running":
    case "pending_reactions":
      return deciding.entry.pull_request;
    default:
      return undefined;
  }
}

function laneWord(marker: LaneMarker): string {
  return marker === "ended" ? "Ended" : stateWord(marker);
}

function activeLanes(state: StateResponse, stations: readonly Station[], now: number) {
  const attention = state.attention ?? [];
  const lanes: PipelineLane[] = [];
  for (const entry of state.active ?? []) {
    const row = activeRowOf(state, entry);
    const deciding = decidingRowOf(state, entry);
    if (row === undefined || deciding === undefined) {
      continue;
    }
    const standing = standingOf(
      { deciding, outcome: null, issueId: entry.issue_id, attention, now },
      stations,
    );
    if (standing === null) {
      continue;
    }
    const pending = (state.pending_reactions ?? []).filter(
      (reaction) => reaction.issue_id === entry.issue_id,
    );
    const position = positionFrom(entry.stage_position);
    lanes.push({
      issueId: row.issueId,
      identifier: row.identifier,
      displayKey: row.displayKey,
      title: row.title,
      phase: row.phase,
      rule: row.rule,
      route: position === null ? row.route : null,
      agentKind: row.agentKind,
      word: laneWord(standing.mark ?? row.mark),
      statusTextFull: row.statusTextFull,
      marker: standing.mark ?? row.mark,
      stations: laneOf(stations, standing, pending, entry.pr_loop),
      station: standing.station,
      position,
      progress:
        position === null
          ? null
          : stageProgressOf(position, true, kindOf(stations, standing.station), undefined),
      pullRequestNumber: (ownPullRequestOf(deciding) ?? pending[0]?.pull_request)?.number ?? null,
      live: true,
    });
  }
  return lanes;
}

function byEndingTime(a: EndedIssue, b: EndedIssue): number {
  const observed =
    (parseInstant(a.outcome.observed_at) ?? 0) - (parseInstant(b.outcome.observed_at) ?? 0);
  if (observed !== 0) {
    return observed;
  }
  if (a.identifier !== b.identifier) {
    return a.identifier < b.identifier ? -1 : 1;
  }
  return a.issueId < b.issueId ? -1 : a.issueId > b.issueId ? 1 : 0;
}

function endedLanes(
  state: StateResponse,
  pages: readonly RunsResponse[],
  stations: readonly Station[],
  now: number,
) {
  const attention = state.attention ?? [];
  const hasRules = (state.config?.dispatch.rules ?? []).length > 0;
  const runs = pages.flatMap((page) => page.runs ?? []);
  const lanes: PipelineLane[] = [];
  for (const issue of buildEnded([...pages], state).sort(byEndingTime)) {
    const issueRuns = runs.filter((run) => run.issue_id === issue.issueId);
    const position = lastStagePositionOf(issueRuns);
    const standing = standingOf(
      { deciding: null, outcome: issue.outcome, issueId: issue.issueId, attention, now },
      stations,
    );
    if (standing === null) {
      continue;
    }
    lanes.push({
      issueId: issue.issueId,
      identifier: issue.identifier,
      displayKey: issue.displayKey,
      title: issue.title,
      phase: "ended_today",
      rule: position?.current ?? issue.rule,
      route: position === null && hasRules ? issue.rule : null,
      agentKind: issue.agentKind,
      word: endingWord(issue.outcome),
      statusTextFull: issue.outcome.status_text_full,
      marker: "ended",
      stations: laneOf(stations, standing, [], issue.prLoop),
      station: standing.station,
      position,
      progress:
        position === null
          ? null
          : stageProgressOf(
              position,
              false,
              kindOf(stations, standing.station),
              lastStageRunOf(issueRuns, position.current),
            ),
      pullRequestNumber: issue.outcome.pull_request?.number ?? null,
      live: false,
    });
  }
  return lanes;
}

/** `pages` undefined means today's endings are not loaded, and only active lanes come back; a state without `config` has no stations and so no lanes. */
export function pipelineLanes(
  state: StateResponse,
  pages: readonly RunsResponse[] | undefined,
  now: number,
): PipelineLane[] {
  if (state.config === undefined) {
    return [];
  }
  const stations = stationsOf(state.config);
  const active = activeLanes(state, stations, now);
  return pages === undefined ? active : [...active, ...endedLanes(state, pages, stations, now)];
}

export function laneNavKey(issueId: string): string {
  return `lane:${issueId}`;
}
