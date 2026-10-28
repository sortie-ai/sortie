import { cn } from "@/lib/cn";

export function rowStyles(isSelected: boolean, isOpenable: boolean): string {
  return cn(
    "scroll-mt-20 scroll-mb-md transition-colors duration-(--duration-feedback)",
    isSelected
      ? "bg-item-selected shadow-[inset_2px_0_0_var(--color-line-active)]"
      : "hover:bg-item-hover active:bg-item-selected",
    isOpenable && "cursor-pointer",
  );
}

/** Gives the color of a row's second lines, raised while the row is selected so they keep their contrast. */
export function secondLineColor(isSelected: boolean): string {
  return isSelected ? "text-ink-body" : "text-ink-muted";
}
