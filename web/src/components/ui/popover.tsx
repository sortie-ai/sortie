import { Popover as BasePopover } from "@base-ui/react/popover";

import { cn } from "@/lib/cn";

import type { ReactNode } from "react";

export type PopoverProps = BasePopover.Root.Props;

export function Popover(props: PopoverProps) {
  return <BasePopover.Root {...props} />;
}

export type PopoverTriggerProps = BasePopover.Trigger.Props;

export function PopoverTrigger(props: PopoverTriggerProps) {
  return <BasePopover.Trigger {...props} />;
}

export interface PopoverPopupProps {
  "aria-label"?: string;
  children: ReactNode;
  side?: BasePopover.Positioner.Props["side"];
  align?: BasePopover.Positioner.Props["align"];
  /** Whether the popup opens onto the content of the panel whose title line holds its trigger. */
  isOnPanelContent?: boolean;
  className?: string;
}

export function PopoverPopup({
  children,
  side = "bottom",
  align = "start",
  isOnPanelContent = false,
  className,
  ...rest
}: PopoverPopupProps) {
  return (
    <BasePopover.Portal>
      <BasePopover.Positioner
        side={side}
        align={align}
        sideOffset={isOnPanelContent ? 0 : 4}
        className="z-(--z-dropdown) outline-hidden"
      >
        <BasePopover.Popup
          className={cn(
            // Clipped to its rounding, so a field or a row at its edge keeps the corners.
            "origin-(--transform-origin) overflow-clip rounded-lg border border-line bg-canvas text-ink shadow-lg outline-hidden",
            // The panel's controls overhang its title line; the popup starts that much closer.
            isOnPanelContent &&
              "data-[side=bottom]:mt-[calc(var(--spacing-sm)-(var(--size-control)-var(--text-h7--line-height))/2)] data-[side=top]:mb-[calc(var(--spacing-sm)-(var(--size-control)-var(--text-h7--line-height))/2)]",
            "transition-[opacity,translate] duration-(--duration-enter-small) ease-enter",
            "data-ending-style:duration-(--duration-exit) data-ending-style:ease-exit",
            "data-starting-style:opacity-0 data-ending-style:opacity-0",
            "data-[side=top]:data-starting-style:translate-y-(--offset-anchored) data-[side=top]:data-ending-style:translate-y-(--offset-anchored)",
            "data-[side=bottom]:data-starting-style:-translate-y-(--offset-anchored) data-[side=bottom]:data-ending-style:-translate-y-(--offset-anchored)",
            className,
          )}
          {...rest}
        >
          {children}
        </BasePopover.Popup>
      </BasePopover.Positioner>
    </BasePopover.Portal>
  );
}
