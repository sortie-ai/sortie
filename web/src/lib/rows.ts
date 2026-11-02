import { cn } from "@/lib/cn";

// From 768 px the rows share the list's columns, so status and time cells line up.
// The issue column keeps its width while the room after the status can give way, so the status stays put.
/** Classes of a list whose rows hold a mark, the issue, the status, and a time. */
export const ROW_LIST =
  "divide-y divide-line overflow-hidden rounded-xl border border-line bg-canvas md:grid md:grid-cols-[auto_minmax(0,var(--container-xl))_minmax(max-content,1fr)_max-content]";

/** Classes of one row of a {@link ROW_LIST}. */
export const ROW_GRID =
  "grid grid-cols-[auto_minmax(0,1fr)] gap-x-sm gap-y-xxs max-md:gap-y-xs px-md py-sm md:col-span-full md:grid-cols-subgrid md:gap-x-md wrap-anywhere";

/** Classes of the edge that marks a row whose issue needs a person. */
export const ATTENTION_EDGE = "shadow-[inset_1px_0_0_var(--color-warning-fill)]";

export function rowStyles(isSelected: boolean, isOpenable: boolean, needsPerson = false): string {
  return cn(
    "scroll-mt-20 scroll-mb-md transition-colors duration-(--duration-feedback)",
    // A row names its fill as the ground too, so a lane's marks fill with it.
    isSelected
      ? "bg-item-selected [--ground:var(--color-item-selected)]"
      : "hover:bg-item-hover hover:[--ground:var(--color-item-hover)] active:bg-item-selected active:[--ground:var(--color-item-selected)]",
    needsPerson ? ATTENTION_EDGE : isSelected && "shadow-[inset_2px_0_0_var(--color-line-active)]",
    isOpenable && "cursor-pointer",
  );
}

/** Gives the color of a row's second lines, raised while the row is selected so they keep their contrast. */
export function secondLineColor(isSelected: boolean): string {
  return isSelected ? "text-ink-body" : "text-ink-muted";
}
