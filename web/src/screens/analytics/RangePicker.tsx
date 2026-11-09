import { useStatsQuery } from "@/api/queries";
import { ChevronDownIcon, ClockIcon } from "@/components/icons";
import { Menu, MenuPopup, MenuRadioGroup, MenuRadioItem, MenuTrigger } from "@/components/ui/menu";
import { SELECT_TRIGGER } from "@/components/ui/select-trigger";
import { fmtClock, fmtDate } from "@/lib/format";

import { RANGE_OPTIONS, useAnalyticsView } from "./analytics-range";
import { zoomLabelOf } from "./bucket-names";

export function RangePicker() {
  const [view, actions] = useAnalyticsView();
  const since = useStatsQuery(view.range, view.zoom).data?.since;
  const rangeLabel = RANGE_OPTIONS.find((option) => option.value === view.range)?.label;

  return (
    <Menu>
      <MenuTrigger
        className={SELECT_TRIGGER}
        title={
          view.zoom !== null
            ? `Zoomed in from ${rangeLabel}`
            : since == null
              ? undefined
              : `Since ${fmtDate(since)}, ${fmtClock(since)}`
        }
      >
        <ClockIcon className="text-ink-muted" />
        {view.zoom === null ? rangeLabel : zoomLabelOf(view.zoom)}
        <ChevronDownIcon />
      </MenuTrigger>
      <MenuPopup side="bottom" align="end">
        <MenuRadioGroup value={view.range}>
          {RANGE_OPTIONS.map((option) => (
            <MenuRadioItem
              key={option.value}
              value={option.value}
              onClick={() => actions.pickRange(option.value)}
              closeOnClick
            >
              {option.label}
            </MenuRadioItem>
          ))}
        </MenuRadioGroup>
      </MenuPopup>
    </Menu>
  );
}
