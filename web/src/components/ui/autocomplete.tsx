import { Autocomplete as BaseAutocomplete } from "@base-ui/react/autocomplete";

import { SearchIcon, XIcon } from "@/components/icons";
import { cn } from "@/lib/cn";

import type { ReactNode } from "react";

/** The filter a query is matched with: substring, ignoring case and accents. */
export const useAutocompleteFilter = BaseAutocomplete.useFilter;

/** A captioned run of items; `value` is the caption. */
export interface AutocompleteGroupData<ItemValue> {
  value: string;
  items: readonly ItemValue[];
}

export type AutocompleteProps<ItemValue> = Omit<
  BaseAutocomplete.Root.Props<ItemValue>,
  "open" | "inline" | "autoHighlight" | "keepHighlight" | "items"
> & {
  items: readonly AutocompleteGroupData<ItemValue>[];
};

/** A list that is always open and renders in place, as a command palette does inside a dialog; the first match stays highlighted, so `Enter` runs it. */
export function Autocomplete<ItemValue>(props: AutocompleteProps<ItemValue>) {
  return <BaseAutocomplete.Root open inline autoHighlight="always" keepHighlight {...props} />;
}

export type AutocompleteInputProps = Omit<BaseAutocomplete.Input.Props, "className">;

type InputKeyDownEvent = Parameters<NonNullable<AutocompleteInputProps["onKeyDown"]>>[0];

const TEXT_SELECTION_KEYS = new Set(["Home", "End"]);

/** Leaves Shift+Home and Shift+End to the field, which the inline list takes as jumps otherwise. */
function keepTextSelection(event: InputKeyDownEvent) {
  if (event.shiftKey && TEXT_SELECTION_KEYS.has(event.key)) {
    event.preventBaseUIHandler();
  }
}

export function AutocompleteInput(props: AutocompleteInputProps) {
  return (
    <BaseAutocomplete.InputGroup className="flex items-center gap-xs border-b border-line bg-field px-md text-ink-muted">
      <SearchIcon />
      <BaseAutocomplete.Input
        className="h-10 w-full min-w-0 bg-transparent text-body-md text-ink outline-none placeholder:text-ink-muted"
        onKeyDown={keepTextSelection}
        {...props}
      />
      <BaseAutocomplete.Clear
        aria-label="Clear search"
        className="-mr-xxs inline-grid size-6 shrink-0 cursor-pointer place-items-center rounded text-ink-muted hover:text-ink focus-visible:outline-2 focus-visible:outline-focus-ring"
      >
        <XIcon />
      </BaseAutocomplete.Clear>
    </BaseAutocomplete.InputGroup>
  );
}

export type AutocompleteListProps = Omit<BaseAutocomplete.List.Props, "className">;

export function AutocompleteList(props: AutocompleteListProps) {
  return (
    <BaseAutocomplete.List
      className="min-h-0 overflow-y-auto overscroll-contain pt-xxs pb-xs scroll-py-xxs empty:hidden"
      {...props}
    />
  );
}

export interface AutocompleteGroupProps<ItemValue> {
  items: readonly ItemValue[];
  children: ReactNode;
}

export function AutocompleteGroup<ItemValue>(props: AutocompleteGroupProps<ItemValue>) {
  return <BaseAutocomplete.Group {...props} />;
}

export interface AutocompleteGroupLabelProps {
  children: ReactNode;
}

export function AutocompleteGroupLabel({ children }: AutocompleteGroupLabelProps) {
  return (
    <BaseAutocomplete.GroupLabel className="mx-md pt-xs pb-xxs text-label-caps font-medium text-ink-muted [[role=group]:not(:first-child)>&]:mt-xs [[role=group]:not(:first-child)>&]:border-t [[role=group]:not(:first-child)>&]:border-line">
      {children}
    </BaseAutocomplete.GroupLabel>
  );
}

export interface AutocompleteCollectionProps<ItemValue> {
  children: (item: ItemValue, index: number) => ReactNode;
}

export function AutocompleteCollection<ItemValue>({
  children,
}: AutocompleteCollectionProps<ItemValue>) {
  return <BaseAutocomplete.Collection>{children}</BaseAutocomplete.Collection>;
}

export interface AutocompleteItemProps extends Omit<BaseAutocomplete.Item.Props, "className"> {
  className?: string;
}

export function AutocompleteItem({ className, ...rest }: AutocompleteItemProps) {
  return (
    <BaseAutocomplete.Item
      className={cn(
        "flex min-h-8 cursor-default items-center justify-between gap-md px-md py-xxs text-body-md text-ink outline-none select-none data-highlighted:bg-item-hover active:bg-item-selected",
        className,
      )}
      {...rest}
    />
  );
}

export interface AutocompleteEmptyProps {
  title: string;
  children: ReactNode;
}

export function AutocompleteEmpty({ title, children }: AutocompleteEmptyProps) {
  return (
    <BaseAutocomplete.Empty className="pt-xxs pb-xs empty:hidden">
      <div className="mx-md pt-xs pb-xxs text-label-caps font-medium text-ink-muted">{title}</div>
      <p className="flex min-h-8 items-center px-md py-xxs text-body-md text-ink-muted">
        {children}
      </p>
    </BaseAutocomplete.Empty>
  );
}
