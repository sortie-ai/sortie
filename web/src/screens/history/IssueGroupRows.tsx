import { Fragment, useState } from "react";
import { Link, useLocation, useNavigate } from "react-router";

import { useHistoryRunsQuery } from "@/api/queries";
import { ServedText } from "@/components/ServedText";
import { StandingMark } from "@/components/Lane";
import { StatusMark } from "@/components/StatusMark";
import { Button } from "@/components/ui/button";
import {
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
  TableSpanCell,
} from "@/components/ui/table";
import { hasRateSource } from "@/lib/budget";
import { cn } from "@/lib/cn";
import {
  NOT_MEASURED,
  UNPRICED,
  costTitle,
  runCacheReadsText,
  runCostText,
  runTokensText,
  totalsCacheReadsText,
  totalsCostText,
  totalsTokensText,
} from "@/lib/figures";
import { fmtCount, fmtDuration, fmtStamp, parseInstant, plural, upperFirst } from "@/lib/format";
import { issueLinkOf } from "@/lib/issue-link";
import { hopLabel, ruleLabel } from "@/lib/labels";
import { issueAwaitsApproval } from "@/lib/active-rows";
import { issueStanding } from "@/lib/lane";
import { agentStatement, runOutcome } from "@/lib/run-status";

import type { HistoryFilter } from "@/api/client";
import type { IssueGroupResponse, RunResponse, SkillCall, StateResponse } from "@/api/types.gen";

const READ_FAILED = "Run history could not be read.";

// The column takes the width the others leave.
const SKILLS_WIDTH = "w-full min-w-40";

interface Columns {
  hasRule: boolean;
  hasCost: boolean;
  span: number;
}

function columnsOf(state: StateResponse | undefined): Columns {
  const hasRule = (state?.config?.dispatch.rules ?? []).length > 0;
  const hasCost = hasRateSource(state);
  return { hasRule, hasCost, span: 7 + (hasRule ? 1 : 0) + (hasCost ? 1 : 0) };
}

/** Props of {@link HistoryTableHead}. */
export interface HistoryTableHeadProps {
  state: StateResponse | undefined;
}

/** Renders the header row of the run table; its columns follow the workflow. */
export function HistoryTableHead({ state }: HistoryTableHeadProps) {
  const { hasRule, hasCost } = columnsOf(state);
  return (
    <TableHeader>
      <TableHead>Status</TableHead>
      {hasRule ? <TableHead>Rule</TableHead> : undefined}
      <TableHead>Agent</TableHead>
      <TableHead className={SKILLS_WIDTH}>Skills</TableHead>
      <TableHead>Duration</TableHead>
      <TableHead>Turns</TableHead>
      <TableHead title="Cache reads not included">Tokens</TableHead>
      {hasCost ? <TableHead>Est. cost</TableHead> : undefined}
      <TableHead>Finished</TableHead>
    </TableHeader>
  );
}

function runSeconds(run: RunResponse): number | null {
  const started = parseInstant(run.started_at);
  const completed = parseInstant(run.completed_at);
  return started === null || completed === null || completed < started
    ? null
    : (completed - started) / 1000;
}

function hopText(run: RunResponse): string | undefined {
  if (!run.stage_target) {
    return undefined;
  }
  return upperFirst(hopLabel(run.stage_target, run.stage_result));
}

function tokensPart(text: string): string {
  return text === NOT_MEASURED ? "tokens not measured" : `${text} tokens`;
}

function cacheReadsTitle(text: string | null): string | undefined {
  return text === null ? undefined : `${text} cache reads`;
}

function costPart(text: string): string {
  if (text === UNPRICED) {
    return "cost unpriced";
  }
  return text === NOT_MEASURED ? "cost not measured" : `${text} estimated`;
}

function skillsTitle(skills: readonly SkillCall[]): string {
  return skills
    .map((call) => (call.tool_error === true ? `${call.skill} (failed)` : call.skill))
    .join(", ");
}

interface SkillsCellProps {
  skills: readonly SkillCall[] | undefined;
}

function SkillsCell({ skills }: SkillsCellProps) {
  const calls = skills ?? [];

  return (
    <TableCell className={cn(SKILLS_WIDTH, "max-w-0")}>
      {calls.length === 0 ? undefined : (
        <span title={skillsTitle(calls)} className="block truncate">
          {calls.map((call, index) => (
            <Fragment key={index}>
              {index === 0 ? undefined : ", "}
              {call.tool_error === true ? (
                <StatusMark
                  mark="failed"
                  label={
                    <>
                      {call.skill}
                      <span className="sr-only"> (failed)</span>
                    </>
                  }
                />
              ) : (
                call.skill
              )}
            </Fragment>
          ))}
        </span>
      )}
    </TableCell>
  );
}

interface RunRowProps {
  run: RunResponse;
  columns: Columns;
}

function RunRow({ run, columns }: RunRowProps) {
  const navigate = useNavigate();
  const location = useLocation();
  const link = issueLinkOf(run.issue_identifier, location);
  const status = runOutcome(run);
  const statement = agentStatement(run);
  const model = run.model_name || run.configured_model;
  const agent = [run.agent_kind, model].filter((part) => part !== undefined && part !== "");
  const isModelDiverged =
    !!run.model_name && !!run.configured_model && run.model_name !== run.configured_model;
  const ruleName = run.rule_name;
  const rule = ruleLabel(ruleName);
  const hop = hopText(run);

  return (
    <TableRow look="link" onClick={() => void navigate(link.to, { state: link.state })}>
      <TableCell title={statement === null ? run.error || undefined : `\u201C${statement}\u201D`}>
        <Link
          to={link.to}
          state={link.state}
          onClick={(event) => event.stopPropagation()}
          className="rounded focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-focus-ring"
        >
          {status.mark === null ? (
            <ServedText text={status.word} />
          ) : (
            <StatusMark mark={status.mark} label={<ServedText text={status.word} />} />
          )}
        </Link>
      </TableCell>
      {columns.hasRule ? (
        <TableCell note={hop === undefined ? undefined : <ServedText text={hop} />}>
          <span className={cn(rule === ruleName && "font-mono text-body-sm")}>
            <ServedText text={rule} />
          </span>
        </TableCell>
      ) : undefined}
      <TableCell
        title={isModelDiverged ? `Configured ${run.configured_model}` : undefined}
        note={agent[1] === undefined ? undefined : <ServedText text={agent[1]} />}
      >
        {agent[0] === undefined ? undefined : <ServedText text={agent[0]} />}
      </TableCell>
      <SkillsCell skills={run.skills} />
      <TableCell>{fmtDuration(runSeconds(run))}</TableCell>
      <TableCell>{fmtCount(run.turns_completed)}</TableCell>
      <TableCell title={cacheReadsTitle(runCacheReadsText(run))}>{runTokensText(run)}</TableCell>
      {columns.hasCost ? (
        <TableCell title={costTitle(run.cost_status, run.estimated_cost_usd)}>
          {runCostText(run)}
        </TableCell>
      ) : undefined}
      <TableCell>{fmtStamp(run.completed_at)}</TableCell>
    </TableRow>
  );
}

/** Props of {@link IssueGroupRows}. */
export interface IssueGroupRowsProps {
  group: IssueGroupResponse;
  filter: HistoryFilter;
  state: StateResponse | undefined;
}

/**
 * Renders one issue of History as a row group: a header row for the issue, its
 * newest runs, and a "Show more runs" row that swaps the served runs for pages
 * read from the run list. Mount it with a key that changes with the filter so
 * an expansion resets then.
 */
export function IssueGroupRows({ group, filter, state }: IssueGroupRowsProps) {
  const navigate = useNavigate();
  const location = useLocation();
  const link = issueLinkOf(group.issue_identifier, location);
  const columns = columnsOf(state);
  const [isExpanded, setIsExpanded] = useState(false);
  const expansion = useHistoryRunsQuery(group.issue_id, filter, isExpanded);

  const key = group.display_identifier || group.issue_identifier;
  const title = group.issue_title || key;
  const standing = issueStanding(
    group.issue_id,
    group.status,
    group.issue_outcome,
    state?.attention ?? [],
    issueAwaitsApproval(state, group.issue_id),
  );
  const ending = group.status === "history" ? group.issue_outcome : null;
  const cacheReads = totalsCacheReadsText(group.totals);
  const costHover = costTitle(group.totals.cost_status, group.totals.estimated_cost_usd);
  const secondLine = (
    [
      ["key", group.issue_title ? key : undefined],
      ["status", standing.word || undefined],
      ["runs", plural(group.totals.run_count, "run", "runs")],
      ["tokens", tokensPart(totalsTokensText(group.totals))],
      ["cache-reads", cacheReads === null ? undefined : `${cacheReads} cache reads`],
      ["cost", columns.hasCost ? costPart(totalsCostText(group.totals)) : undefined],
    ] as const
  ).filter(([, part]) => part !== undefined);

  const expandedRuns = expansion.data?.pages.flatMap((page) => page.runs ?? []);
  const runs = expandedRuns ?? group.runs ?? [];
  const hasMore = isExpanded
    ? expandedRuns === undefined || expansion.hasNextPage
    : group.next_cursor !== null;
  const isFailed =
    isExpanded &&
    expansion.isError &&
    (expandedRuns === undefined || expansion.isFetchNextPageError);

  function handleShowMore() {
    if (isExpanded) {
      void expansion.fetchNextPage();
    } else {
      setIsExpanded(true);
    }
  }

  return (
    <TableBody>
      <TableRow look="group" onClick={() => void navigate(link.to, { state: link.state })}>
        <TableSpanCell colSpan={columns.span}>
          <div className="flex items-start gap-sm">
            <span className="flex w-(--size-icon) shrink-0 items-center justify-center self-center">
              {standing.marker === null ? undefined : <StandingMark marker={standing.marker} />}
            </span>
            <div className="min-w-0 flex-1">
              <p className="text-body-md font-medium text-ink wrap-anywhere">
                <Link
                  to={link.to}
                  state={link.state}
                  onClick={(event) => event.stopPropagation()}
                  className="rounded focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-focus-ring"
                >
                  <ServedText text={title} />
                </Link>
              </p>
              <p className="flex flex-wrap gap-x-md text-body-sm text-ink-muted wrap-anywhere">
                {secondLine.map(([id, part]) => (
                  <span key={id} title={id === "cost" ? costHover : undefined}>
                    {part}
                  </span>
                ))}
              </p>
            </div>
            {ending === null ? undefined : (
              <span className="shrink-0 text-body-sm whitespace-nowrap text-ink-muted tabular-nums">
                {fmtStamp(ending.observed_at)}
              </span>
            )}
          </div>
        </TableSpanCell>
      </TableRow>
      {runs.map((run) => (
        <RunRow key={run.run_id} run={run} columns={columns} />
      ))}
      {hasMore ? (
        <TableRow>
          <TableSpanCell colSpan={columns.span} isCompact>
            <div className="flex items-center gap-sm">
              <Button variant="ghost" onClick={handleShowMore} disabled={expansion.isFetching}>
                Show more runs
              </Button>
              {isFailed ? (
                <span className="text-body-sm text-error">{READ_FAILED}</span>
              ) : undefined}
            </div>
          </TableSpanCell>
        </TableRow>
      ) : undefined}
    </TableBody>
  );
}
