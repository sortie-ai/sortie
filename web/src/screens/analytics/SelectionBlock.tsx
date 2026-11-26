import { useLocation } from "react-router";

import { useStatsSpanBeforeQuery, useStatsSpanQuery } from "@/api/queries";
import { RunStatusText } from "@/components/RunStatusText";
import { XIcon } from "@/components/icons";
import { Link, useNavigate } from "@/components/carried-links";
import { NameText } from "@/components/NameText";
import { KindRows, KindsFigure, kindsOfCosts, kindsOfStats } from "@/components/TokenKinds";
import { Button } from "@/components/ui/button";
import { Hint } from "@/components/ui/hint";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import {
  COST_LOWER_BOUND,
  LABEL_EST_COST,
  LABEL_TOKENS,
  NOT_MEASURED,
  TOKENS_LOWER_BOUND,
  UNKNOWN_FIGURE,
  UNPRICED_TEXT,
} from "@/lib/figures";
import { fmtChange, fmtCount, fmtMoney, fmtMoneyCut, fmtPercent, fmtTokens } from "@/lib/format";
import { issueLinkOf, sessionLinkOf } from "@/lib/issue-link";
import { ruleLabel } from "@/lib/labels";
import { cn } from "@/lib/cn";

import {
  BASELINE_BUCKETS,
  baselineWindowOf,
  bucketsBefore,
  isEmptyBucket,
  previousWindowOf,
  selectedWindowOf,
  sessionsByShare,
  valueOf,
} from "./activity";
import { BASELINE_WORDS, PERIOD_NAMES, previousLabelOf, spanNameOf } from "./bucket-names";

import type { UseQueryResult } from "@tanstack/react-query";
import type { SpanReport, SpanSession, SpendBucket } from "@/api/types.gen";
import type { Units } from "@/lib/units";
import type { ReactNode } from "react";
import type { Period } from "./activity";

const LINK =
  "text-link underline underline-offset-2 hover:text-link-hover focus-visible:outline-2 focus-visible:outline-focus-ring active:text-link-active";

// Holds the height of a figure line while the span report loads.
const PENDING = "\u00a0";

// A figure the report could not give; the prototype's dash.
const NONE = "\u2013";

export interface SelectionBlockProps {
  /** The selected buckets, in chart order. */
  buckets: readonly SpendBucket[];
  period: Period;
  units: Units;
  /** Whether "Zoom in" is offered. */
  canZoom: boolean;
  onZoomIn: () => void;
  onClear: () => void;
}

interface Scope {
  first: number;
  count: number;
  period: Period;
  units: Units;
}

interface Reads {
  selected: UseQueryResult<SpanReport>;
  before: UseQueryResult<SpanReport>;
  baseline: UseQueryResult<SpanReport>;
}

function fmtOf(units: Units): (n: number) => string {
  return units === "tokens" ? fmtTokens : fmtMoney;
}

function placeholderOf(read: UseQueryResult<SpanReport>): string {
  return read.isError ? NONE : PENDING;
}

function hasFigureOf(totals: SpendBucket, units: Units): boolean {
  return (
    isEmptyBucket(totals) ||
    (units === "tokens" ? totals.tokens !== null : totals.cost_usd !== null)
  );
}

function costNoteOf(totals: SpendBucket): ReactNode {
  const { cost_by_kind: costByKind, cost_lower_bound: isLowerBound } = totals;
  if (costByKind === null && !isLowerBound) {
    return undefined;
  }
  return (
    <div className="min-w-35">
      {costByKind === null ? undefined : (
        <KindRows
          kinds={kindsOfCosts(costByKind)}
          figureOf={(n) => ({ text: fmtMoney(n) })}
          showsHitRate={false}
        />
      )}
      {isLowerBound ? <div className="mt-xxs">{COST_LOWER_BOUND}</div> : undefined}
    </div>
  );
}

function spendFigureOf(totals: SpendBucket, units: Units): ReactNode {
  if (units === "tokens") {
    if (isEmptyBucket(totals)) {
      return fmtTokens(0);
    }
    const { tokens } = totals;
    return tokens === null ? (
      NOT_MEASURED
    ) : (
      <KindsFigure
        figure={{
          text: fmtTokens(tokens.uncached),
          note: totals.tokens_lower_bound ? TOKENS_LOWER_BOUND : undefined,
        }}
        kinds={kindsOfStats(tokens)}
        figureOf={(n) => ({ text: fmtTokens(n) })}
      />
    );
  }
  if (isEmptyBucket(totals)) {
    return fmtMoney(0);
  }
  const { cost_usd: cost } = totals;
  if (cost === null) {
    return UNKNOWN_FIGURE;
  }
  const text = totals.cost_lower_bound ? fmtMoneyCut(cost) : fmtMoney(cost);
  const note = costNoteOf(totals);
  return note === undefined ? (
    text
  ) : (
    <Hint delay={0} hint={note}>
      {text}
    </Hint>
  );
}

function changeFigureOf({ first, count, period, units }: Scope, reads: Reads): ReactNode {
  const selected = reads.selected.data?.totals;
  if (selected === undefined) {
    return placeholderOf(reads.selected);
  }
  const before = reads.before.data?.totals;
  if (before === undefined) {
    return placeholderOf(reads.before);
  }
  const text = fmtChange(valueOf(selected, units), valueOf(before, units));
  if (text === null || !hasFigureOf(selected, units)) {
    return NONE;
  }
  const earlier = spanNameOf(
    bucketsBefore(first, period, count),
    bucketsBefore(first, period, 1),
    period,
  );
  return <Hint hint={`${fmtOf(units)(valueOf(before, units))} in ${earlier}.`}>{text}</Hint>;
}

function multipleFigureOf({ period, units }: Scope, reads: Reads): ReactNode {
  const selected = reads.selected.data?.totals;
  if (selected === undefined) {
    return placeholderOf(reads.selected);
  }
  const baseline = reads.baseline.data?.totals;
  if (baseline === undefined) {
    return placeholderOf(reads.baseline);
  }
  const average = valueOf(baseline, units) / BASELINE_BUCKETS[period];
  if (average <= 0 || !hasFigureOf(selected, units)) {
    return NONE;
  }
  const note = `${fmtOf(units)(average)} per ${PERIOD_NAMES[period].per} on average in ${BASELINE_WORDS[period].note}.`;
  return <Hint hint={note}>{`${(valueOf(selected, units) / average).toFixed(1)}x`}</Hint>;
}

function averageFigureOf({ count, units }: Scope, reads: Reads): ReactNode {
  const selected = reads.selected.data?.totals;
  if (selected === undefined) {
    return placeholderOf(reads.selected);
  }
  return hasFigureOf(selected, units) ? fmtOf(units)(valueOf(selected, units) / count) : NONE;
}

function countFigureOf(read: UseQueryResult<SpanReport>, pick: (totals: SpendBucket) => number) {
  const totals = read.data?.totals;
  return totals === undefined ? placeholderOf(read) : fmtCount(pick(totals));
}

interface TileProps {
  label: string;
  children: ReactNode;
}

function Tile({ label, children }: TileProps) {
  return (
    <div className="flex min-w-0 flex-col whitespace-nowrap">
      <span className="text-body-sm text-ink-muted">{label}</span>
      <span className="mt-xxs font-heading text-h5 font-semibold text-ink tabular-nums">
        {children}
      </span>
    </div>
  );
}

function sessionFigureOf(session: SpanSession, units: Units): ReactNode {
  const { tokens } = session;
  if (units === "tokens") {
    return tokens === null ? (
      NOT_MEASURED
    ) : (
      <KindsFigure
        figure={{ text: fmtTokens(tokens.uncached) }}
        kinds={kindsOfStats(tokens)}
        figureOf={(n) => ({ text: fmtTokens(n) })}
      />
    );
  }
  if (session.cost_usd !== null) {
    return fmtMoney(session.cost_usd);
  }
  return tokens === null ? NOT_MEASURED : UNPRICED_TEXT;
}

interface SessionRowViewProps {
  session: SpanSession;
  units: Units;
}

function SessionRowView({ session, units }: SessionRowViewProps) {
  const navigate = useNavigate();
  const location = useLocation();
  const issue = issueLinkOf(session.identifier, location);
  const live =
    session.dispatch_id === null
      ? undefined
      : sessionLinkOf(session.identifier, session.dispatch_id, location);
  const share = units === "tokens" ? session.tokens_share : session.cost_share;
  const title = session.issue_title;

  return (
    <TableRow
      look={live === undefined ? "plain" : "link"}
      onClick={live === undefined ? undefined : () => void navigate(live.to, { state: live.state })}
    >
      <TableCell>
        <span className="flex min-w-0 items-center gap-xs">
          <Link
            to={issue.to}
            state={issue.state}
            onClick={(event) => event.stopPropagation()}
            className={cn(LINK, "shrink-0")}
          >
            {session.display_identifier || session.identifier}
          </Link>
          <span className="min-w-0 truncate text-ink-body" title={title}>
            {title}
          </span>
        </span>
      </TableCell>
      <TableCell>
        <NameText name={session.rule_name} label={ruleLabel(session.rule_name)} />
      </TableCell>
      <TableCell>{session.agent_kind}</TableCell>
      <TableCell>
        <RunStatusText status={session.status} />
      </TableCell>
      <TableCell look="number">
        {session.turns === null ? undefined : fmtCount(session.turns)}
      </TableCell>
      <TableCell look="number">{sessionFigureOf(session, units)}</TableCell>
      <TableCell look="number">{share === null ? undefined : fmtPercent(share)}</TableCell>
      <TableCell look="number">
        {live === undefined ? undefined : (
          <Link
            to={live.to}
            state={live.state}
            onClick={(event) => event.stopPropagation()}
            className={LINK}
          >
            {session.status === "running" ? "Open live session" : "Open session"}
          </Link>
        )}
      </TableCell>
    </TableRow>
  );
}

interface SessionsTableProps {
  read: UseQueryResult<SpanReport>;
  units: Units;
}

function SessionsTable({ read, units }: SessionsTableProps) {
  if (read.data === undefined) {
    return read.isError ? (
      <div className="border-t border-line">
        <p className="px-md py-sm text-body-md text-ink-body">Run statistics could not be read.</p>
      </div>
    ) : undefined;
  }
  return (
    <div className="border-t border-line">
      <Table isBare frozenColumns={1} className="table-fixed min-w-240">
        <TableHeader>
          <TableHead>Issue</TableHead>
          <TableHead className="w-28">Rule</TableHead>
          <TableHead className="w-32">Agent</TableHead>
          <TableHead className="w-40">Status</TableHead>
          <TableHead align="right" className="w-18">
            Turns
          </TableHead>
          <TableHead align="right" className="w-26">
            {units === "tokens" ? LABEL_TOKENS : LABEL_EST_COST}
          </TableHead>
          <TableHead align="right" className="w-20">
            Share
          </TableHead>
          <TableHead className="w-40">
            <span className="sr-only">Session</span>
          </TableHead>
        </TableHeader>
        <TableBody>
          {sessionsByShare(read.data.sessions ?? [], units).map((session, index) => (
            <SessionRowView
              key={session.dispatch_id ?? `${session.identifier}-${index}`}
              session={session}
              units={units}
            />
          ))}
        </TableBody>
      </Table>
    </div>
  );
}

/** Reads the selected buckets as one span: its figures and sessions come from the span report of the window the head names, and the change and multiple tiles from the windows just before it. */
export function SelectionBlock({
  buckets,
  period,
  units,
  canZoom,
  onZoomIn,
  onClear,
}: SelectionBlockProps) {
  const first = Date.parse(buckets[0].start);
  const last = Date.parse(buckets[buckets.length - 1].start);
  const count = buckets.length;
  const reads: Reads = {
    selected: useStatsSpanQuery(selectedWindowOf({ first, last }, period)),
    before: useStatsSpanBeforeQuery(previousWindowOf(first, count, period)),
    baseline: useStatsSpanBeforeQuery(count === 1 ? baselineWindowOf(first, period) : null),
  };
  const scope: Scope = { first, count, period, units };
  const totals = reads.selected.data?.totals;
  const isInProgress = buckets.some((bucket) => bucket.in_progress);

  return (
    <>
      <section
        aria-label="Selected period"
        className="flex flex-col gap-md border-t border-line px-md py-sm"
      >
        <div className="flex items-center justify-between gap-sm">
          <h3 className="min-w-0 truncate text-body-md font-medium text-ink">
            {spanNameOf(first, last, period)}
            {isInProgress ? (
              <span className="ml-sm text-body-sm text-ink-muted">In progress</span>
            ) : undefined}
          </h3>
          <span className="flex shrink-0 items-center gap-xs">
            {canZoom ? (
              <Button variant="secondary" onClick={onZoomIn}>
                Zoom in
              </Button>
            ) : undefined}
            <Button variant="ghost" onClick={onClear}>
              <XIcon />
              Clear selection
            </Button>
          </span>
        </div>
        <div className="flex flex-wrap gap-x-xxxl gap-y-md">
          <Tile label={units === "tokens" ? "Tokens" : "Estimated cost"}>
            {totals === undefined ? placeholderOf(reads.selected) : spendFigureOf(totals, units)}
          </Tile>
          <Tile label={previousLabelOf(count, period)}>{changeFigureOf(scope, reads)}</Tile>
          {count === 1 ? (
            <Tile label={BASELINE_WORDS[period].label}>{multipleFigureOf(scope, reads)}</Tile>
          ) : (
            <Tile label={`Average per ${PERIOD_NAMES[period].per}`}>
              {averageFigureOf(scope, reads)}
            </Tile>
          )}
          <Tile label="Turns">{countFigureOf(reads.selected, (t) => t.turns)}</Tile>
          <Tile label="Tool calls">{countFigureOf(reads.selected, (t) => t.tool_calls)}</Tile>
          <Tile label="Errors">{countFigureOf(reads.selected, (t) => t.errors)}</Tile>
        </div>
      </section>
      <SessionsTable read={reads.selected} units={units} />
    </>
  );
}
