import { Popover as BasePopover } from "@base-ui/react/popover";

import { CircleHelpIcon } from "@/components/icons";

import type { MouseEvent, ReactNode } from "react";

// A hint can sit inside a clickable row; opening it must not also activate the row.
function keepClickInside(event: MouseEvent) {
  event.stopPropagation();
}

export interface HintProps {
  hint: ReactNode;
  children: ReactNode;
}

/** A popover rather than a tooltip, because touch devices never open a tooltip. */
export function Hint({ hint, children }: HintProps) {
  return (
    <BasePopover.Root>
      <BasePopover.Trigger
        openOnHover
        onClick={keepClickInside}
        className="cursor-help underline decoration-line-strong decoration-dotted underline-offset-4 transition-colors duration-(--duration-feedback) outline-none hover:decoration-ink-body focus-visible:rounded-xs focus-visible:outline-2 focus-visible:outline-focus-ring data-popup-open:decoration-ink-body"
      >
        {children}
      </BasePopover.Trigger>
      <HintPopup hint={hint} />
    </BasePopover.Root>
  );
}

export interface HelpHintProps {
  hint: ReactNode;
  label: string;
}

export function HelpHint({ hint, label }: HelpHintProps) {
  return (
    <BasePopover.Root>
      <BasePopover.Trigger
        openOnHover
        onClick={keepClickInside}
        aria-label={label}
        className="inline-flex cursor-help text-line-strong transition-colors duration-(--duration-feedback) outline-none hover:text-ink-body focus-visible:rounded-full focus-visible:outline-2 focus-visible:outline-focus-ring data-popup-open:text-ink-body"
      >
        <CircleHelpIcon className="size-3" />
      </BasePopover.Trigger>
      <HintPopup hint={hint} />
    </BasePopover.Root>
  );
}

function HintPopup({ hint }: { hint: ReactNode }) {
  return (
    <BasePopover.Portal>
      <BasePopover.Positioner side="top" sideOffset={4} className="z-(--z-tooltip)">
        <BasePopover.Popup className="max-w-72 rounded bg-surface-inverse px-xs py-xxs text-body-sm text-on-dark shadow-lg outline-hidden transition-opacity duration-(--duration-enter-small) data-ending-style:opacity-0 data-starting-style:opacity-0">
          {hint}
        </BasePopover.Popup>
      </BasePopover.Positioner>
    </BasePopover.Portal>
  );
}
