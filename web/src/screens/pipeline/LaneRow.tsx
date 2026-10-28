import { Collapsible } from "@base-ui/react/collapsible";

import { ChevronDownIcon } from "@/components/icons";
import { Lane } from "@/components/Lane";
import { cn } from "@/lib/cn";
import { plural, pluralVerb } from "@/lib/format";
import { rowStyles, secondLineColor } from "@/lib/rows";

import { laneNavKey } from "./lanes";

import type { Station } from "@/lib/lane";
import type { PipelineLane } from "./lanes";

const GROUP_KEY_LIMIT = 12;

function laneGridStyles(): string {
  return "grid grid-cols-[minmax(0,1fr)_auto] [grid-template-areas:'ident_snt'_'trk_trk'] items-baseline gap-x-lg px-md py-xs max-md:grid-cols-[minmax(0,1fr)] max-md:[grid-template-areas:'ident'_'trk'_'snt']";
}

export interface LaneRowProps {
  lane: PipelineLane;
  variant: "lane" | "dense";
  isSelected: boolean;
  /** Whether the selected-row look shows, which it does from the first navigation key on. */
  showSelection: boolean;
  onOpen: (lane: PipelineLane) => void;
}

export function LaneRow({ lane, variant, isSelected, showSelection, onOpen }: LaneRowProps) {
  const isLook = isSelected && showSelection;
  const isNeeds = lane.marker === "needs";
  const hasTitle = lane.title !== null;
  const heading = hasTitle ? `${lane.displayKey}, ${lane.title}` : lane.displayKey;

  return (
    <li
      data-nav-key={laneNavKey(lane.issueId)}
      aria-label={`${heading}. ${lane.statusTextFull}`}
      onClick={() => onOpen(lane)}
      className={cn(
        laneGridStyles(),
        "scroll-mt-28! border-b border-line wrap-anywhere",
        rowStyles(isLook && !isNeeds, true),
        isNeeds && "shadow-[inset_2px_0_0_var(--color-warning-fill)]",
        isNeeds && isLook && "bg-item-selected",
      )}
    >
      <div className="flex min-w-0 flex-wrap items-baseline gap-x-sm [grid-area:ident]">
        <span className="text-body-md font-medium text-ink">{lane.title ?? lane.displayKey}</span>
        {hasTitle ? (
          <span className={cn("text-body-sm tabular-nums", secondLineColor(isLook))}>
            {lane.displayKey}
          </span>
        ) : undefined}
        {lane.route === null ? undefined : (
          <span className={cn("font-mono text-body-sm", secondLineColor(isLook))}>
            {lane.route}
          </span>
        )}
        {lane.agentKind === null ? undefined : (
          <span className={cn("text-body-sm", secondLineColor(isLook))}>{lane.agentKind}</span>
        )}
      </div>
      <span
        className={cn(
          "w-32 truncate text-body-md font-medium [grid-area:snt]",
          isNeeds ? "text-warning" : "text-ink",
        )}
      >
        {lane.word}
      </span>
      <div className="min-w-0 [grid-area:trk]">
        <Lane
          stations={lane.stations}
          position={lane.position}
          progress={lane.progress}
          live={lane.live}
          variant={variant}
          pullRequestNumber={lane.pullRequestNumber}
        />
      </div>
    </li>
  );
}

export interface StationGroupProps {
  station: Station;
  lanes: readonly PipelineLane[];
  isOpen: boolean;
  onOpenChange: (isOpen: boolean) => void;
  selectedKey: string | undefined;
  showSelection: boolean;
  onOpenLane: (lane: PipelineLane) => void;
}

export function StationGroup({
  station,
  lanes,
  isOpen,
  onOpenChange,
  selectedKey,
  showSelection,
  onOpenLane,
}: StationGroupProps) {
  const needing = lanes.filter((lane) => lane.marker === "needs").length;
  const needsWord = pluralVerb(needing, ["issue", "issues"], ["needs", "need"]);

  return (
    <li>
      <Collapsible.Root open={isOpen} onOpenChange={onOpenChange}>
        <Collapsible.Trigger
          className={cn(
            laneGridStyles(),
            "w-full cursor-pointer border-b border-line text-left transition-colors duration-(--duration-feedback) hover:bg-item-hover focus-visible:outline-2 focus-visible:-outline-offset-2 focus-visible:outline-focus-ring active:bg-item-selected",
            needing > 0 && "shadow-[inset_2px_0_0_var(--color-warning-fill)]",
          )}
        >
          <span className="flex min-w-0 flex-wrap items-baseline gap-x-sm [grid-area:ident]">
            <ChevronDownIcon
              className={cn(
                "self-center transition-transform duration-(--duration-feedback)",
                !isOpen && "-rotate-90",
              )}
            />
            <span className="font-medium text-ink">{station.label}</span>
            <span className="text-body-sm text-ink-muted">
              {plural(lanes.length, "issue", "issues")}
              {needing > 0 ? `, ${needsWord.count} ${needsWord.verb} attention` : undefined}
            </span>
          </span>
          <span className="flex flex-wrap gap-x-xs gap-y-xxs py-xxs text-body-sm text-ink-body tabular-nums [grid-area:trk]">
            {lanes.slice(0, GROUP_KEY_LIMIT).map((lane) => (
              <span key={lane.issueId}>{lane.displayKey}</span>
            ))}
            {lanes.length > GROUP_KEY_LIMIT ? (
              <span className="text-ink-muted">{`+${lanes.length - GROUP_KEY_LIMIT}`}</span>
            ) : undefined}
          </span>
          <span className="text-right text-body-sm text-ink-muted [grid-area:snt] max-md:text-left">
            {isOpen ? "Hide lanes" : "Show lanes"}
          </span>
        </Collapsible.Trigger>
        <Collapsible.Panel>
          <ul>
            {lanes.map((lane) => (
              <LaneRow
                key={lane.issueId}
                lane={lane}
                variant="dense"
                isSelected={selectedKey === laneNavKey(lane.issueId)}
                showSelection={showSelection}
                onOpen={onOpenLane}
              />
            ))}
          </ul>
        </Collapsible.Panel>
      </Collapsible.Root>
    </li>
  );
}
