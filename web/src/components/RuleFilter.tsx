import { useMemo, useState } from "react";
import { useSearchParams } from "react-router";

import { ChevronDownIcon } from "@/components/icons";
import {
  Combobox,
  ComboboxEmpty,
  ComboboxInput,
  ComboboxItem,
  ComboboxList,
} from "@/components/ui/combobox";
import { MONO } from "@/components/ui/mono";
import { Popover, PopoverPopup, PopoverTrigger } from "@/components/ui/popover";
import { SELECT_TRIGGER } from "@/components/ui/select-trigger";
import { ruleOptions } from "@/lib/active-rows";
import { cn } from "@/lib/cn";

import type { Filterable, RuleOption } from "@/lib/active-rows";
import type { ConfigResponse } from "@/api/types.gen";

const RULE_PARAM = "rule";

const CAPTION = "px-md pt-xs pb-xxs text-label-caps font-medium text-ink-muted";

function describe(value: string, options: readonly RuleOption[]): string {
  if (value === "") {
    return "Any rule";
  }
  const option = options.find((candidate) => candidate.value === value);
  if (option !== undefined) {
    return option.label;
  }
  if (value === "none") {
    return "No rule";
  }
  return value.slice(value.indexOf(":") + 1);
}

export interface RuleFilterProps {
  config: ConfigResponse | undefined;
  /** Every row before any filter; the option counts read them. */
  rows: readonly Filterable[];
}

export function RuleFilter({ config, rows }: RuleFilterProps) {
  const [params, setParams] = useSearchParams();
  const [isOpen, setIsOpen] = useState(false);
  const [query, setQuery] = useState("");
  const chosen = params.get(RULE_PARAM) ?? "";
  const all = useMemo(() => ruleOptions(config, rows, ""), [config, rows]);
  const shown = useMemo(() => ruleOptions(config, rows, query), [config, rows, query]);

  if (all.length === 0) {
    return undefined;
  }

  const selectable = all.filter((option) => option.kind !== "caption");
  const visible = shown.filter((option) => option.kind !== "caption");
  const captions = new Map<string, string>();
  shown.forEach((option, index) => {
    const next = shown[index + 1];
    if (option.kind === "caption" && next !== undefined) {
      captions.set(next.value, option.label);
    }
  });
  const current = selectable.find((option) => option.value === chosen) ?? null;

  function handleOpenChange(open: boolean) {
    setIsOpen(open);
    setQuery("");
  }

  function handleValueChange(option: RuleOption | null) {
    if (option === null) {
      return;
    }
    setParams(
      (previous) => {
        const next = new URLSearchParams(previous);
        if (option.value === "") {
          next.delete(RULE_PARAM);
        } else {
          next.set(RULE_PARAM, option.value);
        }
        return next;
      },
      { replace: true },
    );
    handleOpenChange(false);
  }

  return (
    <Popover open={isOpen} onOpenChange={handleOpenChange}>
      <PopoverTrigger className={SELECT_TRIGGER}>
        Rule
        <span className="text-ink wrap-anywhere">{describe(chosen, selectable)}</span>
        <ChevronDownIcon />
      </PopoverTrigger>
      <PopoverPopup
        aria-label="Rule"
        align="end"
        isOnPanelContent
        className="w-80 max-w-[calc(100vw-2rem)]"
      >
        <Combobox
          items={selectable}
          filteredItems={visible}
          inputValue={query}
          onInputValueChange={(value, details) => {
            if (details.reason === "input-change" || details.reason === "input-clear") {
              setQuery(value);
            }
          }}
          value={current}
          onValueChange={handleValueChange}
          itemToStringLabel={(option) => option.label}
          isItemEqualToValue={(a, b) => a.value === b.value}
        >
          <ComboboxInput
            placeholder="Search rules and stages"
            aria-label="Search rules and stages"
          />
          <ComboboxList>
            {(option: RuleOption) => (
              <div key={`${option.kind}:${option.group ?? ""}:${option.value}`}>
                {captions.has(option.value) ? (
                  <div role="presentation" className={CAPTION}>
                    {captions.get(option.value)}
                  </div>
                ) : undefined}
                <ComboboxItem value={option} indented={option.kind === "member"}>
                  <span
                    className={
                      option.kind === "chain"
                        ? "font-semibold wrap-anywhere"
                        : option.kind === "any" || option.kind === "none"
                          ? "wrap-anywhere"
                          : cn(MONO, "wrap-anywhere")
                    }
                  >
                    {option.label}
                  </span>
                  <span className="text-body-sm text-ink-muted tabular-nums">{option.count}</span>
                </ComboboxItem>
              </div>
            )}
          </ComboboxList>
          <ComboboxEmpty>Nothing matches "{query}".</ComboboxEmpty>
        </Combobox>
      </PopoverPopup>
    </Popover>
  );
}
