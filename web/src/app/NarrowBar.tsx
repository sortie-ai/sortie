import { useStateQuery } from "@/api/queries";
import { BrandMark } from "@/app/BrandMark";
import { keyTip } from "@/app/key-tips";
import { modifierLabel } from "@/app/keys";
import { SearchIcon } from "@/components/icons";
import { StatusMark } from "@/components/StatusMark";
import { Button } from "@/components/ui/button";
import { cn } from "@/lib/cn";
import { ACKNOWLEDGED_WORD, acknowledgedCount, condition } from "@/lib/condition";
import { fmtCount, pluralVerb } from "@/lib/format";

import type { StateResponse } from "@/api/types.gen";
import type { ReactElement } from "react";

function statusLabel(state: StateResponse): string | ReactElement {
  const { subject, verb } = pluralVerb(
    state.counts.attention,
    ["item", "items"],
    ["needs", "need"],
  );
  const needs = `${subject} ${verb} attention`;
  switch (condition(state)) {
    case "degraded":
      return state.counts.attention > 0 ? (
        <span className="flex flex-wrap gap-x-xs">
          <span>Degraded</span> <span>{needs}</span>
        </span>
      ) : (
        "Degraded"
      );
    case "needs":
      return needs;
    case "held":
      return `${fmtCount(acknowledgedCount(state))} ${ACKNOWLEDGED_WORD}`;
    case "clear":
      return "All clear";
  }
}

export interface NarrowBarProps {
  onOpenPalette: () => void;
}

export function NarrowBar({ onOpenPalette }: NarrowBarProps) {
  const { data } = useStateQuery();
  const project = data?.config?.tracker.project;
  const hasProject = project !== undefined && project !== "";

  return (
    <header className="sticky top-0 z-(--z-sticky) flex items-center gap-sm border-b border-line bg-canvas px-md py-sm md:hidden">
      <BrandMark />
      <div className={cn("flex flex-col", hasProject && "min-w-0")}>
        <span className="font-heading font-semibold">Sortie</span>
        {hasProject ? (
          <span title={project} className="truncate text-body-sm text-ink-muted">
            {project}
          </span>
        ) : undefined}
      </div>
      {data === undefined ? undefined : (
        <span className="min-w-0 text-body-sm wrap-anywhere">
          <StatusMark mark={condition(data)} label={statusLabel(data)} />
        </span>
      )}
      <Button
        className="ml-auto"
        onClick={onOpenPalette}
        title={keyTip("Open commands", modifierLabel(), "K")}
      >
        <SearchIcon />
        Menu
      </Button>
    </header>
  );
}
