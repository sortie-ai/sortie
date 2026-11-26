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

/** The distance every tooltip and hint keeps from the window's edges. */
// Base UI adds 1px to the top padding when it decides whether to flip, so the top is one less than
// the 8px a note keeps from the window.
export const TOOLTIP_COLLISION_PADDING = { top: 7, right: 8, bottom: 8, left: 8 };

export interface TooltipPopupProps {
  children: ReactNode;
  /** The chart a column's note opens beside, from the column's top, on the side the chart has room for. */
  besideChart?: Element | null;
}

// A chart column spans the plot's height, so a note above it would cover the chart's title and legend.
function besideProps(chart: Element) {
  return {
    side: "right",
    align: "start",
    collisionBoundary: chart,
    collisionPadding: 0,
    collisionAvoidance: { side: "flip", align: "none", fallbackAxisSide: "none" },
  } as const;
}

export function TooltipPopup({ children, besideChart }: TooltipPopupProps) {
  const placement =
    besideChart === undefined || besideChart === null
      ? ({ side: "top", collisionPadding: TOOLTIP_COLLISION_PADDING } as const)
      : besideProps(besideChart);
  return (
    <BaseTooltip.Portal>
      <BaseTooltip.Positioner {...placement} sideOffset={4} className="z-(--z-tooltip)">
        <BaseTooltip.Popup className={TOOLTIP_POPUP}>{children}</BaseTooltip.Popup>
      </BaseTooltip.Positioner>
    </BaseTooltip.Portal>
  );
}
