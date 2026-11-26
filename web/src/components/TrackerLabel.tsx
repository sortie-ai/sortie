import { cn } from "@/lib/cn";

/** The shape every tracker label takes, a state included; its colors come on top. */
export const LABEL_SHAPE =
  "inline-flex h-5 max-w-full items-center rounded-full border px-xs align-top text-body-sm whitespace-nowrap";

export interface TrackerLabelProps {
  children: string;
}

export function TrackerLabel({ children }: TrackerLabelProps) {
  return (
    <span className={cn(LABEL_SHAPE, "border-line-medium bg-canvas text-ink-body")}>
      <span className="truncate">{children}</span>
    </span>
  );
}
