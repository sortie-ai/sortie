import { Popover as BasePopover } from "@base-ui/react/popover";

import { TOOLTIP_COLLISION_PADDING, TOOLTIP_POPUP } from "@/components/ui/tooltip";
import { cn } from "@/lib/cn";

import type { MouseEvent, ReactNode } from "react";

// A hint can sit inside a clickable row; opening it must not also activate the row.
function keepClickInside(event: MouseEvent) {
  event.stopPropagation();
}

export interface HintProps {
  hint: ReactNode;
  children: ReactNode;
  /** Hover delay before the hint opens, in ms; omitted, Base UI's default applies. */
  delay?: number;
}

/** A popover rather than a tooltip, because touch devices never open a tooltip. */
export function Hint({ hint, children, delay }: HintProps) {
  return (
    <BasePopover.Root>
      <BasePopover.Trigger
        openOnHover
        delay={delay}
        onClick={keepClickInside}
        className="cursor-help underline decoration-line-strong decoration-dotted underline-offset-4 transition-colors duration-(--duration-feedback) outline-none hover:decoration-ink-body focus-visible:rounded-xs focus-visible:outline-2 focus-visible:outline-focus-ring data-popup-open:decoration-ink-body"
      >
        {children}
      </BasePopover.Trigger>
      <HintPopup hint={hint} />
    </BasePopover.Root>
  );
}

function HintPopup({ hint }: { hint: ReactNode }) {
  return (
    <BasePopover.Portal>
      <BasePopover.Positioner
        side="top"
        sideOffset={4}
        collisionPadding={TOOLTIP_COLLISION_PADDING}
        className="z-(--z-tooltip)"
      >
        <BasePopover.Popup className={cn(TOOLTIP_POPUP, "outline-hidden")}>
          {hint}
        </BasePopover.Popup>
      </BasePopover.Positioner>
    </BasePopover.Portal>
  );
}
