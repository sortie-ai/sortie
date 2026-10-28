import { Combobox as BaseCombobox } from "@base-ui/react/combobox";

import { CheckIcon, SearchIcon } from "@/components/icons";
import { cn } from "@/lib/cn";

import type { ReactNode } from "react";

export type ComboboxProps<Value> = Omit<
  BaseCombobox.Root.Props<Value, false, Value>,
  "open" | "inline" | "multiple"
>;

/** Narrow the list through `filteredItems` and `inputValue` when the match rule is not a plain substring. */
export function Combobox<Value>(props: ComboboxProps<Value>) {
  return <BaseCombobox.Root open inline {...props} />;
}

export type ComboboxInputProps = Omit<BaseCombobox.Input.Props, "className">;

export function ComboboxInput(props: ComboboxInputProps) {
  return (
    <div className="flex items-center gap-xs border-b border-line bg-field px-md text-ink-muted">
      <SearchIcon />
      <BaseCombobox.Input
        className="h-10 w-full min-w-0 bg-transparent text-body-md text-ink outline-none placeholder:text-ink-muted"
        {...props}
      />
    </div>
  );
}

export type ComboboxListProps = Omit<BaseCombobox.List.Props, "className">;

export function ComboboxList(props: ComboboxListProps) {
  return (
    <BaseCombobox.List
      className="max-h-80 min-h-0 overflow-y-auto overscroll-contain py-xxs scroll-py-xxs"
      {...props}
    />
  );
}

export type ComboboxGroupProps = Omit<BaseCombobox.Group.Props, "className">;

export function ComboboxGroup(props: ComboboxGroupProps) {
  return <BaseCombobox.Group {...props} />;
}

export interface ComboboxGroupLabelProps {
  children: ReactNode;
}

export function ComboboxGroupLabel({ children }: ComboboxGroupLabelProps) {
  return (
    <BaseCombobox.GroupLabel className="px-md pt-xs pb-xxs text-label-caps font-medium text-ink-muted">
      {children}
    </BaseCombobox.GroupLabel>
  );
}

export interface ComboboxItemProps extends Omit<BaseCombobox.Item.Props, "className"> {
  /** Indents the option under the option above it, as a stage under its chain. */
  indented?: boolean;
}

export function ComboboxItem({ indented = false, children, ...rest }: ComboboxItemProps) {
  return (
    <BaseCombobox.Item
      className={cn(
        "flex min-h-8 cursor-default items-center gap-xs py-xxs pr-md text-body-md text-ink outline-none select-none data-highlighted:bg-item-hover active:bg-item-selected",
        indented ? "pl-xl" : "pl-md",
      )}
      {...rest}
    >
      <span className="flex w-(--size-icon) shrink-0 text-ink-muted">
        <BaseCombobox.ItemIndicator>
          <CheckIcon strokeWidth={2.5} />
        </BaseCombobox.ItemIndicator>
      </span>
      <span className="flex min-w-0 flex-1 items-baseline justify-between gap-md">{children}</span>
    </BaseCombobox.Item>
  );
}

export interface ComboboxEmptyProps {
  children: ReactNode;
}

export function ComboboxEmpty({ children }: ComboboxEmptyProps) {
  return (
    <BaseCombobox.Empty className="px-md py-lg text-body-md text-ink-muted empty:hidden">
      {children}
    </BaseCombobox.Empty>
  );
}
