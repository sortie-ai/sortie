import { useMatch } from "react-router";

import { Kbd } from "@/app/Kbd";
import { modifierLabel } from "@/app/keys";
import { Dialog, DialogPopup, DialogTitle } from "@/components/ui/dialog";
import { cn } from "@/lib/cn";

import type { ScreenEntry } from "@/app/Rail";

interface Shortcut {
  label: string;
  keys: string[];
}

interface ShortcutGroup {
  label: string;
  shortcuts: Shortcut[];
}

const OVERVIEW_SHORTCUTS: Shortcut[] = [
  { label: "Select the next, previous item", keys: ["J", "K"] },
  { label: "Peek the selected issue", keys: ["Space"] },
  { label: "Open the issue page", keys: ["Enter"] },
  { label: "Open the latest session", keys: ["O"] },
  { label: "Run the item's action", keys: ["A"] },
  { label: "Acknowledge the item", keys: ["E"] },
  { label: "Switch to Pipeline", keys: ["L"] },
];

const PIPELINE_SHORTCUTS: Shortcut[] = [
  { label: "Select the next, previous lane", keys: ["J", "K"] },
  { label: "Peek the selected issue", keys: ["Space"] },
  { label: "Open the issue page", keys: ["Enter"] },
  { label: "Open the latest session", keys: ["O"] },
  { label: "Run the peeked issue's action", keys: ["A"] },
  { label: "Acknowledge the peeked issue's item", keys: ["E"] },
  { label: "Switch to Overview", keys: ["L"] },
];

const ISSUE_SHORTCUTS: Shortcut[] = [{ label: "Open the latest session", keys: ["O"] }];

const EVERYWHERE_SHORTCUTS: Shortcut[] = [
  { label: "Refresh now", keys: ["R"] },
  { label: "Pause or resume live updates", keys: ["P"] },
  { label: "Mark everything read", keys: ["Shift", "U"] },
  { label: "Command palette", keys: [modifierLabel(), "K"] },
  { label: "Keyboard shortcuts", keys: ["?"] },
  { label: "Next theme", keys: ["T"] },
  { label: "Close, go back", keys: ["Esc"] },
];

function screenShortcuts(screens: ScreenEntry[]): Shortcut[] {
  return screens.map((screen) => ({
    label: `Go to ${screen.label}`,
    keys: ["G", screen.key.toUpperCase()],
  }));
}

export interface ShortcutsDialogProps {
  screens: ScreenEntry[];
  open: boolean;
  onOpenChange: (open: boolean) => void;
}

export function ShortcutsDialog({ screens, open, onOpenChange }: ShortcutsDialogProps) {
  const isOverview = useMatch("/") !== null;
  const isPipeline = useMatch("/pipeline") !== null;
  const isIssue = useMatch("/issues/:identifier") !== null;
  const groups: ShortcutGroup[] = [
    { label: "Navigation", shortcuts: screenShortcuts(screens) },
    { label: "Everywhere", shortcuts: EVERYWHERE_SHORTCUTS },
    ...(isOverview ? [{ label: "This page", shortcuts: OVERVIEW_SHORTCUTS }] : []),
    ...(isPipeline ? [{ label: "This page", shortcuts: PIPELINE_SHORTCUTS }] : []),
    ...(isIssue ? [{ label: "This page", shortcuts: ISSUE_SHORTCUTS }] : []),
  ];

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogPopup>
        {/* The last row's own padding counts toward the bottom edge, so the content ends as far from it as from the sides. */}
        <div className="overflow-y-auto px-md pt-sm pb-[calc(var(--spacing-md)-(var(--size-control)-var(--size-kbd))/2)]">
          <DialogTitle>Keyboard shortcuts</DialogTitle>
          {groups.map((group, index) => (
            <section key={group.label} className={cn(index > 0 && "mt-sm border-t border-line")}>
              {/* The first label sits as low as the palette's, whose input rule is one pixel tall. */}
              <h3
                className={cn(
                  "pb-xxs text-label-caps font-medium text-ink-muted",
                  index === 0 ? "pt-[calc(var(--spacing-md)+1px)]" : "pt-sm",
                )}
              >
                {group.label}
              </h3>
              <ul className="columns-2 gap-x-xl max-md:columns-1">
                {group.shortcuts.map((shortcut) => (
                  <li
                    key={shortcut.label}
                    className="flex min-h-8 break-inside-avoid items-center justify-between gap-sm py-xxs text-body-md"
                  >
                    <span>{shortcut.label}</span>
                    <span className="flex shrink-0 gap-xxs">
                      {shortcut.keys.map((key) => (
                        <Kbd key={key}>{key}</Kbd>
                      ))}
                    </span>
                  </li>
                ))}
              </ul>
            </section>
          ))}
        </div>
      </DialogPopup>
    </Dialog>
  );
}
