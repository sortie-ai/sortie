import { condition } from "@/lib/condition";
import { fmtClock, plural, pluralVerb } from "@/lib/format";

import type { StateResponse } from "@/api/types.gen";

/** A run of headline text; `strong` is a bold count, `critical` colors a lead phrase. */
export interface Segment {
  text: string;
  emphasis?: "strong" | "critical";
}

export interface Headline {
  heading: Segment[];
  lede: Segment[];
}

function clause(n: number, nouns: readonly [string, string], rest: string): Segment[] {
  return [{ text: plural(n, nouns[0], nouns[1]), emphasis: "strong" }, { text: ` ${rest}` }];
}

/** Counts what runs without a person in as few words as it takes, so the headline keeps to one line; while Sortie is degraded it leaves out the issues between stages and the acknowledged items. */
function handledClauses(
  state: StateResponse,
  acknowledged: number,
  isDegraded: boolean,
): Segment[][] {
  const waiting = (state.pending_reactions ?? []).filter(
    (reaction) => reaction.escalated_at === null,
  );
  const { running, retrying, between_stages: between } = state.counts;
  const clauses: Segment[][] = [];
  if (running > 0) {
    clauses.push(clause(running, ["agent", "agents"], "running"));
  }
  if (waiting.length > 0) {
    const pullRequests = new Set(
      waiting.map(({ pull_request: pr }) => JSON.stringify([pr.repository, pr.number])),
    );
    clauses.push(clause(pullRequests.size, ["pull request", "pull requests"], "watched"));
  }
  if (retrying > 0) {
    clauses.push(clause(retrying, ["retry", "retries"], "scheduled"));
  }
  if (isDegraded) {
    return clauses;
  }
  if (between > 0) {
    clauses.push(clause(between, ["issue", "issues"], "between stages"));
  }
  if (acknowledged > 0) {
    clauses.push(clause(acknowledged, ["item", "items"], "acknowledged"));
  }
  return clauses;
}

function lede(clauses: Segment[][], lead: string): Segment[] {
  if (clauses.length === 0) {
    return [{ text: lead === "" ? "No agents running." : `${lead}no agents running.` }];
  }
  const segments: Segment[] = [{ text: lead }];
  clauses.forEach((segmentsOfClause, index) => {
    if (index > 0) {
      segments.push({ text: ", " });
    }
    segments.push(...segmentsOfClause);
  });
  segments.push({ text: "." });
  return segments;
}

function firstRunLede(state: StateResponse): string {
  const { subject } = pluralVerb(state.counts.running, ["issue", "issues"], ["is", "are"]);
  const started = state.started_at === null ? "" : `Started at ${fmtClock(state.started_at)}. `;
  return `${started}${subject} picked up, with no finished runs yet.`;
}

/** A first run, with no run on record, reads as picking up only while an agent runs and nothing needs attention or waits on a person; a failing health check or a critical item reads as degraded. */
export function buildHeadline(state: StateResponse, isFirstRun: boolean): Headline {
  const items = (state.attention ?? []).filter((item) => item.acknowledged_at === null);
  const acknowledged = (state.attention ?? []).length - items.length;
  const critical = items.filter((item) => item.severity === "critical").length;
  const isDegraded = condition(state) === "degraded" || critical > 0;
  const clauses = handledClauses(state, acknowledged, isDegraded);
  const others = items.length - critical;

  if (
    isFirstRun &&
    state.counts.running > 0 &&
    items.length === 0 &&
    acknowledged === 0 &&
    !isDegraded
  ) {
    return {
      heading: [{ text: "Sortie is picking up its first issues." }],
      lede: [{ text: firstRunLede(state) }],
    };
  }

  if (isDegraded) {
    const more = pluralVerb(others, ["item", "items"], ["needs", "need"]);
    return {
      heading: [
        { text: "Sortie is degraded", emphasis: "critical" },
        ...(others > 0
          ? [{ text: `, and ${more.count} more ${more.noun} ${more.verb} attention.` }]
          : [{ text: ", and nothing else needs attention." }]),
      ],
      lede: lede(clauses, "Handled: "),
    };
  }
  if (items.length > 0) {
    const needs = pluralVerb(items.length, ["item", "items"], ["needs", "need"]);
    return {
      heading: [{ text: `${needs.count} ${needs.noun} ${needs.verb} attention.` }],
      lede: lede(clauses, "Handled: "),
    };
  }
  if (acknowledged > 0) {
    const held = pluralVerb(
      acknowledged,
      ["acknowledged item", "acknowledged items"],
      ["waits", "wait"],
    );
    return {
      heading: [{ text: `${held.count} ${held.noun} ${held.verb} on a person.` }],
      lede: lede(handledClauses(state, 0, false), ""),
    };
  }
  return { heading: [{ text: "Nothing needs attention." }], lede: lede(clauses, "") };
}
