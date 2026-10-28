import { useEffect, useState } from "react";
import { useLocation, useNavigate } from "react-router";

import { useIssueLookupQuery, useStateQuery } from "@/api/queries";
import { Kbd } from "@/app/Kbd";
import { useLive } from "@/app/live";
import {
  Autocomplete,
  AutocompleteCollection,
  AutocompleteEmpty,
  AutocompleteGroup,
  AutocompleteGroupLabel,
  AutocompleteInput,
  AutocompleteItem,
  AutocompleteList,
  useAutocompleteFilter,
} from "@/components/ui/autocomplete";
import { Dialog, DialogPopup } from "@/components/ui/dialog";
import { issueAwaitsApproval, routeText } from "@/lib/active-rows";
import { fmtStamp, parseInstant } from "@/lib/format";
import { issueLinkOf, sessionLinkOf } from "@/lib/issue-link";
import { issueStanding, runRouteText } from "@/lib/lane";
import { numberedRunsOf } from "@/lib/run-number";
import { runOutcome } from "@/lib/run-status";
import { setTheme } from "@/lib/theme";

import type { ScreenEntry } from "@/app/Rail";
import type {
  ActiveIssueResponse,
  AttentionItemResponse,
  ConfigResponse,
  IssueGroupResponse,
  RunningEntryResponse,
} from "@/api/types.gen";
import type { AutocompleteGroupData } from "@/components/ui/autocomplete";
import type { ThemeChoice } from "@/lib/theme";

interface Command {
  value: string;
  label: string;
  keys: string[];
  run: () => void;
}

interface IssueEntry {
  value: string;
  label: string;
  identifier: string;
  word: string;
}

interface SessionEntry {
  value: string;
  label: string;
  identifier: string;
  dispatchId: string;
  route: string | null;
  status: string;
  startedAt: string;
}

type PaletteItem = Command | IssueEntry | SessionEntry;

const ISSUES_GROUP = "Issues";
const SESSIONS_GROUP = "Sessions";
const ACTIVE_ENTRY_CAP = 8;
const SESSION_ENTRY_CAP = 8;
const FINISHED_ENTRY_CAP = 5;
const LOOKUP_DEBOUNCE_MS = 250;
const LOOKUP_FAILED = "Run history could not be read.";

function isIssueEntry(item: PaletteItem): item is IssueEntry {
  return "word" in item;
}

function isSessionEntry(item: PaletteItem): item is SessionEntry {
  return "dispatchId" in item;
}

function issueEntryOf(
  issue: ActiveIssueResponse | IssueGroupResponse,
  attention: readonly AttentionItemResponse[],
  awaitingApproval: boolean,
): IssueEntry {
  const key = issue.display_identifier || issue.issue_identifier;
  const outcome = "issue_outcome" in issue ? issue.issue_outcome : null;
  return {
    value: `issue:${issue.issue_id}`,
    label: issue.issue_title ? `${key} ${issue.issue_title}` : key,
    identifier: issue.issue_identifier,
    word: issueStanding(issue.issue_id, issue.status, outcome, attention, awaitingApproval).word,
  };
}

function matchesActive(
  issue: Pick<ActiveIssueResponse, "issue_identifier" | "display_identifier" | "issue_title">,
  query: string,
): boolean {
  const needle = query.toLowerCase();
  const key = issue.display_identifier || issue.issue_identifier;
  return (
    key.toLowerCase().includes(needle) || (issue.issue_title ?? "").toLowerCase().includes(needle)
  );
}

function liveSessionEntryOf(
  running: RunningEntryResponse,
  config: ConfigResponse | undefined,
): SessionEntry {
  const key = running.display_identifier || running.issue_identifier;
  return {
    value: `session:${running.dispatch_id}`,
    label: `${key} live session`,
    identifier: running.issue_identifier,
    dispatchId: running.dispatch_id,
    route: routeText(running, config),
    status: "Running",
    startedAt: running.started_at,
  };
}

function finishedSessionEntriesOf(
  groups: readonly IssueGroupResponse[],
  config: ConfigResponse | undefined,
): SessionEntry[] {
  return groups
    .flatMap((group) =>
      numberedRunsOf(group.runs ?? [], group.totals.run_count).flatMap(({ run, number }) =>
        run.dispatch_id ? [{ group, run, number, dispatchId: run.dispatch_id }] : [],
      ),
    )
    .sort((a, b) => (parseInstant(b.run.started_at) ?? 0) - (parseInstant(a.run.started_at) ?? 0))
    .map(({ group, run, number, dispatchId }) => ({
      value: `session:${dispatchId}`,
      label: `${group.display_identifier || group.issue_identifier} session ${number}`,
      identifier: group.issue_identifier,
      dispatchId,
      route: runRouteText(run, config),
      status: runOutcome(run).word,
      startedAt: run.started_at,
    }));
}

function useDebounced(value: string, delayMs: number): string {
  const [debounced, setDebounced] = useState(value);
  useEffect(() => {
    const timer = setTimeout(() => setDebounced(value), delayMs);
    return () => clearTimeout(timer);
  }, [value, delayMs]);
  return debounced;
}

const THEME_COMMANDS: { choice: ThemeChoice; label: string }[] = [
  { choice: "system", label: "Theme: System" },
  { choice: "light", label: "Theme: Light" },
  { choice: "dark", label: "Theme: Dark" },
];

export interface CommandPaletteProps {
  screens: ScreenEntry[];
  open: boolean;
  onOpenChange: (open: boolean) => void;
  onRefresh: () => void;
  onTogglePause: () => void;
  onMarkAllRead: () => void;
  onOpenShortcuts: () => void;
}

export function CommandPalette({
  screens,
  open,
  onOpenChange,
  onRefresh,
  onTogglePause,
  onMarkAllRead,
  onOpenShortcuts,
}: CommandPaletteProps) {
  const navigate = useNavigate();
  const location = useLocation();
  const { paused } = useLive();
  const { contains } = useAutocompleteFilter();
  const [query, setQuery] = useState("");
  const issueQuery = query.trim();
  const state = useStateQuery().data;
  const lookup = useIssueLookupQuery(useDebounced(open ? issueQuery : "", LOOKUP_DEBOUNCE_MS));

  const commands: Command[] = [
    ...screens.map((screen) => ({
      value: `go:${screen.path}`,
      label: `Go to ${screen.label}`,
      keys: ["G", screen.key.toUpperCase()],
      run: () => void navigate(screen.path),
    })),
    { value: "refresh", label: "Refresh now", keys: ["R"], run: onRefresh },
    {
      value: "pause",
      label: paused ? "Resume live updates" : "Pause live updates",
      keys: ["P"],
      run: onTogglePause,
    },
    { value: "read", label: "Mark everything read", keys: ["Shift", "U"], run: onMarkAllRead },
    ...THEME_COMMANDS.map(({ choice, label }) => ({
      value: `theme:${choice}`,
      label,
      keys: ["T"],
      run: () => setTheme(choice),
    })),
    { value: "shortcuts", label: "Keyboard shortcuts", keys: ["?"], run: onOpenShortcuts },
  ];

  const activeIssues =
    issueQuery === ""
      ? []
      : (state?.active ?? [])
          .filter((issue) => matchesActive(issue, issueQuery))
          .slice(0, ACTIVE_ENTRY_CAP);
  const activeIds = new Set(activeIssues.map((issue) => issue.issue_id));
  const finishedIssues =
    issueQuery === ""
      ? []
      : (lookup.data ?? [])
          .filter((group) => !activeIds.has(group.issue_id))
          .slice(0, FINISHED_ENTRY_CAP);
  const issueEntries = [...activeIssues, ...finishedIssues].map((issue) =>
    issueEntryOf(issue, state?.attention ?? [], issueAwaitsApproval(state, issue.issue_id)),
  );
  const sessionEntries =
    issueQuery === ""
      ? []
      : [
          ...(state?.running ?? [])
            .filter((running) => matchesActive(running, issueQuery))
            .map((running) => liveSessionEntryOf(running, state?.config)),
          ...finishedSessionEntriesOf(lookup.data ?? [], state?.config),
        ].slice(0, SESSION_ENTRY_CAP);
  const groups: AutocompleteGroupData<PaletteItem>[] = [
    { value: "Commands", items: commands },
    ...(issueEntries.length === 0 ? [] : [{ value: ISSUES_GROUP, items: issueEntries }]),
    ...(sessionEntries.length === 0 ? [] : [{ value: SESSIONS_GROUP, items: sessionEntries }]),
  ];

  const handleRun = (command: Command) => {
    onOpenChange(false);
    command.run();
  };

  const handleOpenIssue = (entry: IssueEntry) => {
    const link = issueLinkOf(entry.identifier, location);
    onOpenChange(false);
    void navigate(link.to, { state: link.state });
  };

  const handleOpenSession = (entry: SessionEntry) => {
    const link = sessionLinkOf(entry.identifier, entry.dispatchId, location);
    onOpenChange(false);
    void navigate(link.to, { state: link.state });
  };

  // Issue and session entries are already matched by key or title, or by the
  // server, so the query must not narrow them again.
  const handleFilter = (
    item: PaletteItem,
    text: string,
    itemToString?: (item: PaletteItem) => string,
  ) => isIssueEntry(item) || isSessionEntry(item) || contains(item, text, itemToString);

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogPopup aria-label="Command palette">
        <Autocomplete items={groups} value={query} onValueChange={setQuery} filter={handleFilter}>
          <AutocompleteInput aria-label="Search commands" placeholder="Search or run a command" />
          <AutocompleteList>
            {(group: AutocompleteGroupData<PaletteItem>) => (
              <AutocompleteGroup key={group.value} items={group.items}>
                {group.value === ISSUES_GROUP || group.value === SESSIONS_GROUP ? (
                  <AutocompleteGroupLabel>{group.value}</AutocompleteGroupLabel>
                ) : undefined}
                <AutocompleteCollection>
                  {(item: PaletteItem) =>
                    isSessionEntry(item) ? (
                      <AutocompleteItem
                        key={item.value}
                        value={item}
                        onClick={() => handleOpenSession(item)}
                        className="py-xs"
                      >
                        <span className="flex min-w-0 flex-col">
                          <span className="wrap-anywhere">{item.label}</span>
                          <span className="flex flex-wrap gap-x-sm text-body-sm text-ink-muted wrap-anywhere">
                            {item.route === null ? undefined : <span>{item.route}</span>}
                            <span>{item.status}</span>
                            <span className="tabular-nums">{fmtStamp(item.startedAt)}</span>
                          </span>
                        </span>
                      </AutocompleteItem>
                    ) : isIssueEntry(item) ? (
                      <AutocompleteItem
                        key={item.value}
                        value={item}
                        onClick={() => handleOpenIssue(item)}
                        className="py-xs"
                      >
                        <span className="flex min-w-0 flex-col">
                          <span className="wrap-anywhere">{item.label}</span>
                          <span className="text-body-sm text-ink-muted wrap-anywhere">
                            {item.word}
                          </span>
                        </span>
                      </AutocompleteItem>
                    ) : (
                      <AutocompleteItem
                        key={item.value}
                        value={item}
                        onClick={() => handleRun(item)}
                      >
                        <span>{item.label}</span>
                        <span className="flex gap-xxs">
                          {item.keys.map((key) => (
                            <Kbd key={key}>{key}</Kbd>
                          ))}
                        </span>
                      </AutocompleteItem>
                    )
                  }
                </AutocompleteCollection>
              </AutocompleteGroup>
            )}
          </AutocompleteList>
          {issueQuery !== "" && lookup.isError ? (
            <p className="px-md py-xs text-body-sm text-ink-muted">{LOOKUP_FAILED}</p>
          ) : undefined}
          <AutocompleteEmpty>{`Nothing matches "${query}".`}</AutocompleteEmpty>
        </Autocomplete>
      </DialogPopup>
    </Dialog>
  );
}
