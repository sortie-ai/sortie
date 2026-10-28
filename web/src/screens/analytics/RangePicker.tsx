import { useStatsQuery } from "@/api/queries";
import { ChevronDownIcon, ClockIcon } from "@/components/icons";
import { Menu, MenuPopup, MenuRadioGroup, MenuRadioItem, MenuTrigger } from "@/components/ui/menu";
import { SELECT_TRIGGER } from "@/components/ui/select-trigger";
import { cn } from "@/lib/cn";
import { fmtClock, fmtDate } from "@/lib/format";

import { RANGE_OPTIONS, useAnalyticsRange } from "./analytics-range";

export interface RangePickerProps {
  className?: string;
}

export function RangePicker({ className }: RangePickerProps) {
  const [range, setRange] = useAnalyticsRange();
  const since = useStatsQuery(range).data?.since;
  const label = RANGE_OPTIONS.find((option) => option.value === range)?.label;

  return (
    <Menu>
      <MenuTrigger
        className={cn(SELECT_TRIGGER, className)}
        title={since == null ? undefined : `Since ${fmtDate(since)}, ${fmtClock(since)}`}
      >
        <ClockIcon className="text-ink-muted" />
        {label}
        <ChevronDownIcon />
      </MenuTrigger>
      <MenuPopup side="bottom" align="end">
        <MenuRadioGroup value={range} onValueChange={setRange}>
          {RANGE_OPTIONS.map((option) => (
            <MenuRadioItem key={option.value} value={option.value}>
              {option.label}
            </MenuRadioItem>
          ))}
        </MenuRadioGroup>
      </MenuPopup>
    </Menu>
  );
}
