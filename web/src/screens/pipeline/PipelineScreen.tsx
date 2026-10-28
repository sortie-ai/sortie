import { useEffect, useMemo, useState } from "react";
import { Link, useLocation, useNavigate, useSearchParams } from "react-router";

import { useEndedQuery, useStateQuery } from "@/api/queries";
import { useHold, useNow } from "@/app/live";
import { useShortcuts } from "@/app/keys";
import { ActiveHead } from "@/components/ActiveHead";
import { PhaseFilter } from "@/components/PhaseFilter";
import { RuleFilter } from "@/components/RuleFilter";
import { StatusMark } from "@/components/StatusMark";
import { Button } from "@/components/ui/button";
import { useAttentionActions } from "@/components/use-attention-actions";
import { useOpenLatestSession } from "@/components/use-open-session";
import { usePeek } from "@/components/use-peek";
import { filterRows, isPhase } from "@/lib/active-rows";
import { cn } from "@/lib/cn";
import { plural, pluralVerb } from "@/lib/format";
import { issueLinkOf } from "@/lib/issue-link";
import { stationsOf } from "@/lib/lane";
import { localMidnight } from "@/lib/server-clock";

import { StationGroup, LaneRow } from "./LaneRow";
import { PIPELINE_COMPRESS_AFTER, laneNavKey, pipelineLanes, stationTally } from "./lanes";
import { StationHeader } from "./StationHeader";

import type { AttentionItemResponse, StateResponse } from "@/api/types.gen";
import type { Binding } from "@/app/keys";
import type { PipelineLane } from "./lanes";
import type { Station } from "@/lib/lane";

const LINK =
  "text-link underline underline-offset-2 hover:text-link-hover focus-visible:outline-2 focus-visible:outline-focus-ring active:text-link-active";

interface AttentionLineProps {
  items: readonly AttentionItemResponse[];
}

function AttentionLine({ items }: AttentionLineProps) {
  const location = useLocation();
  if (items.length === 0) {
    // Holds the line's place so the lanes do not shift when attention comes and goes.
    return <div aria-hidden="true" className="mb-lg min-h-lh text-body-md" />;
  }
  const firstOfIssue = new Map<string, AttentionItemResponse>();
  for (const item of items) {
    const identifier = item.issue_identifier;
    if (identifier !== undefined && identifier !== "" && !firstOfIssue.has(identifier)) {
      firstOfIssue.set(identifier, item);
    }
  }
  const agreement = pluralVerb(items.length, ["item", "items"], ["needs", "need"]);

  return (
    <p className="mb-lg flex flex-wrap items-baseline gap-x-md gap-y-xs">
      <StatusMark
        mark="needs"
        label={`${agreement.count} ${agreement.noun} ${agreement.verb} attention`}
      />
      <span className="inline-flex flex-wrap gap-sm">
        {[...firstOfIssue].map(([identifier, item]) => {
          const link = issueLinkOf(identifier, location);
          return (
            <Link
              key={identifier}
              to={link.to}
              state={link.state}
              title={item.reason_text}
              className="text-link tabular-nums hover:text-link-hover hover:underline hover:underline-offset-2 focus-visible:outline-2 focus-visible:outline-focus-ring active:text-link-active"
            >
              {item.display_identifier || identifier}
            </Link>
          );
        })}
      </span>
      <Link to="/" className={LINK}>
        Open Overview
      </Link>
    </p>
  );
}

interface LegendProps {
  hasReactions: boolean;
  hasChains: boolean;
}

function Legend({ hasReactions, hasChains }: LegendProps) {
  const swatch = "inline-block size-3 rounded-full";
  return (
    <div className="flex flex-wrap gap-md px-md pt-sm text-body-sm text-ink-muted">
      <span className="inline-flex items-center gap-xs">
        <i aria-hidden="true" className={cn(swatch, "bg-ink-body")} />
        passed
      </span>
      <span className="inline-flex items-center gap-xs">
        <i
          aria-hidden="true"
          className={cn(swatch, "border-[1.5px] border-dashed border-ink-body")}
        />
        waiting
      </span>
      <span className="inline-flex items-center gap-xs">
        <i aria-hidden="true" className={cn(swatch, "border-[1.5px] border-ink-muted")} />
        retry scheduled
      </span>
      <span className="inline-flex items-center gap-xs">
        <i
          aria-hidden="true"
          className={cn(swatch, "border-[1.5px] border-warning bg-warning-container")}
        />
        needs attention
      </span>
      <span className="inline-flex items-center gap-xs">
        <i
          aria-hidden="true"
          className={cn(
            swatch,
            "border-2 border-ink-body bg-[radial-gradient(circle,var(--color-ink-body)_0_2.5px,var(--color-canvas)_2.5px)]",
          )}
        />
        ended
      </span>
      {hasReactions ? (
        <span className="inline-flex items-center gap-xs">
          <i
            aria-hidden="true"
            className={cn(swatch, "border-[1.5px] border-ink-faint bg-canvas")}
          />
          no record
        </span>
      ) : undefined}
      {hasChains ? (
        <span className="inline-flex items-center gap-xs">
          <i
            aria-hidden="true"
            className="inline-block h-1 w-6 rounded-full bg-[linear-gradient(to_right,var(--color-ink-body)_60%,var(--color-ink-faint)_60%)]"
          />
          stages done
        </span>
      ) : undefined}
    </div>
  );
}

interface NavItem {
  key: string;
  identifier: string;
}

function renderedLanesOf(
  visible: readonly PipelineLane[],
  stations: readonly Station[],
  isGrouped: boolean,
  openGroups: ReadonlySet<string>,
): PipelineLane[] {
  if (!isGrouped) {
    return [...visible];
  }
  return stations
    .filter((station) => openGroups.has(station.key))
    .flatMap((station) => visible.filter((lane) => lane.station === station.key));
}

interface PipelineBodyProps {
  state: StateResponse;
}

function PipelineBody({ state }: PipelineBodyProps) {
  const navigate = useNavigate();
  const location = useLocation();
  const [params, setParams] = useSearchParams();
  const { identifier: peekIdentifier, open, follow, close } = usePeek();
  const actions = useAttentionActions();
  const openLatestSession = useOpenLatestSession();
  const now = useNow();
  const ended = useEndedQuery(localMidnight(now).toISOString());
  const [selection, setSelection] = useState({ key: null as string | null, index: 0 });
  const [showSelection, setShowSelection] = useState(false);
  const [openGroups, setOpenGroups] = useState<ReadonlySet<string>>(new Set());

  const isHeld = peekIdentifier !== null;
  const view = useHold({ state, pages: ended.data, now }, isHeld);
  const config = view.state.config;
  const phase = params.get("phase");
  const rule = params.get("rule");

  const lanes = useMemo(
    () => pipelineLanes(view.state, view.pages, view.now),
    [view.state, view.pages, view.now],
  );
  const stations = useMemo(() => (config === undefined ? [] : stationsOf(config)), [config]);
  const unacknowledged = useMemo(
    () => (view.state.attention ?? []).filter((item) => item.acknowledged_at === null),
    [view.state],
  );
  const ruleLanes = useMemo(() => filterRows(lanes, null, rule, config), [lanes, rule, config]);
  const visible = useMemo(
    () => filterRows(lanes, phase, rule, config),
    [lanes, phase, rule, config],
  );
  const tally = useMemo(() => stationTally(visible), [visible]);

  const isCompressed = lanes.length > PIPELINE_COMPRESS_AFTER;
  const isGrouped = isCompressed && !isPhase(phase) && (rule === null || rule === "");
  const rendered = useMemo(
    () => renderedLanesOf(visible, stations, isGrouped, openGroups),
    [visible, stations, isGrouped, openGroups],
  );
  const navItems = useMemo<NavItem[]>(
    () => rendered.map((lane) => ({ key: laneNavKey(lane.issueId), identifier: lane.identifier })),
    [rendered],
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

  function handleOpenLane(lane: PipelineLane) {
    const key = laneNavKey(lane.issueId);
    setSelection({
      key,
      index: Math.max(
        0,
        navItems.findIndex((item) => item.key === key),
      ),
    });
    open(lane.identifier);
  }

  function handleMove(delta: 1 | -1) {
    setShowSelection(true);
    if (navItems.length === 0) {
      return;
    }
    const next =
      selectedIndex < 0 ? (delta === 1 ? 0 : navItems.length - 1) : selectedIndex + delta;
    const item = navItems[next];
    if (item === undefined) {
      return;
    }
    setSelection({ key: item.key, index: next });
    if (isHeld) {
      follow(item.identifier);
    }
  }

  function handleTogglePeek() {
    if (isHeld) {
      close();
    } else if (selected !== undefined) {
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

  function peekedItem(): AttentionItemResponse | undefined {
    return isHeld
      ? (view.state.attention ?? []).find(
          (item) => item.issue_identifier === peekIdentifier && item.acknowledged_at === null,
        )
      : undefined;
  }

  const bindings: Binding[] = [
    { keys: ["j", "ArrowDown"], run: () => handleMove(1) },
    { keys: ["k", "ArrowUp"], run: () => handleMove(-1) },
    { keys: [" "], run: handleTogglePeek },
    { keys: ["Enter"], run: handleOpenPage },
    { keys: ["o"], run: handleOpenSession },
    {
      keys: ["a"],
      run: () => {
        const item = peekedItem();
        if (item !== undefined) {
          actions.runAction(item);
        }
      },
    },
    {
      keys: ["e"],
      run: () => {
        const item = peekedItem();
        if (item !== undefined) {
          void actions.acknowledge(item);
        }
      },
    },
  ];
  useShortcuts(bindings);

  function handleShowAll() {
    setParams(
      (previous) => {
        const next = new URLSearchParams(previous);
        next.delete("phase");
        next.delete("rule");
        return next;
      },
      { replace: true },
    );
  }

  function handleGroupOpenChange(key: string, isOpen: boolean) {
    setOpenGroups((previous) => {
      const next = new Set(previous);
      if (isOpen) {
        next.add(key);
      } else {
        next.delete(key);
      }
      return next;
    });
  }

  const issues = plural(lanes.length, "issue", "issues");
  const meta = view.pages === undefined ? issues : `${issues}, ended today included`;

  return (
    <>
      <h1 className="mb-xs font-heading text-h5 font-semibold text-ink">Pipeline</h1>
      <AttentionLine items={unacknowledged} />
      {config === undefined ? undefined : (
        <section className="flex flex-col gap-sm">
          <ActiveHead view="pipeline" meta={meta} />
          <div className="flex flex-wrap items-center justify-between gap-xs empty:hidden">
            <PhaseFilter rows={ruleLanes} />
            <RuleFilter config={config} rows={lanes} />
          </div>
          {lanes.length === 0 ? (
            // Holds the lanes' place so the page does not shift when work starts.
            <div
              aria-hidden="true"
              className="min-h-lh border border-transparent py-lg text-body-md"
            />
          ) : visible.length === 0 ? (
            <div className="flex flex-wrap items-center gap-sm rounded-lg border border-line bg-canvas px-md py-lg text-body-md text-ink-body">
              No issue matches these filters.
              <Button onClick={handleShowAll}>Show all</Button>
            </div>
          ) : (
            <div className="-mx-md mt-xs">
              <StationHeader stations={stations} tally={tally} />
              <ul aria-label="Active issues">
                {isGrouped
                  ? stations
                      .filter((station) => tally.has(station.key))
                      .map((station) => (
                        <StationGroup
                          key={station.key}
                          station={station}
                          lanes={visible.filter((lane) => lane.station === station.key)}
                          isOpen={openGroups.has(station.key)}
                          onOpenChange={(isOpen) => handleGroupOpenChange(station.key, isOpen)}
                          selectedKey={selectedKey}
                          showSelection={showSelection}
                          onOpenLane={handleOpenLane}
                        />
                      ))
                  : visible.map((lane) => (
                      <LaneRow
                        key={lane.issueId}
                        lane={lane}
                        variant={isCompressed ? "dense" : "lane"}
                        isSelected={selectedKey === laneNavKey(lane.issueId)}
                        showSelection={showSelection}
                        onOpen={handleOpenLane}
                      />
                    ))}
              </ul>
              <Legend
                hasReactions={stations.some((station) => station.kind === "reaction")}
                hasChains={(config.dispatch.chains ?? []).some((chain) => chain.length > 0)}
              />
            </div>
          )}
        </section>
      )}
    </>
  );
}

/**
 * Renders the Pipeline: every active issue and every issue that ended today as
 * one lane across the stations of the workflow. While a peek is open it
 * renders the data as it was when the peek opened.
 */
export function PipelineScreen() {
  const { data: state } = useStateQuery();
  return state === undefined ? undefined : <PipelineBody state={state} />;
}
