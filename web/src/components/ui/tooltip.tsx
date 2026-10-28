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

export interface TooltipPopupProps {
  children: ReactNode;
}

export function TooltipPopup({ children }: TooltipPopupProps) {
  return (
    <BaseTooltip.Portal>
      <BaseTooltip.Positioner side="top" sideOffset={4} className="z-(--z-tooltip)">
        <BaseTooltip.Popup className="max-w-72 rounded bg-surface-inverse px-xs py-xxs text-body-sm text-on-dark shadow-lg transition-opacity duration-(--duration-enter-small) data-ending-style:opacity-0 data-starting-style:opacity-0">
          {children}
        </BaseTooltip.Popup>
      </BaseTooltip.Positioner>
    </BaseTooltip.Portal>
  );
}
