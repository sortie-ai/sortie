import { useMatch } from "react-router";

import { Kbd } from "@/app/Kbd";
import { modifierLabel } from "@/app/keys";
import { Dialog, DialogPopup, DialogTitle } from "@/components/ui/dialog";

import type { ScreenEntry } from "@/app/Rail";

interface Shortcut {
  label: string;
  keys: string[];
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

function everywhereShortcuts(screens: ScreenEntry[]): Shortcut[] {
  return [
    { label: "Refresh now", keys: ["R"] },
    { label: "Pause or resume live updates", keys: ["P"] },
    { label: "Mark everything read", keys: ["Shift", "U"] },
    { label: "Next theme", keys: ["T"] },
    ...screens.map((screen) => ({
      label: `Go to ${screen.label}`,
      keys: ["G", screen.key.toUpperCase()],
    })),
    { label: "Keyboard shortcuts", keys: ["?"] },
    { label: "Command palette", keys: [modifierLabel(), "K"] },
    { label: "Close, go back", keys: ["Esc"] },
  ];
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
  const shortcuts = [
    ...(isOverview ? OVERVIEW_SHORTCUTS : []),
    ...(isPipeline ? PIPELINE_SHORTCUTS : []),
    ...(isIssue ? ISSUE_SHORTCUTS : []),
    ...everywhereShortcuts(screens),
  ];

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogPopup>
        <div className="overflow-y-auto p-lg">
          <DialogTitle className="mb-md">Keyboard shortcuts</DialogTitle>
          <ul className="grid grid-cols-2 gap-x-xl gap-y-xs max-md:grid-cols-1">
            {shortcuts.map((shortcut) => (
              <li
                key={shortcut.label}
                className="flex items-center justify-between gap-sm border-b border-line py-xxs text-body-md"
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
        </div>
      </DialogPopup>
    </Dialog>
  );
}
