import { condition } from "@/lib/condition";
import { fmtClock, pluralVerb } from "@/lib/format";

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

/** Reaction kinds in the order a lede names them, with the word each takes. */
const KIND_WORDS: ReadonlyMap<string, string> = new Map([
  ["ci_failure", "CI"],
  ["review_comments", "review"],
  ["bot_review", "bot review"],
  ["merge_conflicts", "conflicts"],
  ["auto_merge", "merge"],
  ["merge_completion", "merge"],
]);

function kindWord(configKey: string): string {
  return KIND_WORDS.get(configKey) ?? configKey.replaceAll("_", " ");
}

function joinWords(words: readonly string[], conjunction: "and" | "or"): string {
  const last = words.at(-1);
  if (last === undefined || words.length === 1) {
    return last ?? "";
  }
  return `${words.slice(0, -1).join(", ")} ${conjunction} ${last}`;
}

function waitingKinds(keys: readonly string[]): string[] {
  const known = [...KIND_WORDS.keys()].filter((key) => keys.includes(key));
  const others = [...new Set(keys)].filter((key) => !KIND_WORDS.has(key));
  return [...new Set([...known, ...others].map(kindWord))];
}

function clause(
  n: number,
  nouns: readonly [string, string],
  verbs: readonly [string, string],
  rest: string,
): Segment[] {
  const { subject, verb } = pluralVerb(n, nouns, verbs);
  return [{ text: subject, emphasis: "strong" }, { text: ` ${verb} ${rest}` }];
}

function handledClauses(state: StateResponse, acknowledged: number): Segment[][] {
  const waiting = (state.pending_reactions ?? []).filter(
    (reaction) => reaction.escalated_at === null,
  );
  const { running, retrying, between_stages: between } = state.counts;
  const clauses: Segment[][] = [];
  if (running > 0) {
    clauses.push(clause(running, ["agent", "agents"], ["is", "are"], "running"));
  }
  if (waiting.length > 0) {
    const kinds = joinWords(waitingKinds(waiting.map((reaction) => reaction.config_key)), "or");
    const pullRequests = new Set(
      waiting.map(({ pull_request: pr }) => JSON.stringify([pr.repository, pr.number])),
    );
    clauses.push(
      clause(
        pullRequests.size,
        ["pull request", "pull requests"],
        ["is", "are"],
        `waiting on ${kinds}`,
      ),
    );
  }
  if (retrying > 0) {
    clauses.push(clause(retrying, ["retry", "retries"], ["is", "are"], "scheduled"));
  }
  if (between > 0) {
    clauses.push(clause(between, ["issue", "issues"], ["is", "are"], "between stages"));
  }
  if (acknowledged > 0) {
    clauses.push(
      clause(
        acknowledged,
        ["acknowledged item", "acknowledged items"],
        ["waits", "wait"],
        "in Active",
      ),
    );
  }
  return clauses;
}

function lede(clauses: Segment[][], lead: string): Segment[] {
  if (clauses.length === 0) {
    return [{ text: lead === "" ? "No agents are running." : `${lead}no agents are running.` }];
  }
  const segments: Segment[] = [{ text: lead }];
  clauses.forEach((segmentsOfClause, index) => {
    if (index > 0) {
      segments.push({ text: index === clauses.length - 1 ? " and " : ", " });
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
  const clauses = handledClauses(state, acknowledged);
  const critical = items.filter((item) => item.severity === "critical").length;
  const isDegraded = condition(state) === "degraded" || critical > 0;
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
      lede: lede(clauses, "Everything else is handled: "),
    };
  }
  if (items.length > 0) {
    const needs = pluralVerb(items.length, ["item", "items"], ["needs", "need"]);
    return {
      heading: [{ text: `${needs.count} ${needs.noun} ${needs.verb} attention.` }],
      lede: lede(clauses, "Everything else is handled: "),
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
      lede: lede(handledClauses(state, 0), ""),
    };
  }
  return { heading: [{ text: "Nothing needs attention." }], lede: lede(clauses, "") };
}
