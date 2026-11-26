import { useStateQuery } from "@/api/queries";
import { BrandLink } from "@/app/BrandLink";
import { keyTip } from "@/app/key-tips";
import { modifierLabel } from "@/app/keys";
import { Link } from "@/components/carried-links";
import { SearchIcon } from "@/components/icons";
import { StatusMark } from "@/components/StatusMark";
import { Button } from "@/components/ui/button";
import { ACKNOWLEDGED_WORD, acknowledgedCount, condition } from "@/lib/condition";
import { cn } from "@/lib/cn";
import { fmtCount, pluralVerb } from "@/lib/format";

import type { StateResponse } from "@/api/types.gen";

function statusLabel(state: StateResponse): string {
  const { subject, verb } = pluralVerb(
    state.counts.attention,
    ["item", "items"],
    ["needs", "need"],
  );
  const needs = `${subject} ${verb} attention`;
  switch (condition(state)) {
    case "degraded":
      return state.counts.attention > 0 ? `Degraded, ${needs}` : "Degraded";
    case "needs":
      return needs;
    case "held":
      return `${fmtCount(acknowledgedCount(state))} ${ACKNOWLEDGED_WORD}`;
    case "clear":
      return "All clear";
  }
}

const LINK =
  "rounded-sm focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-focus-ring";

export interface NarrowBarProps {
  onOpenPalette: () => void;
}

export function NarrowBar({ onOpenPalette }: NarrowBarProps) {
  const { data } = useStateQuery();

  return (
    <header className="sticky top-0 z-(--z-sticky) flex items-center gap-x-md border-b border-line bg-canvas px-md py-sm md:hidden">
      <BrandLink />
      <div className="flex min-w-0 flex-1 justify-center">
        {data === undefined ? undefined : (
          <Link
            to="/"
            title={statusLabel(data)}
            className={cn(
              "flex max-w-full min-w-0 text-body-sm hover:underline hover:underline-offset-2",
              LINK,
            )}
          >
            <StatusMark
              mark={condition(data)}
              label={<span className="block truncate">{statusLabel(data)}</span>}
              className="min-w-0 *:min-w-0"
            />
          </Link>
        )}
      </div>
      <Button onClick={onOpenPalette} title={keyTip("Open commands", modifierLabel(), "K")}>
        <SearchIcon />
        Menu
      </Button>
    </header>
  );
}
