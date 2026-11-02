import { Tooltip as BaseTooltip } from "@base-ui/react/tooltip";

import type { ReactNode } from "react";

export type TooltipProps = BaseTooltip.Root.Props;

export function Tooltip(props: TooltipProps) {
  return <BaseTooltip.Root {...props} />;
}

export type TooltipTriggerProps = BaseTooltip.Trigger.Props;

export function TooltipTrigger(props: TooltipTriggerProps) {
  return <BaseTooltip.Trigger {...props} />;
}

/** The look every tooltip and hint shares: at most 288px wide, lines filled to the widest, and a
 * long token such as a digest broken rather than run out of the box. */
export const TOOLTIP_POPUP =
  "max-w-72 rounded bg-surface-inverse px-xs py-xxs text-body-sm text-on-dark wrap-anywhere shadow-lg transition-opacity duration-(--duration-enter-small) data-ending-style:opacity-0 data-starting-style:opacity-0";

export interface TooltipPopupProps {
  children: ReactNode;
}

export function TooltipPopup({ children }: TooltipPopupProps) {
  return (
    <BaseTooltip.Portal>
      <BaseTooltip.Positioner side="top" sideOffset={4} className="z-(--z-tooltip)">
        <BaseTooltip.Popup className={TOOLTIP_POPUP}>{children}</BaseTooltip.Popup>
      </BaseTooltip.Positioner>
    </BaseTooltip.Portal>
  );
}
