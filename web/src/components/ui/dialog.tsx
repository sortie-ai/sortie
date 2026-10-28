import { Dialog as BaseDialog } from "@base-ui/react/dialog";

import { cn } from "@/lib/cn";

import type { ReactNode } from "react";

export type DialogProps = BaseDialog.Root.Props;

export function Dialog(props: DialogProps) {
  return <BaseDialog.Root {...props} />;
}

export interface DialogPopupProps {
  /** Accessible name when the content holds no {@link DialogTitle}. */
  "aria-label"?: string;
  children: ReactNode;
  className?: string;
}

export function DialogPopup({ children, className, ...rest }: DialogPopupProps) {
  return (
    <BaseDialog.Portal>
      <BaseDialog.Backdrop className="fixed inset-0 z-(--z-dialog) bg-scrim backdrop-blur-scrim transition-opacity duration-(--duration-enter) ease-enter data-ending-style:opacity-0 data-ending-style:duration-(--duration-exit) data-ending-style:ease-exit data-starting-style:opacity-0" />
      <BaseDialog.Viewport className="fixed inset-0 z-(--z-dialog) flex items-start justify-center overflow-hidden px-sm pt-[12vh] pb-sm">
        <BaseDialog.Popup
          className={cn(
            "flex max-h-full w-[min(36rem,100%)] flex-col overflow-hidden rounded-xl border border-transparent bg-canvas text-ink dark:border-line dark:bg-surface shadow-2xl outline-hidden",
            "transition-[opacity,translate,scale] duration-(--duration-enter) ease-enter",
            "data-ending-style:duration-(--duration-exit) data-ending-style:ease-exit",
            "data-starting-style:translate-y-(--offset-centered) data-starting-style:scale-(--scale-dialog) data-starting-style:opacity-0",
            "data-ending-style:translate-y-(--offset-centered) data-ending-style:scale-(--scale-dialog) data-ending-style:opacity-0",
            className,
          )}
          {...rest}
        >
          {children}
          <BaseDialog.Close className="sr-only">Close</BaseDialog.Close>
        </BaseDialog.Popup>
      </BaseDialog.Viewport>
    </BaseDialog.Portal>
  );
}

export interface DialogTitleProps {
  children: ReactNode;
  className?: string;
}

export function DialogTitle({ children, className }: DialogTitleProps) {
  return (
    <BaseDialog.Title className={cn("text-h7 font-semibold text-ink", className)}>
      {children}
    </BaseDialog.Title>
  );
}
