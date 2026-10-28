import { useState } from "react";
import { useSearchParams } from "react-router";

import { SearchIcon, XIcon } from "@/components/icons";
import { ruleLabel } from "@/lib/labels";
import { RUN_STATUS_NAMES, runStatus } from "@/lib/run-status";

import { FilterMenu } from "./FilterMenu";

import type { HistoryFilter } from "@/api/client";
import type { ConfigResponse } from "@/api/types.gen";
import type { FilterOption } from "./FilterMenu";

const ISSUE_PARAM = "issue";
const RULE_PARAM = "rule";
const STATUS_PARAM = "status";
const STAGE_PARAM = "stage";
const RULE_PREFIX = "rule:";
const NO_RULE_VALUE = "none";

/** Reads the History filter the URL carries, in the values the API takes. */
export function filterFromParams(params: URLSearchParams): HistoryFilter {
  const filter: HistoryFilter = {};
  const issue = params.get(ISSUE_PARAM)?.trim() ?? "";
  const rule = params.get(RULE_PARAM) ?? "";
  const status = params.get(STATUS_PARAM) ?? "";
  const stage = params.get(STAGE_PARAM) ?? "";
  if (issue !== "") {
    filter.issue = issue;
  }
  if (rule === NO_RULE_VALUE) {
    filter.rule = "<none>";
  } else if (rule.startsWith(RULE_PREFIX) && rule.length > RULE_PREFIX.length) {
    filter.rule = rule.slice(RULE_PREFIX.length);
  }
  if (status !== "") {
    filter.status = status;
  }
  if (stage !== "") {
    filter.stage = stage;
  }
  return filter;
}

const FIELD =
  "h-(--size-control) w-56 max-w-full rounded-lg border border-line bg-field pr-xl pl-[calc(var(--spacing-sm)+var(--size-icon)+var(--spacing-xs))] text-body-md text-ink outline-none transition-colors duration-(--duration-feedback) placeholder:text-ink-muted hover:border-line-medium focus-visible:outline-2 focus-visible:-outline-offset-1 focus-visible:outline-focus-ring";

interface IssueKeyFieldProps {
  applied: string;
  onApply: (key: string) => void;
}

function IssueKeyField({ applied, onApply }: IssueKeyFieldProps) {
  const [draft, setDraft] = useState(applied);
  const [seen, setSeen] = useState(applied);
  if (seen !== applied) {
    setSeen(applied);
    setDraft(applied);
  }

  function handleKeyDown(event: React.KeyboardEvent<HTMLInputElement>) {
    if (event.key === "Enter") {
      const key = draft.trim();
      setDraft(key);
      onApply(key);
    }
  }

  function handleClear() {
    setDraft("");
    onApply("");
  }

  return (
    <span className="relative inline-flex items-center">
      <SearchIcon className="pointer-events-none absolute left-sm text-ink-muted" />
      <input
        type="text"
        aria-label="Issue key"
        placeholder="Issue key"
        value={draft}
        onChange={(event) => setDraft(event.target.value)}
        onKeyDown={handleKeyDown}
        className={FIELD}
      />
      {draft === "" ? undefined : (
        <button
          type="button"
          aria-label="Clear issue key"
          onClick={handleClear}
          className="absolute right-xxs inline-grid size-6 cursor-pointer place-items-center rounded text-ink-muted hover:text-ink focus-visible:outline-2 focus-visible:outline-focus-ring"
        >
          <XIcon />
        </button>
      )}
    </span>
  );
}

function ruleOptions(config: ConfigResponse | undefined): FilterOption[] {
  const options: FilterOption[] = [{ value: "", label: "Any rule" }];
  const seen = new Set<string>();
  for (const rule of config?.dispatch.rules ?? []) {
    const value = `${RULE_PREFIX}${rule.name ?? ""}`;
    if (rule.name !== undefined && rule.name !== "" && !seen.has(value)) {
      seen.add(value);
      options.push({ value, label: ruleLabel(rule.name) });
    }
  }
  if (config?.dispatch.default != null) {
    options.push({ value: `${RULE_PREFIX}default`, label: ruleLabel("default") });
  }
  options.push({ value: NO_RULE_VALUE, label: ruleLabel(undefined) });
  return options;
}

const STATUS_OPTIONS: readonly FilterOption[] = [
  { value: "", label: "Any status" },
  ...RUN_STATUS_NAMES.map((status) => ({ value: status, label: runStatus(status).word })),
];

function stageOptions(config: ConfigResponse | undefined): FilterOption[] {
  const options: FilterOption[] = [{ value: "", label: "Any stage" }];
  const seen = new Set<string>();
  for (const chain of config?.dispatch.chains ?? []) {
    for (const rule of chain.slice(1)) {
      if (!seen.has(rule)) {
        seen.add(rule);
        options.push({ value: rule, label: rule });
      }
    }
  }
  return options;
}

function valueTextOf(value: string, options: readonly FilterOption[], anyText: string): string {
  if (value === "") {
    return anyText;
  }
  const option = options.find((candidate) => candidate.value === value);
  if (option !== undefined) {
    return option.label;
  }
  return value.startsWith(RULE_PREFIX) ? value.slice(RULE_PREFIX.length) : value;
}

export interface HistoryFiltersProps {
  config: ConfigResponse | undefined;
}

export function HistoryFilters({ config }: HistoryFiltersProps) {
  const [params, setParams] = useSearchParams();
  const rule = params.get(RULE_PARAM) ?? "";
  const status = params.get(STATUS_PARAM) ?? "";
  const stage = params.get(STAGE_PARAM) ?? "";
  const rules = ruleOptions(config);
  const stages = stageOptions(config);
  const hasRules = (config?.dispatch.rules ?? []).length > 0;
  const hasChains = (config?.dispatch.chains ?? []).length > 0;

  function write(name: string, value: string) {
    setParams(
      (previous) => {
        const next = new URLSearchParams(previous);
        if (value === "") {
          next.delete(name);
        } else {
          next.set(name, value);
        }
        return next;
      },
      { replace: true },
    );
  }

  return (
    <div className="flex flex-wrap items-center gap-sm">
      <IssueKeyField
        applied={params.get(ISSUE_PARAM) ?? ""}
        onApply={(key) => write(ISSUE_PARAM, key)}
      />
      {hasRules || rule !== "" ? (
        <FilterMenu
          label="Rule"
          valueText={valueTextOf(rule, rules, "Any rule")}
          options={rules}
          value={rule}
          onChange={(value) => write(RULE_PARAM, value)}
        />
      ) : undefined}
      <FilterMenu
        label="Run status"
        valueText={valueTextOf(status, STATUS_OPTIONS, "Any status")}
        options={STATUS_OPTIONS}
        value={status}
        onChange={(value) => write(STATUS_PARAM, value)}
      />
      {hasChains || stage !== "" ? (
        <FilterMenu
          label="Hop to"
          valueText={valueTextOf(stage, stages, "Any stage")}
          options={stages}
          value={stage}
          onChange={(value) => write(STAGE_PARAM, value)}
        />
      ) : undefined}
    </div>
  );
}
