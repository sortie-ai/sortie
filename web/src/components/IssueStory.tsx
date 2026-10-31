import { Collapsible } from "@base-ui/react/collapsible";
import { useEffect, useState } from "react";
import { Link, useLocation } from "react-router";

import { useHistoryRunsQuery, useStateQuery } from "@/api/queries";
import { useNow } from "@/app/live";
import { AgentStatement } from "@/components/AgentStatement";
import { NotedFigure } from "@/components/NotedFigure";
import { ServedText } from "@/components/ServedText";
import { Parts } from "@/components/Parts";
import { StatusMark } from "@/components/StatusMark";
import { Button, buttonStyles } from "@/components/ui/button";
import { hasRateSource } from "@/lib/budget";
import { cn } from "@/lib/cn";
import {
  NOT_MEASURED,
  liveCostFigure,
  runCostFigure,
  runningTokensText,
  runTokensFigure,
} from "@/lib/figures";
import { closingClock } from "@/lib/ended";
import { fmtClock, fmtDuration, lowerFirst, parseInstant, plural, pluralVerb } from "@/lib/format";
import { sessionLinkOf } from "@/lib/issue-link";
import { agentLabel, hopLabel, hopReason, modelLabel, ruleLabel, startLabel } from "@/lib/labels";
import { numberedRunsOf } from "@/lib/run-number";
import { agentStatement, runOutcome } from "@/lib/run-status";

import type { ReactNode } from "react";

import type {
  IssueDetailResponse,
  RunningEntryResponse,
  RunResponse,
  RunsResponse,
} from "@/api/types.gen";
import type { StagePosition, StageProgress } from "@/lib/lane";
import type { Mark } from "@/lib/run-status";

const PULL_REQUEST_KEY = "pull_request";

type StoryRun =
  | { kind: "run"; run: RunResponse; number: number }
  | { kind: "live"; running: RunningEntryResponse };

interface Chapter {
  id: string;
  key: string;
  isAgain: boolean;
  items: StoryRun[];
}

function startOf(item: StoryRun): string {
  return item.kind === "live" ? item.running.started_at : item.run.started_at;
}

function endOf(item: StoryRun): string | null {
  return item.kind === "live" ? null : item.run.completed_at;
}

function secondsBetween(from: number | null, to: number | null): number | null {
  return from === null || to === null || to < from ? null : (to - from) / 1000;
}

export function storyRunsOf(
  served: readonly RunResponse[],
  pages: readonly RunsResponse[],
): RunResponse[] {
  const byId = new Map<number, RunResponse>();
  for (const run of [...served, ...pages.flatMap((page) => page.runs ?? [])]) {
    byId.set(run.run_id, run);
  }
  return [...byId.values()].sort(
    (a, b) =>
      (parseInstant(a.started_at) ?? 0) - (parseInstant(b.started_at) ?? 0) || a.run_id - b.run_id,
  );
}

function chapterKeyOf(item: StoryRun): string {
  if (item.kind === "live") {
    return item.running.reaction_kind ? PULL_REQUEST_KEY : (item.running.rule_name ?? "");
  }
  const { run } = item;
  return run.reaction_kind || run.status === "ci_failed" ? PULL_REQUEST_KEY : (run.rule_name ?? "");
}

function chaptersOf(items: readonly StoryRun[]): Chapter[] {
  const chapters: Chapter[] = [];
  const seen = new Set<string>();
  for (const item of items) {
    const key = chapterKeyOf(item);
    const last = chapters.at(-1);
    if (last !== undefined && last.key === key) {
      last.items.push(item);
      continue;
    }
    chapters.push({
      id: item.kind === "live" ? "live" : String(item.run.run_id),
      key,
      isAgain: seen.has(key),
      items: [item],
    });
    seen.add(key);
  }
  return chapters;
}

function agentPartsOf(item: StoryRun | undefined): string[] {
  if (item === undefined) {
    return [];
  }
  const kind = item.kind === "live" ? item.running.agent_kind : item.run.agent_kind;
  const model =
    item.kind === "live"
      ? item.running.model_name || item.running.configured_model
      : item.run.model_name || item.run.configured_model;
  if (!kind) {
    return [];
  }
  return model ? [agentLabel(kind), modelLabel(model)] : [agentLabel(kind)];
}

function chapterHeadOf(
  chapter: Chapter,
  position: StagePosition | null,
  pullRequest: number | undefined,
): { title: string; sub: string[] } {
  if (chapter.key === PULL_REQUEST_KEY) {
    return {
      title: pullRequest === undefined ? "Pull request" : `Pull request #${pullRequest}`,
      sub: [],
    };
  }
  const agent = agentPartsOf(chapter.items[0]);
  if (position !== null && position.chain.includes(chapter.key)) {
    const stage = `Stage ${position.chain.indexOf(chapter.key) + 1} of ${position.chain.length}`;
    return {
      title: `${ruleLabel(chapter.key)}${chapter.isAgain ? ", again" : ""}`,
      sub: [stage, ...agent],
    };
  }
  if (chapter.key === "") {
    return { title: "Work", sub: agent };
  }
  return { title: "Work", sub: [`Rule ${ruleLabel(chapter.key)}`, ...agent] };
}

function endWordOf(last: StoryRun): string {
  if (last.kind === "live") {
    return "running";
  }
  const { stage_result: result, stage_target: target } = last.run;
  if (target && result) {
    return hopLabel(target, result);
  }
  return lowerFirst(runOutcome(last.run).word);
}

function foldedPartsOf(chapter: Chapter): string[] {
  const first = chapter.items[0];
  const last = chapter.items.at(-1);
  if (first === undefined || last === undefined) {
    return [];
  }
  const end = endOf(last);
  const span = `${fmtClock(startOf(first))} to ${end === null ? "now" : fmtClock(end)}`;
  return [plural(chapter.items.length, "run", "runs"), span, endWordOf(last)];
}

function originOf(item: StoryRun): string | undefined {
  const reaction = item.kind === "live" ? item.running.reaction_kind : item.run.reaction_kind;
  if (reaction) {
    return startLabel(reaction, "origin");
  }
  const trigger = item.kind === "live" ? undefined : item.run.trigger;
  return trigger === "error" ||
    trigger === "stall" ||
    trigger === "continuation" ||
    trigger === "usage_limit" ||
    trigger === "ci_fix"
    ? startLabel(trigger, "trigger")
    : undefined;
}

interface RunCardProps {
  item: StoryRun;
  identifier: string;
  now: number;
  hasEstimates: boolean;
}

function RunCard({ item, identifier, now, hasEstimates }: RunCardProps) {
  const location = useLocation();
  const isLive = item.kind === "live";
  const status = isLive ? { word: "Running", mark: "running" as Mark } : runOutcome(item.run);
  const origin = originOf(item);
  const started = parseInstant(startOf(item));
  const end = endOf(item);
  const duration = fmtDuration(secondsBetween(started, end === null ? now : parseInstant(end)));
  const turns = isLive ? item.running.turn_count : item.run.turns_completed;
  const tokens = isLive ? { text: runningTokensText(item.running) } : runTokensFigure(item.run);
  const cost = isLive
    ? liveCostFigure(item.running.cost_status, item.running.estimated_cost_usd, "session")
    : runCostFigure(item.run);
  const error = isLive ? null : item.run.error;
  const statement = isLive ? null : agentStatement(item.run);
  const dispatchId = isLive ? item.running.dispatch_id : item.run.dispatch_id;
  const session = dispatchId ? sessionLinkOf(identifier, dispatchId, location) : undefined;

  return (
    <div className="flex flex-col gap-xxs rounded-xl border border-line bg-canvas px-md py-sm">
      <div className="flex flex-wrap items-baseline gap-x-sm gap-y-xxs">
        <strong className="text-body-md font-bold text-ink">
          {item.kind === "live" ? "Live run" : `Run ${item.number}`}
        </strong>
        {status.mark === null ? (
          <span className="text-body-sm text-ink-body">{lowerFirst(status.word)}</span>
        ) : (
          <StatusMark mark={status.mark} label={lowerFirst(status.word)} className="text-body-sm" />
        )}
        {origin === undefined ? undefined : (
          <span className="text-body-sm text-ink-muted">{lowerFirst(origin)}</span>
        )}
        <Parts
          parts={[
            `${fmtClock(startOf(item))} to ${end === null ? "now" : fmtClock(end)}`,
            duration,
          ]}
          className="ml-auto text-body-sm whitespace-nowrap text-ink-muted tabular-nums"
        />
      </div>
      {error ? <p className="text-body-md text-ink-body">{error}</p> : undefined}
      {statement === null ? undefined : (
        <AgentStatement text={statement} className="text-body-md" />
      )}
      <p className="mt-xxs flex flex-wrap gap-x-md gap-y-xxs text-body-sm text-ink-muted">
        {turns > 0 ? (
          <span>
            Turns <b className="font-medium text-ink-body tabular-nums">{turns}</b>
          </span>
        ) : undefined}
        <span>
          Tokens{" "}
          <b className="font-medium text-ink-body tabular-nums">
            <NotedFigure figure={tokens} />
          </b>
        </span>
        {hasEstimates && cost.text !== NOT_MEASURED ? (
          <span>
            Est. cost{" "}
            <b className="font-medium text-ink-body tabular-nums">
              <NotedFigure figure={cost} />
            </b>
          </span>
        ) : undefined}
        {session === undefined ? undefined : (
          <Link
            to={session.to}
            state={session.state}
            className="text-link underline underline-offset-2 hover:text-link-hover focus-visible:outline-2 focus-visible:outline-focus-ring active:text-link-active"
          >
            Session timeline
          </Link>
        )}
      </p>
    </div>
  );
}

interface BeatProps {
  time: string;
  node: "open" | "solid" | "now" | "attention";
  children: ReactNode;
}

const NODE: Record<BeatProps["node"], string> = {
  open: "border-ink-muted bg-canvas",
  solid: "border-ink-body bg-ink-body",
  now: "border-primary bg-primary shadow-[0_0_0_3px_var(--color-surface-sunken)]",
  attention: "border-warning-fill bg-warning-fill",
};

function Beat({ time, node, children }: BeatProps) {
  return (
    <li className="relative grid grid-cols-[48px_20px_minmax(0,1fr)] gap-x-sm py-xs text-body-md max-md:grid-cols-[40px_16px_minmax(0,1fr)]">
      <span className="pt-xxs text-right text-body-sm text-ink-muted tabular-nums">{time}</span>
      <span
        aria-hidden="true"
        className={cn(
          "relative z-(--z-raised) mt-xs ml-xxs size-2.25 rounded-full border-[1.5px]",
          NODE[node],
        )}
      />
      <div className="min-w-0">{children}</div>
    </li>
  );
}

function Beats({ children }: { children: ReactNode }) {
  return (
    <ul className="relative before:absolute before:top-xs before:bottom-xs before:left-15 before:w-px before:bg-line max-md:before:left-13">
      {children}
    </ul>
  );
}

interface StageSummaryProps {
  position: StagePosition;
  progress: StageProgress;
  chapters: readonly Chapter[];
}

function stageSpanOf(chapters: readonly Chapter[], stage: string): string | undefined {
  const items = chapters.filter((chapter) => chapter.key === stage).flatMap((c) => c.items);
  const first = items[0];
  const last = items.at(-1);
  if (first === undefined || last === undefined) {
    return undefined;
  }
  const end = endOf(last);
  const again = chapters.filter((chapter) => chapter.key === stage).length > 1 ? ", ran again" : "";
  return `${fmtClock(startOf(first))} to ${end === null ? "now" : fmtClock(end)}${again}`;
}

function StageSummary({ position, progress, chapters }: StageSummaryProps) {
  const { k, n, done, focus, remaining, remainingWord } = progress;
  const [isOpen, setIsOpen] = useState(false);
  const parts: ReactNode[] = [];
  if (done > 0) {
    parts.push(
      <StatusMark key="done" mark="succeeded" label={plural(done, "stage done", "stages done")} />,
    );
  }
  if (focus?.kind === "current") {
    const label = (
      <>
        <b className="font-bold text-ink">{ruleLabel(position.current)}</b> ({k} of {n})
      </>
    );
    parts.push(<StatusMark key="focus" mark={focus.mark ?? "current"} label={label} />);
  } else if (focus !== null) {
    parts.push(
      <StatusMark
        key="focus"
        mark="handed"
        label={
          <>
            <b className="font-bold text-ink">hop to {ruleLabel(focus.target)} not made</b>:{" "}
            {hopReason(focus.result)}
          </>
        }
      />,
    );
  }
  if (remaining > 0) {
    parts.push(<span key="remaining">{`${remaining} ${remainingWord}`}</span>);
  }

  return (
    <Collapsible.Root
      open={isOpen}
      onOpenChange={setIsOpen}
      className="mb-lg rounded-xl border border-line px-md py-sm"
    >
      <div className="flex flex-wrap items-center justify-between gap-md text-body-md text-ink-body">
        <Parts parts={parts} className="gap-x-md" />
        <Collapsible.Trigger className={buttonStyles("secondary")}>
          {isOpen ? "Hide stages" : "All stages"}
        </Collapsible.Trigger>
      </div>
      <Collapsible.Panel>
        <ol className="mt-sm columns-2 gap-x-xl border-t border-line pt-xs max-md:columns-1">
          {position.chain.map((stage, index) => {
            const j = index + 1;
            const state =
              j <= done ? "done" : focus?.kind === "current" && j === k ? "cur" : "ahead";
            const span = stageSpanOf(chapters, stage) ?? (state === "ahead" ? remainingWord : "");
            return (
              <li
                key={`${j}-${stage}`}
                className={cn(
                  "grid break-inside-avoid grid-cols-[20px_var(--size-icon)_auto_minmax(0,1fr)] items-center gap-xs py-xxs text-body-sm",
                  state === "cur" && "font-bold",
                  state === "ahead" ? "text-ink-muted" : "text-ink-body",
                )}
              >
                <span className="text-right text-ink-muted tabular-nums">{j}</span>
                <span className="inline-flex">
                  {state === "done" ? (
                    <StatusMark mark="succeeded" />
                  ) : state === "cur" ? (
                    <StatusMark mark="current" />
                  ) : undefined}
                </span>
                <span className="font-mono">{ruleLabel(stage)}</span>
                <span className="font-normal text-ink-muted">{span}</span>
              </li>
            );
          })}
        </ol>
      </Collapsible.Panel>
    </Collapsible.Root>
  );
}

interface ChapterBodyProps {
  chapter: Chapter;
  identifier: string;
  now: number;
  hasEstimates: boolean;
  nowLine?: { text: string; time: string; needsAttention: boolean };
}

function ChapterBeats({ chapter, identifier, now, hasEstimates, nowLine }: ChapterBodyProps) {
  return (
    <Beats>
      {chapter.items.map((item) =>
        item.kind === "run" && item.run.status === "ci_failed" ? (
          <Beat key={item.run.run_id} time={fmtClock(item.run.completed_at)} node="solid">
            <StatusMark mark="failed" label="CI checks failed" />
          </Beat>
        ) : (
          <Beat
            key={item.kind === "live" ? "live" : item.run.run_id}
            time={fmtClock(startOf(item))}
            node={item.kind === "live" ? "now" : "solid"}
          >
            <RunCard item={item} identifier={identifier} now={now} hasEstimates={hasEstimates} />
          </Beat>
        ),
      )}
      {nowLine === undefined ? undefined : (
        <Beat time={nowLine.time} node={nowLine.needsAttention ? "attention" : "now"}>
          <p className="text-ink">
            <ServedText text={nowLine.text} />
          </p>
        </Beat>
      )}
    </Beats>
  );
}

export interface IssueStoryProps {
  detail: IssueDetailResponse;
  position: StagePosition | null;
  progress: StageProgress | null;
  /** Whether the issue has an unacknowledged attention item, which colors the closing beat. */
  needsAttention: boolean;
}

export function IssueStory({ detail, position, progress, needsAttention }: IssueStoryProps) {
  const now = useNow();
  const { data: state } = useStateQuery();
  const hasEstimates = hasRateSource(state);
  const [wanted, setWanted] = useState(0);
  const older = useHistoryRunsQuery(detail.issue_id, {}, wanted > 0);
  const { hasNextPage, isFetching, isError, fetchNextPage } = older;
  const loadedPages = older.data?.pages.length ?? 0;

  useEffect(() => {
    if (wanted > loadedPages && loadedPages > 0 && hasNextPage && !isFetching && !isError) {
      void fetchNextPage();
    }
  }, [wanted, loadedPages, hasNextPage, isFetching, isError, fetchNextPage]);

  const { history, running } = detail;
  const runs = storyRunsOf(history?.runs ?? [], older.data?.pages ?? []);
  const items: StoryRun[] = [
    ...numberedRunsOf(runs, history?.totals.run_count ?? runs.length).map(
      ({ run, number }): StoryRun => ({ kind: "run", run, number }),
    ),
    ...(running === null ? [] : [{ kind: "live", running } satisfies StoryRun]),
  ];
  const isExhausted = wanted > 0 && older.data !== undefined && !hasNextPage;
  const canShowOlder =
    history !== null &&
    history.next_cursor !== null &&
    runs.length < history.totals.run_count &&
    !isExhausted;
  const unlisted =
    history === null
      ? undefined
      : pluralVerb(
          history.totals.run_count - runs.length,
          ["older run", "older runs"],
          ["is", "are"],
        );

  function handleShowOlder() {
    if (isError) {
      void (older.data === undefined ? older.refetch() : fetchNextPage());
      return;
    }
    setWanted(wanted === 0 ? 2 : Math.max(wanted, loadedPages) + 1);
  }

  const chapters = chaptersOf(items);
  const pullRequest =
    detail.pending_reactions?.[0]?.pull_request.number ??
    detail.history?.issue_outcome?.pull_request?.number;
  const closingTime = closingClock(detail, now);
  const nowLine = {
    text: detail.status_text_full,
    time: closingTime,
    needsAttention,
  };

  return (
    <div className="flex min-w-0 flex-col">
      {position === null || progress === null ? undefined : (
        <StageSummary position={position} progress={progress} chapters={chapters} />
      )}
      {canShowOlder && unlisted !== undefined ? (
        <div className="mb-sm flex flex-wrap items-center gap-x-sm gap-y-xs">
          <p className="text-body-sm text-ink-muted">
            {unlisted.subject} {unlisted.verb} not listed
          </p>
          <Button disabled={isFetching} onClick={handleShowOlder}>
            Show older runs
          </Button>
          {isError ? (
            <p className="text-body-sm text-ink-body">Older runs could not be read.</p>
          ) : undefined}
        </div>
      ) : undefined}
      {chapters.length === 0 ? (
        <Beats>
          <Beat time={closingTime} node={needsAttention ? "attention" : "now"}>
            <p className="text-ink">
              <ServedText text={detail.status_text_full} />
            </p>
          </Beat>
        </Beats>
      ) : (
        chapters.map((chapter, index) => {
          const head = chapterHeadOf(chapter, position, pullRequest);
          const isCurrent = index === chapters.length - 1;
          if (position !== null && !isCurrent) {
            return (
              <Collapsible.Root key={chapter.id} className="border-t border-line data-open:pb-md">
                <Collapsible.Trigger className="group flex w-full cursor-pointer items-baseline gap-sm rounded py-xs text-left hover:bg-item-hover focus-visible:outline-2 focus-visible:outline-focus-ring">
                  <span
                    aria-hidden="true"
                    className="w-3 text-ink-muted tabular-nums group-data-panel-open:hidden"
                  >
                    +
                  </span>
                  <span
                    aria-hidden="true"
                    className="hidden w-3 text-ink-muted tabular-nums group-data-panel-open:inline"
                  >
                    −
                  </span>
                  <span className="font-mono text-body-md font-bold text-ink">{head.title}</span>
                  <Parts parts={foldedPartsOf(chapter)} className="text-body-sm text-ink-muted" />
                </Collapsible.Trigger>
                <Collapsible.Panel>
                  {head.sub.length === 0 ? undefined : (
                    <p className="mb-sm text-body-sm text-ink-muted">
                      <Parts parts={head.sub} />
                    </p>
                  )}
                  <ChapterBeats
                    chapter={chapter}
                    identifier={detail.issue_identifier}
                    now={now}
                    hasEstimates={hasEstimates}
                  />
                </Collapsible.Panel>
              </Collapsible.Root>
            );
          }
          return (
            <section key={chapter.id} className={cn("mb-xl", index > 0 && "mt-lg")}>
              <h2 className="font-heading text-h6 font-semibold text-ink">{head.title}</h2>
              {head.sub.length === 0 ? undefined : (
                <p className="mb-sm text-body-sm text-ink-muted">
                  <Parts parts={head.sub} />
                </p>
              )}
              <ChapterBeats
                chapter={chapter}
                identifier={detail.issue_identifier}
                now={now}
                hasEstimates={hasEstimates}
                {...(isCurrent ? { nowLine } : {})}
              />
            </section>
          );
        })
      )}
    </div>
  );
}
