import { SCREEN_STANDFIRST, SCREEN_TITLE } from "@/components/ui/page-title";
import { cn } from "@/lib/cn";

import type { ReactNode } from "react";

export interface ScreenHeaderProps {
  title: string;
  /** What the screen shows now, in a sentence; the header keeps its place while it is missing. */
  standfirst?: ReactNode;
  /** Controls that act on the whole screen, at the end of the title's line. */
  actions?: ReactNode;
  className?: string;
}

/** Heads every screen, a detail page included: its name, a standfirst under it, and its controls beside the name. A name too long for its line ends in an ellipsis on a wide screen and wraps below that, with the controls under it. */
export function ScreenHeader({ title, standfirst, actions, className }: ScreenHeaderProps) {
  return (
    <header className={cn("mb-head flex flex-col gap-md", className)}>
      {/* The line is as tall as a control at every width, so the title does not move when controls come or go;
          the margin takes back the room that line holds around the title's capitals, and the title starts a fixed step below the line's top, wrapped or not. */}
      <div className="-my-xxs flex min-h-(--size-control) items-start justify-between gap-x-md gap-y-lg max-lg:flex-wrap">
        <h1
          title={title}
          className={cn(
            "trim-caps mt-xxs min-w-0 lg:overflow-x-clip lg:text-ellipsis lg:whitespace-nowrap",
            SCREEN_TITLE,
          )}
        >
          {title}
        </h1>
        {actions === undefined ? undefined : (
          <div className="flex shrink-0 items-center">{actions}</div>
        )}
      </div>
      {/* A whole-pixel height keeps what follows the header on the spacing grid. */}
      <p className={cn("trim-caps min-h-[round(up,1cap,1px)]", SCREEN_STANDFIRST)}>{standfirst}</p>
    </header>
  );
}
