import { plural } from "@/lib/format";

const NONE = "<none>";
const UNKNOWN = "<unknown>";

const ORIGIN_LABELS: Record<string, string> = {
  poll_tick: "Picked up by a poll",
  stage_hop: "Stage hop",
  error: "Retry after an error",
  stall: "Retry after a stall",
  usage_limit: "Retry after a usage limit reset",
  continuation: "Continuation",
  ci_fix: "CI fix",
  ci: "CI fix",
  review: "Review fix",
  "bot-review": "Bot review fix",
  "merge-conflict": "Merge conflict fix",
  "label-review": "Review command",
  "label-fix": "Fix command",
  [NONE]: "CI result",
  [UNKNOWN]: "Not recorded",
};

const TRIGGER_LABELS: Record<string, string> = {
  error: "Retry after an error",
  stall: "Retry after a stall",
  usage_limit: "Retry after a usage limit reset",
  continuation: "Continuation",
  ci_fix: "CI fix",
  [NONE]: "Not a retry",
  [UNKNOWN]: "Not recorded",
};

const HOP_NOT_MADE_REASONS: Record<string, string> = {
  failed: "the next stage's label could not be added",
  ceiling: "the limit on automatic hops in a row was reached",
};

const REACTION_LABELS: Record<string, string> = {
  ci_failure: "CI fix",
  review_comments: "Review",
  bot_review: "Bot review",
  merge_conflicts: "Conflicts",
  auto_merge: "Merge",
  merge_completion: "Merge completion",
  label_commands: "Label commands",
};

function worded(labels: Record<string, string>, name: string): string {
  return Object.hasOwn(labels, name) ? labels[name] : name;
}

/** Words a rule name; an absent rule reads "No rule" and any rule the client has no wording for reads as served. */
export function ruleLabel(name: string | undefined): string {
  if (name === undefined || name === "" || name === NONE) {
    return "No rule";
  }
  return name === "default" ? "dispatch.default" : name;
}

/** Reports whether a rule breakdown group names a configured rule, as opposed to the dispatch default or runs that recorded no rule. */
export function isNamedRuleGroup(name: string): boolean {
  return name !== NONE && name !== "default";
}

/** Words a model name; a run that neither configured nor reported one reads "Not reported". */
export function modelLabel(name: string): string {
  return name === NONE ? "Not reported" : name;
}

/** Words an agent kind; a run with none reads "No agent". */
export function agentLabel(name: string): string {
  return name === NONE ? "No agent" : name;
}

/** Words a retry trigger or a run origin; a name the client has no wording for reads as served. */
export function startLabel(name: string, breakdown: "trigger" | "origin"): string {
  return worded(breakdown === "trigger" ? TRIGGER_LABELS : ORIGIN_LABELS, name);
}

/** Words a reaction config key; a key the client has no wording for reads as served. */
export function reactionLabel(configKey: string): string {
  return worded(REACTION_LABELS, configKey);
}

const REACTION_COUNT_NOUNS: Record<string, readonly [one: string, many: string]> = {
  ci_failure: ["fix", "fixes"],
  review_comments: ["fix", "fixes"],
  bot_review: ["fix", "fixes"],
  merge_conflicts: ["fix", "fixes"],
  label_commands: ["run", "runs"],
};

/** Words how many times a reaction acted; a key with no count wording, or a count below one, reads empty. */
export function reactionCountLabel(configKey: string, count: number): string {
  if (count < 1 || !Object.hasOwn(REACTION_COUNT_NOUNS, configKey)) {
    return "";
  }
  const [one, many] = REACTION_COUNT_NOUNS[configKey];
  return plural(count, one, many);
}

const REACTION_SHORT_LABELS: Record<string, string> = {
  bot_review: "Bot",
  merge_conflicts: "Confl.",
  merge_completion: "Completion",
  label_commands: "Commands",
};

/** Words a reaction config key in the form a narrow station header uses; a key whose label is already short has none. */
export function reactionShortLabel(configKey: string): string | undefined {
  return Object.hasOwn(REACTION_SHORT_LABELS, configKey)
    ? REACTION_SHORT_LABELS[configKey]
    : undefined;
}

/** Words why a hop was not made; a result the client has no wording for reads as served. */
export function hopReason(result: string): string {
  return worded(HOP_NOT_MADE_REASONS, result);
}

/** Words a stage hop as a lower-case phrase, such as "moved to review"; a result the client has no wording for reads as served. */
export function hopLabel(target: string, result: string | undefined): string {
  const rule = ruleLabel(target);
  switch (result) {
    case undefined:
    case "":
      return `hop to ${rule}`;
    case "advanced":
      return `moved to ${rule}`;
    case "partial":
      return `moved to ${rule}, an old stage label was not removed`;
    case "failed":
    case "ceiling":
      return `hop to ${rule} not made: ${hopReason(result)}`;
    default:
      return `hop to ${rule}: ${result}`;
  }
}
