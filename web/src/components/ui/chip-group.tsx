import { Toggle } from "@base-ui/react/toggle";
import { ToggleGroup } from "@base-ui/react/toggle-group";

import { cn } from "@/lib/cn";

import type { ReactNode } from "react";

const ALL_VALUE = "__all__";

const CHIP =
  "inline-flex h-(--size-chip) items-center gap-xxs rounded-full border border-line-medium bg-surface-raised px-sm text-body-sm font-medium whitespace-nowrap text-ink-body select-none cursor-pointer transition-colors duration-(--duration-feedback) focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-focus-ring not-data-pressed:hover:bg-[linear-gradient(var(--color-state-hover),var(--color-state-hover))] not-data-pressed:active:bg-[linear-gradient(var(--color-state-pressed),var(--color-state-pressed))] data-pressed:border-primary data-pressed:bg-primary data-pressed:text-on-primary data-pressed:hover:bg-primary-hover data-pressed:active:bg-primary-active";

export interface ChipGroupItem {
  /** Never the empty string, which stands for the leading "all" chip. */
  value: string;
  label: ReactNode;
}

export interface ChipGroupProps {
  label: string;
  allLabel: ReactNode;
  items: readonly ChipGroupItem[];
  /** The chosen item's value; the empty string chooses the leading chip. */
  value: string;
  onValueChange: (value: string) => void;
  className?: string;
}

/** Pressing the chosen item again returns to "all". */
export function ChipGroup({
  label,
  allLabel,
  items,
  value,
  onValueChange,
  className,
}: ChipGroupProps) {
  const chosen = value === "" ? ALL_VALUE : value;

  function handleValueChange(next: string[]) {
    const picked = next.find((entry) => entry !== chosen);
    onValueChange(picked === undefined || picked === ALL_VALUE ? "" : picked);
  }

  return (
    <ToggleGroup
      aria-label={label}
      value={[chosen]}
      onValueChange={handleValueChange}
      className={cn("flex flex-wrap items-center gap-xs", className)}
    >
      <Toggle value={ALL_VALUE} className={CHIP}>
        {allLabel}
      </Toggle>
      {items.map((item) => (
        <Toggle key={item.value} value={item.value} className={CHIP}>
          {item.label}
        </Toggle>
      ))}
    </ToggleGroup>
  );
}
