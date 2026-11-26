import { cn } from "@/lib/cn";

import type { ReactNode } from "react";

export interface PanelProps {
  title: ReactNode;
  /** Words beside the title, such as a count or the order the panel keeps. */
  meta?: ReactNode;
  /** Chips that act on the whole panel, right after the meta. */
  beside?: ReactNode;
  /** Words at the end of the title's line, such as a legend or a link to more. */
  note?: ReactNode;
  /** Controls that act on the whole panel, at the end of the title's line. */
  right?: ReactNode;
  id?: string;
  /** Whether a rule runs under the title, for a panel whose content has no frame to draw one. */
  isRuled?: boolean;
  children: ReactNode;
}

/** Sets off one section of a screen: its title, what it holds beside the title, then its content. Chips and controls overhang the title's line instead of pushing the content down, so the content starts at one height with or without them. */
export function Panel({
  title,
  meta,
  beside,
  note,
  right,
  id,
  isRuled = false,
  children,
}: PanelProps) {
  return (
    // Controls overhang the title's line by --overhang on each side.
    <section
      id={id}
      className="@container flex min-w-0 scroll-mt-(--overhang) flex-col gap-sm [--overhang:calc((var(--size-control)-var(--text-h7--line-height))/2)]"
    >
      <div
        className={cn(
          "flex flex-wrap items-baseline gap-x-sm gap-y-[calc(var(--overhang)+var(--spacing-xs))]",
          isRuled && "border-b border-line pb-sm",
        )}
      >
        <h2 className="font-heading text-h7 font-semibold text-ink">{title}</h2>
        {meta === undefined ? undefined : (
          <span className="text-body-md text-ink-muted empty:hidden">{meta}</span>
        )}
        {beside === undefined ? undefined : (
          <div className="self-center @max-3xl:order-last @max-3xl:basis-full">{beside}</div>
        )}
        {note === undefined ? undefined : (
          <span className="ml-auto text-body-sm text-ink-muted max-md:ml-0">{note}</span>
        )}
        {right === undefined ? undefined : (
          <div
            className={cn(
              "-my-(--overhang) flex max-w-full min-w-0 items-center gap-md self-center text-body-sm text-ink-muted",
              note === undefined && "ml-auto",
            )}
          >
            {right}
          </div>
        )}
      </div>
      {children}
    </section>
  );
}

export interface PairsProps {
  children: ReactNode;
}

export function Pairs({ children }: PairsProps) {
  return (
    <dl className="grid grid-cols-[120px_minmax(0,1fr)] gap-x-md gap-y-xs text-body-md">
      {children}
    </dl>
  );
}

export interface PairProps {
  label: ReactNode;
  children: ReactNode;
}

export function Pair({ label, children }: PairProps) {
  return (
    <>
      <dt className="text-ink-muted">{label}</dt>
      <dd className="text-ink wrap-anywhere">{children}</dd>
    </>
  );
}
