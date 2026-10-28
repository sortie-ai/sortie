import { useState } from "react";

import { ChevronDownIcon } from "@/components/icons";
import {
  Combobox,
  ComboboxEmpty,
  ComboboxInput,
  ComboboxItem,
  ComboboxList,
} from "@/components/ui/combobox";
import { Popover, PopoverPopup, PopoverTrigger } from "@/components/ui/popover";
import { SELECT_TRIGGER } from "@/components/ui/select-trigger";

/** One choice of a {@link FilterMenu}; the empty value stands for "any". */
export interface FilterOption {
  value: string;
  label: string;
}

export interface FilterMenuProps {
  label: string;
  /** What the trigger shows as the chosen option; it can word a value no option carries. */
  valueText: string;
  options: readonly FilterOption[];
  /** The applied value, whether or not an option carries it. */
  value: string;
  onChange: (value: string) => void;
}

export function FilterMenu({ label, valueText, options, value, onChange }: FilterMenuProps) {
  const [isOpen, setIsOpen] = useState(false);
  const [query, setQuery] = useState("");
  const current = options.find((option) => option.value === value) ?? null;

  function handleOpenChange(open: boolean) {
    setIsOpen(open);
    setQuery("");
  }

  function handleValueChange(option: FilterOption | null) {
    if (option === null) {
      return;
    }
    onChange(option.value);
    handleOpenChange(false);
  }

  return (
    <Popover open={isOpen} onOpenChange={handleOpenChange}>
      <PopoverTrigger className={SELECT_TRIGGER}>
        {label}
        <span className="text-ink wrap-anywhere">{valueText}</span>
        <ChevronDownIcon />
      </PopoverTrigger>
      <PopoverPopup aria-label={label} className="w-72 max-w-[calc(100vw-2rem)]">
        <Combobox
          items={options}
          inputValue={query}
          onInputValueChange={(text, details) => {
            if (details.reason === "input-change" || details.reason === "input-clear") {
              setQuery(text);
            }
          }}
          value={current}
          onValueChange={handleValueChange}
          itemToStringLabel={(option) => option.label}
          isItemEqualToValue={(a, b) => a.value === b.value}
        >
          <ComboboxInput
            placeholder={`Search ${label.toLowerCase()}`}
            aria-label={`Search ${label.toLowerCase()}`}
          />
          <ComboboxList>
            {(option: FilterOption) => (
              <ComboboxItem key={option.value} value={option}>
                <span className="wrap-anywhere">{option.label}</span>
              </ComboboxItem>
            )}
          </ComboboxList>
          <ComboboxEmpty>Nothing matches "{query}".</ComboboxEmpty>
        </Combobox>
      </PopoverPopup>
    </Popover>
  );
}
