import { Drawer as BaseDrawer } from "@base-ui/react/drawer";

import { buttonStyles } from "@/components/ui/button";
import { cn } from "@/lib/cn";

import type { ReactNode } from "react";

/** Props of {@link Drawer}. */
export type DrawerProps = Omit<BaseDrawer.Root.Props, "swipeDirection">;

/** Holds the open state of a drawer that enters from the right edge and is swiped away to the right. */
export function Drawer(props: DrawerProps) {
  return <BaseDrawer.Root swipeDirection="right" {...props} />;
}

/** Props of {@link DrawerPopup}. */
export interface DrawerPopupProps {
  /** Accessible name when the content holds no {@link DrawerTitle}. */
  "aria-label"?: string;
  children: ReactNode;
  className?: string;
}

/**
 * Renders the panel over a scrim: 500 px wide, the full width below 768 px. It
 * travels in on the enter duration and leaves on the shorter exit duration.
 */
export function DrawerPopup({ children, className, ...rest }: DrawerPopupProps) {
  return (
    <BaseDrawer.Portal>
      <BaseDrawer.Backdrop className="fixed inset-0 z-(--z-scrim) bg-scrim backdrop-blur-scrim transition-opacity duration-(--duration-enter) ease-enter data-ending-style:opacity-0 data-ending-style:duration-(--duration-exit) data-ending-style:ease-exit data-starting-style:opacity-0" />
      <BaseDrawer.Viewport className="fixed inset-0 z-(--z-drawer) flex items-stretch justify-end">
        <BaseDrawer.Popup
          className={cn(
            "h-full w-[500px] max-w-full overflow-y-auto overscroll-contain border-l border-transparent bg-canvas text-ink dark:border-line dark:bg-surface shadow-2xl outline-hidden max-md:w-full",
            "transform-[translateX(var(--drawer-swipe-movement-x,0px))] transition-transform duration-(--duration-enter) ease-enter",
            "data-swiping:duration-0 data-ending-style:duration-(--duration-exit) data-ending-style:ease-exit",
            "data-starting-style:transform-[translateX(100%)] data-ending-style:transform-[translateX(100%)]",
            className,
          )}
          {...rest}
        >
          <BaseDrawer.Content>{children}</BaseDrawer.Content>
        </BaseDrawer.Popup>
      </BaseDrawer.Viewport>
    </BaseDrawer.Portal>
  );
}

/** Props of {@link DrawerTitle}. */
export interface DrawerTitleProps {
  children: ReactNode;
  className?: string;
}

/** Names the drawer for assistive technology. */
export function DrawerTitle({ children, className }: DrawerTitleProps) {
  return (
    <BaseDrawer.Title className={cn("text-h7 font-semibold text-ink", className)}>
      {children}
    </BaseDrawer.Title>
  );
}

/** Props of {@link DrawerClose}. */
export interface DrawerCloseProps extends Omit<BaseDrawer.Close.Props, "className"> {
  className?: string;
}

/** Renders a secondary-style button that closes the drawer. */
export function DrawerClose({ className, ...rest }: DrawerCloseProps) {
  return <BaseDrawer.Close className={buttonStyles("secondary", className)} {...rest} />;
}
