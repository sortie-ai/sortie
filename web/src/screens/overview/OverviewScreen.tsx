import { useIsFetching } from "@tanstack/react-query";
import { useEffect, useMemo, useState } from "react";
import { useLocation, useNavigate, useSearchParams } from "react-router";

import { ApiError } from "@/api/client";
import { useEndedQuery, useFirstRunQuery, useStateQuery, useTallyQuery } from "@/api/queries";
import { useHold, useLive, useNow } from "@/app/live";
import { useShortcuts } from "@/app/keys";
import { useAttentionActions } from "@/components/use-attention-actions";
import { useOpenLatestSession } from "@/components/use-open-session";
import { usePeek } from "@/components/use-peek";
import { usageByKind } from "@/lib/agent-kinds";
import { activeRows, filterRows } from "@/lib/active-rows";
import { hasRateSource } from "@/lib/budget";
import { condition } from "@/lib/condition";
import { buildEnded } from "@/lib/ended";
import { issueLinkOf } from "@/lib/issue-link";
import { localMidnight } from "@/lib/server-clock";
import { tallyLine } from "@/lib/tally";
import { attentionKey, endedKey, pruneRead, useReadSet } from "@/lib/unread";

import { ActiveList } from "./ActiveList";
import { AttentionList } from "./AttentionList";
import { CalmState } from "./CalmState";
import { EndedToday } from "./EndedToday";
import { Headline } from "./Headline";
import { buildHeadline } from "./headline";
import { HeldList } from "./HeldList";
import { activeNavKey, attentionNavKey, endedNavKey } from "./rows";
import { Usage } from "./Usage";

import type { AttentionItemResponse, Report, RunsResponse, StateResponse } from "@/api/types.gen";
import type { Binding } from "@/app/keys";
import type { ActiveRow } from "@/lib/active-rows";
import type { EndedEmpty } from "./EndedToday";

interface NavItem {
  key: string;
  /** The `issue_identifier` the peek opens for, or `null` for an item with no issue. */
  identifier: string | null;
  attention: AttentionItemResponse | null;
}

/** Everything Overview renders; the peek holds it still as one value. */
interface OverviewView {
  state: StateResponse;
  rows: ActiveRow[];
  pages: RunsResponse[] | undefined;
  tally: Report | undefined;
  isHistoryUnavailable: boolean;
  isFirstRun: boolean | undefined;
  readSet: ReadonlySet<string>;
  now: number;
}

function isHistoryUnavailable(error: unknown): boolean {
  return error instanceof ApiError && error.status === 503 && error.code === "history_unavailable";
}

function endedEmpty(view: OverviewView): EndedEmpty {
  if (view.isHistoryUnavailable) {
    return "unavailable";
  }
  if (view.isFirstRun === true) {
    return "first-run";
  }
  if (view.pages === undefined) {
    return "loading";
  }
  return "none";
}

interface OverviewBodyProps {
  state: StateResponse;
}

function OverviewBody({ state }: OverviewBodyProps) {
  const navigate = useNavigate();
  const location = useLocation();
  const [params] = useSearchParams();
  const { identifier: peekIdentifier, open, follow, close } = usePeek();
  const { paused } = useLive();
  const actions = useAttentionActions();
  const openLatestSession = useOpenLatestSession();
  const readSet = useReadSet();
  const now = useNow();
  const midnight = localMidnight(now).toISOString();
  const ended = useEndedQuery(midnight);
  const tally = useTallyQuery(midnight);
  const isFirstRun = useFirstRunQuery();
  const isFetching = useIsFetching() > 0;
  const [selection, setSelection] = useState({
    key: null as string | null,
    index: 0,
  });
  const [showSelection, setShowSelection] = useState(false);

  const rows = useMemo(() => activeRows(state), [state]);
  const isHeld = peekIdentifier !== null;
  const view = useHold<OverviewView>(
    {
      state,
      rows,
      pages: ended.data,
      tally: tally.data,
      isHistoryUnavailable: isHistoryUnavailable(ended.error),
      isFirstRun,
      readSet,
      now,
    },
    isHeld,
  );

  const phase = params.get("phase");
  const rule = params.get("rule");
  const config = view.state.config;
  const unacknowledged = useMemo(
    () => (view.state.attention ?? []).filter((item) => item.acknowledged_at === null),
    [view.state],
  );
  const visibleRows = useMemo(
    () => filterRows(view.rows, phase, rule, config),
    [view.rows, phase, rule, config],
  );
  const endedAll = useMemo(
    () => (view.pages === undefined ? [] : buildEnded(view.pages, view.state)),
    [view.pages, view.state],
  );
  const endedUnreadIds = useMemo(
    () =>
      new Set(
        endedAll
          .filter((issue) => !view.readSet.has(endedKey(issue.issueId, issue.outcome.observed_at)))
          .map((issue) => issue.issueId),
      ),
    [endedAll, view.readSet],
  );

  const navItems = useMemo<NavItem[]>(
    () => [
      ...unacknowledged.map((item) => ({
        key: attentionNavKey(item.id),
        identifier: item.issue_identifier || null,
        attention: item,
      })),
      ...visibleRows.map((row) => ({
        key: activeNavKey(row.issueId),
        identifier: row.identifier,
        attention: null,
      })),
      ...endedAll.map((issue) => ({
        key: endedNavKey(issue.issueId),
        identifier: issue.identifier,
        attention: null,
      })),
    ],
    [unacknowledged, visibleRows, endedAll],
  );

  const foundIndex =
    selection.key === null ? -1 : navItems.findIndex((item) => item.key === selection.key);
  const selectedIndex =
    navItems.length === 0
      ? -1
      : foundIndex >= 0
        ? foundIndex
        : Math.min(selection.index, navItems.length - 1);
  const selected = selectedIndex >= 0 ? navItems[selectedIndex] : undefined;
  const selectedKey = selected?.key;

  if (
    selection.key !== null &&
    selectedKey !== undefined &&
    (selectedKey !== selection.key || selectedIndex !== selection.index)
  ) {
    setSelection({ key: selectedKey, index: selectedIndex });
  }

  useEffect(() => {
    if (showSelection && selectedKey !== undefined) {
      document
        .querySelector(`[data-nav-key="${CSS.escape(selectedKey)}"]`)
        ?.scrollIntoView({ block: "nearest" });
    }
  }, [showSelection, selectedKey]);

  const hasEndedData = ended.data !== undefined;
  useEffect(() => {
    if (isHeld || paused || isFetching || !hasEndedData) {
      return;
    }
    const attention = (state.attention ?? []).map((item) => attentionKey(item.id));
    const endings = (ended.data === undefined ? [] : buildEnded(ended.data, state)).map((issue) =>
      endedKey(issue.issueId, issue.outcome.observed_at),
    );
    pruneRead([...attention, ...endings]);
  }, [isHeld, paused, isFetching, hasEndedData, ended.data, state]);

  function selectAndOpen(key: string, identifier: string | null) {
    const index = navItems.findIndex((item) => item.key === key);
    setSelection({ key, index: Math.max(index, 0) });
    if (identifier !== null) {
      open(identifier);
    }
  }

  function move(delta: 1 | -1) {
    setShowSelection(true);
    if (navItems.length === 0) {
      return;
    }
    let next = selectedIndex < 0 ? (delta === 1 ? 0 : navItems.length - 1) : selectedIndex + delta;
    while (isHeld && next >= 0 && next < navItems.length && navItems[next].identifier === null) {
      next += delta;
    }
    if (next < 0 || next >= navItems.length) {
      return;
    }
    const item = navItems[next];
    setSelection({ key: item.key, index: next });
    if (isHeld && item.identifier !== null) {
      follow(item.identifier);
    }
  }

  function handleTogglePeek() {
    if (isHeld) {
      close();
    } else if (selected?.identifier) {
      open(selected.identifier);
    }
  }

  function handleOpenPage() {
    const identifier = peekIdentifier ?? selected?.identifier;
    if (identifier) {
      const link = issueLinkOf(identifier, location);
      void navigate(link.to, { state: link.state });
    }
  }

  function handleOpenSession() {
    const identifier = peekIdentifier ?? selected?.identifier;
    if (identifier) {
      openLatestSession(identifier);
    }
  }

  function actionTarget(): AttentionItemResponse | undefined {
    if (isHeld) {
      if (selected?.attention && selected.attention.issue_identifier === peekIdentifier) {
        return selected.attention;
      }
      return (state.attention ?? []).find(
        (item) => item.issue_identifier === peekIdentifier && item.acknowledged_at === null,
      );
    }
    return selected?.attention ?? undefined;
  }

  const bindings: Binding[] = [
    { keys: ["j", "ArrowDown"], run: () => move(1) },
    { keys: ["k", "ArrowUp"], run: () => move(-1) },
    { keys: [" "], run: handleTogglePeek },
    { keys: ["Enter"], run: handleOpenPage },
    { keys: ["o"], run: handleOpenSession },
    {
      keys: ["a"],
      run: () => {
        const target = actionTarget();
        if (target !== undefined) {
          actions.runAction(target);
        }
      },
    },
    {
      keys: ["e"],
      run: () => {
        const target = actionTarget();
        if (target !== undefined) {
          void actions.acknowledge(target);
        }
      },
    },
  ];
  useShortcuts(bindings);

  const hasCost = hasRateSource(view.state);
  const tallySummary =
    view.tally === undefined ||
    view.pages === undefined ||
    view.isHistoryUnavailable ||
    view.isFirstRun === true
      ? undefined
      : view.tally.summary;
  const tallyParts =
    tallySummary === undefined ? undefined : tallyLine(tallySummary, endedAll.length, hasCost);

  return (
    <div className="flex flex-col gap-xl">
      {view.isFirstRun === undefined && unacknowledged.length === 0 ? undefined : (
        <Headline headline={buildHeadline(view.state, view.isFirstRun === true)} />
      )}
      {unacknowledged.length > 0 ? (
        <AttentionList
          items={unacknowledged}
          now={view.now}
          selectedKey={selectedKey}
          showSelection={showSelection}
          pending={actions.pending}
          onOpen={(item) => selectAndOpen(attentionNavKey(item.id), item.issue_identifier || null)}
          onAcknowledge={(item) => void actions.acknowledge(item)}
          onAction={actions.runAction}
        />
      ) : view.isFirstRun === false && condition(view.state) !== "degraded" ? (
        <CalmState
          allClear={condition(view.state) === "clear"}
          runningSince={(view.state.running ?? []).map((entry) => entry.started_at)}
          maxAgents={config?.agent.max_concurrent_agents}
          ended={endedAll}
          terminalStates={config?.tracker.terminal_states ?? []}
          runs={view.tally?.summary}
          now={view.now}
        />
      ) : undefined}
      <Usage
        ceiling={view.state.spend_ceiling}
        rateLimits={view.state.rate_limits}
        kinds={config ? usageByKind(config).map(([kind]) => kind) : []}
        now={view.now}
      />
      <ActiveList
        rows={view.rows}
        visibleRows={visibleRows}
        config={config}
        now={view.now}
        selectedKey={selectedKey}
        showSelection={showSelection}
        onOpen={(row) => selectAndOpen(activeNavKey(row.issueId), row.identifier)}
      />
      <HeldList rows={view.state.held ?? []} count={view.state.counts.held} now={view.now} />
      <EndedToday
        rows={endedAll}
        unreadIds={endedUnreadIds}
        empty={endedEmpty(view)}
        tally={tallyParts}
        selectedKey={selectedKey}
        showSelection={showSelection}
        onOpen={(issue) => selectAndOpen(endedNavKey(issue.issueId), issue.identifier)}
      />
    </div>
  );
}

/** While a peek is open it renders the data as it was when the peek opened. */
export function OverviewScreen() {
  const { data: state } = useStateQuery();
  return state === undefined ? undefined : <OverviewBody state={state} />;
}
