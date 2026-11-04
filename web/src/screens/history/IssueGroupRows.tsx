import { Fragment, useState } from "react";
import { useLocation } from "react-router";

import { useHistoryRunsQuery } from "@/api/queries";
import { Link, useNavigate } from "@/components/carried-links";
import { IssueTitleLink } from "@/components/IssueTitleLink";
import { NotedFigure } from "@/components/NotedFigure";
import { NameText } from "@/components/NameText";
import { Parts } from "@/components/Parts";
import { ServedText } from "@/components/ServedText";
import { MarkSlot } from "@/components/Lane";
import { StatusMark } from "@/components/StatusMark";
import { KindsFigure, kindsOfTokenInfo } from "@/components/TokenKinds";
import { Button } from "@/components/ui/button";
import { MONO } from "@/components/ui/mono";
import {
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
  TableSpanCell,
} from "@/components/ui/table";
import { cn } from "@/lib/cn";
import {
  runCostFigure,
  runTokensFigure,
  tokensKindFigure,
  totalsCostFigure,
  totalsTokensFigure,
  vendorAmountFigure,
} from "@/lib/figures";
import { fmtCount, fmtDuration, fmtStamp, parseInstant, plural, upperFirst } from "@/lib/format";
import { issueLinkOf } from "@/lib/issue-link";
import { hopLabel, ruleLabel } from "@/lib/labels";
import { issueAwaitsApproval } from "@/lib/active-rows";
import { issueStanding } from "@/lib/lane";
import { agentStatement, runOutcome } from "@/lib/run-status";

import type { HistoryFilter } from "@/api/client";
import type {
  HistoryTotalsResponse,
  IssueGroupResponse,
  RunResponse,
  SkillCall,
  StateResponse,
} from "@/api/types.gen";
import type { Units } from "@/lib/units";
import type { ReactNode } from "react";

const READ_FAILED = "Run history could not be read.";

const SKILLS_WIDTH = "w-full min-w-40";

interface Columns {
  hasRule: boolean;
  units: Units;
  span: number;
}

function columnsOf(state: StateResponse | undefined, units: Units): Columns {
  const hasRule = (state?.config?.dispatch.rules ?? []).length > 0;
  return { hasRule, units, span: 7 + (hasRule ? 1 : 0) };
}

export interface HistoryTableHeadProps {
  state: StateResponse | undefined;
  units: Units;
}

export function HistoryTableHead({ state, units }: HistoryTableHeadProps) {
  const { hasRule } = columnsOf(state, units);
  return (
    <TableHeader>
      <TableHead>Status</TableHead>
      {hasRule ? <TableHead>Rule</TableHead> : undefined}
      <TableHead>Agent</TableHead>
      <TableHead className={SKILLS_WIDTH}>Skills</TableHead>
      <TableHead align="right">Duration</TableHead>
      <TableHead align="right">Turns</TableHead>
      {units === "tokens" ? (
        <TableHead align="right">Tokens</TableHead>
      ) : (
        <TableHead align="right">Est. cost</TableHead>
      )}
      <TableHead align="right">Finished</TableHead>
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

function tokensPart(totals: HistoryTotalsResponse): ReactNode {
  const figure = (
    <KindsFigure
      figure={totalsTokensFigure(totals)}
      kinds={totals.uncached_tokens === null ? null : kindsOfTokenInfo(totals.tokens)}
      figureOf={tokensKindFigure(!totals.tokens_complete)}
    />
  );
  return totals.uncached_tokens === null ? <>tokens {figure}</> : <>{figure} tokens</>;
}

function costPart(totals: HistoryTotalsResponse): ReactNode {
  const figure = <NotedFigure figure={totalsCostFigure(totals)} />;
  return totals.cost_status === "unpriced" || totals.cost_status === "unmeasured" ? (
    <>cost {figure}</>
  ) : (
    <>{figure} estimated</>
  );
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
                      <span className={MONO}>{call.skill}</span>
                      <span className="sr-only"> (failed)</span>
                    </>
                  }
                />
              ) : (
                <span className={MONO}>{call.skill}</span>
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
          <span className={cn(rule === ruleName && MONO)}>
            <ServedText text={rule} />
          </span>
        </TableCell>
      ) : undefined}
      <TableCell
        title={isModelDiverged ? `Configured ${run.configured_model}` : undefined}
        note={agent[1] === undefined ? undefined : <NameText name={agent[1]} />}
      >
        {agent[0] === undefined ? undefined : <NameText name={agent[0]} />}
      </TableCell>
      <SkillsCell skills={run.skills} />
      <TableCell look="number">{fmtDuration(runSeconds(run))}</TableCell>
      <TableCell look="number">{fmtCount(run.turns_completed)}</TableCell>
      <TableCell look="number">
        {columns.units === "tokens" ? (
          <KindsFigure
            figure={runTokensFigure(run)}
            kinds={run.uncached_tokens === null ? null : kindsOfTokenInfo(run.tokens)}
            figureOf={tokensKindFigure(run.usage_completeness === "lower_bound")}
          />
        ) : (
          <NotedFigure figure={runCostFigure(run)} />
        )}
      </TableCell>
      <TableCell look="number">{fmtStamp(run.completed_at)}</TableCell>
    </TableRow>
  );
}

export interface IssueGroupRowsProps {
  group: IssueGroupResponse;
  filter: HistoryFilter;
  state: StateResponse | undefined;
  units: Units;
}

/** Mount it with a key that changes with the filter so an expansion resets then. */
export function IssueGroupRows({ group, filter, state, units }: IssueGroupRowsProps) {
  const navigate = useNavigate();
  const location = useLocation();
  const link = issueLinkOf(group.issue_identifier, location);
  const columns = columnsOf(state, units);
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
  const unitParts: [string, ReactNode][] =
    units === "tokens"
      ? [["tokens", tokensPart(group.totals)]]
      : [
          ["cost", costPart(group.totals)],
          ...(group.totals.vendor_amounts ?? []).map((amount): [string, ReactNode] => [
            `vendor-${amount.unit}`,
            <NotedFigure key={amount.unit} figure={vendorAmountFigure(amount, "full")} />,
          ]),
        ];
  const parts: [string, ReactNode][] = [
    ["key", group.issue_title ? key : undefined],
    ["status", standing.word || undefined],
    ["runs", plural(group.totals.run_count, "run", "runs")],
    ...unitParts,
  ];
  const secondLine = parts.filter(([, part]) => part !== undefined);

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
          {/* The date shares the title's line only, so the second line may run on under it. */}
          <div className="grid grid-cols-[auto_minmax(0,1fr)_auto] items-start gap-x-sm">
            <MarkSlot marker={standing.marker} />
            <p className="text-body-md font-medium text-ink wrap-anywhere max-md:self-baseline">
              <IssueTitleLink identifier={group.issue_identifier}>
                <ServedText text={title} />
              </IssueTitleLink>
            </p>
            <p className="col-span-2 col-start-2 text-body-sm text-ink-muted wrap-anywhere">
              <Parts
                parts={secondLine.map(([id, part]) => (
                  <span key={id}>{part}</span>
                ))}
              />
            </p>
            {ending === null ? undefined : (
              <span className="col-start-3 row-start-1 whitespace-nowrap tabular-nums max-md:self-baseline max-md:text-body-sm max-md:text-ink-muted">
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
