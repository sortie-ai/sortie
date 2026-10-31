import { NotedFigure } from "@/components/NotedFigure";
import { Pair, Pairs, Panel } from "@/components/Panel";
import { StatusMark } from "@/components/StatusMark";
import {
  LABEL_AGENT_REPORTED,
  LABEL_EST_COST,
  LABEL_MEASURED_FROM,
  LABEL_MODEL_NOT_REPORTED,
  LABEL_PRICED_FROM,
  LABEL_TOKENS,
  NOT_MEASURED,
  liveCostFigure,
  NOT_RECORDED,
  PROVENANCE_UNKNOWN,
  RUN_TOKENS_LOWER_BOUND,
  partCostFigure,
  priceSourceFigure,
  provenanceLines,
  runBaseCostFigure,
  runTokensFigure,
  vendorAmountFigure,
} from "@/lib/figures";
import { fmtCount, fmtDuration } from "@/lib/format";

import { ToolText } from "./ToolLine";

import type {
  ConfigResponse,
  ModelUsageResponse,
  RunResponse,
  RunningEntryResponse,
  SkillCall,
  TokenInfo,
  VendorAmountResponse,
} from "@/api/types.gen";
import type { SessionRun } from "@/components/use-session-run";
import type { ReactNode } from "react";

export type ResolvedSession = Extract<SessionRun, { kind: "live" | "finished" }>;

function figureText(n: number | null): string {
  return n === null ? NOT_MEASURED : fmtCount(n);
}

function liveTokensOf(total: number | null): ReactNode {
  return total === null ? (
    NOT_MEASURED
  ) : (
    <>
      <strong className="font-bold">{fmtCount(total)}</strong> so far
    </>
  );
}

/** Input arrives cache-inclusive, so the fresh input leaves out both cache counters. */
function freshInputOf(tokens: TokenInfo): number | null {
  const { input_tokens: input, cache_read_tokens: read, cache_write_tokens: write } = tokens;
  if (input === null || read === null || write === null) {
    return null;
  }
  return Math.max(0, input - read - write);
}

interface ClassPairsProps {
  tokens: TokenInfo;
}

function ClassPairs({ tokens }: ClassPairsProps) {
  return (
    <>
      <Pair label="Input">{figureText(freshInputOf(tokens))}</Pair>
      <Pair label="Output">{figureText(tokens.output_tokens)}</Pair>
      <Pair label="Cache read">{figureText(tokens.cache_read_tokens)}</Pair>
      <Pair label="Cache write">{figureText(tokens.cache_write_tokens)}</Pair>
    </>
  );
}

/** Amounts of one unit sum; a model split of the served list is not shown here. */
function amountsByUnit(amounts: readonly VendorAmountResponse[]): VendorAmountResponse[] {
  const byUnit = new Map<string, VendorAmountResponse>();
  for (const entry of amounts) {
    const before = byUnit.get(entry.unit);
    byUnit.set(entry.unit, {
      unit: entry.unit,
      model: "",
      amount: (before?.amount ?? 0) + entry.amount,
      lower_bound: (before?.lower_bound ?? false) || entry.lower_bound,
    });
  }
  return [...byUnit.values()];
}

interface FinishedUsagePairsProps {
  run: RunResponse;
}

function FinishedUsagePairs({ run }: FinishedUsagePairsProps) {
  const total = run.uncached_tokens;
  const lines = provenanceLines(run);
  const reported = amountsByUnit(run.vendor_amounts ?? []);
  const tokens = runTokensFigure(run);

  return (
    <>
      {run.usage_completeness === "unmeasured" || total === null ? (
        <Pair label={LABEL_TOKENS}>
          <NotedFigure figure={tokens} />
        </Pair>
      ) : (
        <>
          <ClassPairs tokens={run.tokens} />
          <Pair label={LABEL_TOKENS}>
            <strong className="font-bold">
              <NotedFigure
                figure={{
                  text: fmtCount(total),
                  note:
                    run.usage_completeness === "lower_bound" ? RUN_TOKENS_LOWER_BOUND : undefined,
                }}
              />
            </strong>
          </Pair>
        </>
      )}
      <Pair label={LABEL_EST_COST}>
        <NotedFigure figure={runBaseCostFigure(run)} />
      </Pair>
      {lines === null ? (
        <Pair label={LABEL_MEASURED_FROM}>
          <span className="text-ink-muted">
            <NotedFigure figure={{ text: NOT_RECORDED, note: PROVENANCE_UNKNOWN }} />
          </span>
        </Pair>
      ) : lines.length === 0 ? undefined : (
        <Pair label={LABEL_MEASURED_FROM}>
          <span className="flex flex-col">
            {lines.map((line) => (
              <span key={line}>{line}</span>
            ))}
          </span>
        </Pair>
      )}
      {reported.length === 0 ? undefined : (
        <Pair label={LABEL_AGENT_REPORTED}>
          <span className="flex flex-col">
            {reported.map((amount) => (
              <span key={amount.unit}>
                <NotedFigure figure={vendorAmountFigure(amount, "full")} />
              </span>
            ))}
          </span>
        </Pair>
      )}
    </>
  );
}

interface LiveUsagePairsProps {
  running: RunningEntryResponse;
}

function LiveUsagePairs({ running }: LiveUsagePairsProps) {
  return (
    <>
      {running.tokens_measured ? (
        <>
          <ClassPairs tokens={running.tokens} />
          <Pair label={LABEL_TOKENS}>{liveTokensOf(running.uncached_tokens)}</Pair>
        </>
      ) : (
        <Pair label={LABEL_TOKENS}>{NOT_MEASURED}</Pair>
      )}
      <Pair label={LABEL_EST_COST}>
        <NotedFigure
          figure={liveCostFigure(running.cost_status, running.estimated_cost_usd, "session")}
        />
      </Pair>
    </>
  );
}

export interface UsagePanelProps {
  session: ResolvedSession;
}

export function UsagePanel({ session }: UsagePanelProps) {
  return (
    <Panel title="Usage">
      <Pairs>
        {session.kind === "finished" ? (
          <FinishedUsagePairs run={session.run} />
        ) : (
          <LiveUsagePairs running={session.running} />
        )}
      </Pairs>
    </Panel>
  );
}

export interface ModelCostPanelProps {
  parts: readonly ModelUsageResponse[];
}

export function ModelCostPanel({ parts }: ModelCostPanelProps) {
  return (
    <Panel title="Cost per model">
      {parts.map((part) => {
        const pricedFrom = priceSourceFigure(part);
        return (
          <div key={part.model} className="flex flex-col gap-xs">
            {part.model === "" ? (
              <p className="text-body-md font-medium text-ink">{LABEL_MODEL_NOT_REPORTED}</p>
            ) : (
              <p className="font-mono text-body-sm text-ink wrap-anywhere">{part.model}</p>
            )}
            <Pairs>
              <Pair label={LABEL_TOKENS}>{fmtCount(part.uncached_tokens)}</Pair>
              <Pair label={LABEL_EST_COST}>
                <NotedFigure figure={partCostFigure(part)} />
              </Pair>
              {pricedFrom === null ? undefined : (
                <Pair label={LABEL_PRICED_FROM}>
                  <span className={part.price_source === null ? "text-ink-muted" : undefined}>
                    <NotedFigure figure={pricedFrom} />
                  </span>
                </Pair>
              )}
            </Pairs>
          </div>
        );
      })}
    </Panel>
  );
}

export interface SkillsPanelProps {
  skills: readonly SkillCall[];
}

export function SkillsPanel({ skills }: SkillsPanelProps) {
  return (
    <Panel title="Skills">
      <ol className="grid grid-cols-[auto_minmax(0,1fr)] items-baseline gap-x-md gap-y-xs text-body-md">
        {skills.map((call, index) => (
          <li key={index} className="contents">
            <span className="text-right text-ink-muted tabular-nums">{index + 1}</span>
            {call.tool_error === true ? (
              <StatusMark
                mark="failed"
                label={<ToolText text={call.skill} />}
                className="min-w-0 *:min-w-0"
              />
            ) : (
              <span className="min-w-0 text-ink">
                <ToolText text={call.skill} />
              </span>
            )}
          </li>
        ))}
      </ol>
    </Panel>
  );
}

export interface SessionPanelProps {
  session: ResolvedSession;
  /** The state's configuration; absent until the state loads. */
  config: ConfigResponse | undefined;
}

function ShortValue({ full, shown }: { full: string; shown: string }) {
  return (
    <span title={full} className="font-mono text-body-sm">
      {shown}
    </span>
  );
}

function lastSegmentOf(path: string): string {
  return path.split(/[\\/]/).filter(Boolean).pop() ?? path;
}

export function SessionPanel({ session, config }: SessionPanelProps) {
  const isLive = session.kind === "live";
  const source = isLive ? session.running : session.run;
  const stallTimeoutMs = config?.agent.stall_timeout_ms ?? null;

  return (
    <Panel title="Session">
      <Pairs>
        <Pair label="Configured">{source.configured_model || "agent default"}</Pair>
        <Pair label="Reported model">{source.model_name || "not reported"}</Pair>
        {source.ssh_host ? <Pair label="Host">{`${source.ssh_host} (SSH)`}</Pair> : undefined}
        {isLive ? (
          <Pair label="Workspace">
            <ShortValue
              full={session.running.workspace_path}
              shown={lastSegmentOf(session.running.workspace_path)}
            />
          </Pair>
        ) : undefined}
        {source.session_id ? (
          <Pair label="Session ID">
            <ShortValue full={source.session_id} shown={source.session_id.slice(0, 8)} />
          </Pair>
        ) : undefined}
        {source.workflow_file ? <Pair label="Workflow">{source.workflow_file}</Pair> : undefined}
        {isLive && stallTimeoutMs !== null ? (
          <Pair label="Stall timeout">{fmtDuration(stallTimeoutMs / 1000)}</Pair>
        ) : undefined}
      </Pairs>
    </Panel>
  );
}
