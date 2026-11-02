import { IssueTitleLink } from "@/components/IssueTitleLink";
import { MarkSlot } from "@/components/Lane";
import { Parts } from "@/components/Parts";
import { MONO } from "@/components/ui/mono";
import { cn } from "@/lib/cn";
import { fmtClock, parseInstant } from "@/lib/format";
import { timeCell } from "@/lib/active-rows";
import { ROW_GRID, rowStyles, secondLineColor } from "@/lib/rows";
import { stateWord } from "@/lib/run-status";

import { activeNavKey } from "./rows";

import type { ActiveRow as ActiveRowModel } from "@/lib/active-rows";

export interface ActiveRowProps {
  row: ActiveRowModel;
  /** Server-clock time in epoch milliseconds that the time cell measures against. */
  now: number;
  isSelected: boolean;
  /** Whether the selected-row look shows, which it does from the first navigation key on. */
  showSelection: boolean;
  onOpen: (row: ActiveRowModel) => void;
}

export function ActiveRow({ row, now, isSelected, showSelection, onOpen }: ActiveRowProps) {
  const isLook = isSelected && showSelection;
  const hasTitle = row.title !== null;
  const meta = [
    hasTitle ? row.displayKey : null,
    row.route === null ? null : (
      <span key="route" className={MONO}>
        {row.route}
      </span>
    ),
    row.agentKind === null ? null : (
      <span key="agent" className={MONO}>
        {row.agentKind}
      </span>
    ),
  ].filter((part) => part !== null);
  const base = parseInstant(row.timeBase);

  return (
    <li
      data-nav-key={activeNavKey(row.issueId)}
      onClick={() => onOpen(row)}
      className={cn(ROW_GRID, rowStyles(isLook, true, row.mark === "needs"))}
    >
      <MarkSlot marker={row.mark} />
      <div className="min-w-0">
        <p className="text-body-md font-medium text-ink">
          <IssueTitleLink identifier={row.identifier}>{row.title ?? row.displayKey}</IssueTitleLink>
        </p>
        {meta.length === 0 ? undefined : (
          <p className={cn("text-body-sm tabular-nums", secondLineColor(isLook))}>
            <Parts parts={meta} />
          </p>
        )}
        {row.holdText === null ? undefined : (
          <p className={cn("text-body-sm", secondLineColor(isLook))}>{row.holdText}</p>
        )}
      </div>
      <p
        className={cn(
          "text-body-md max-md:col-start-2",
          row.mark === "needs" ? "text-warning" : "text-ink-body",
        )}
      >
        {stateWord(row.mark)}
      </p>
      <p
        className="text-body-md text-ink-body tabular-nums max-md:col-start-2 md:text-right"
        title={base === null ? undefined : fmtClock(base)}
      >
        {timeCell(row, now)}
      </p>
    </li>
  );
}
