import { NotedFigure } from "@/components/NotedFigure";
import { Pair, Pairs, Panel } from "@/components/Panel";
import { StatusMark } from "@/components/StatusMark";
import { NameText } from "@/components/NameText";
import { KIND_ORDER, KindLabel, cacheHitRate, freshInputOf } from "@/components/TokenKinds";
import { MONO } from "@/components/ui/mono";
import {
  LABEL_AGENT_REPORTED,
  LABEL_CACHE_HIT_RATE,
  LABEL_EST_COST,
  LABEL_MEASURED_FROM,
  LABEL_MODEL,
  LABEL_PRICED_FROM,
  LABEL_TOKENS,
  MODEL_NOT_REPORTED,
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
import { fmtCount, fmtDuration, fmtPercent } from "@/lib/format";

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
import type { TokenKind } from "@/components/TokenKinds";
import type { SessionRun } from "@/components/use-session-run";
import type { ReactNode } from "react";

export type ResolvedSession = Extract<SessionRun, { kind: "live" | "finished" }>;

function figureText(n: number | null): string {
  return n === null ? NOT_MEASURED : fmtCount(n);
}

function counterOf(tokens: TokenInfo, key: TokenKind["key"]): number | null {
  switch (key) {
    case "input":
      return freshInputOf(tokens);
    case "output":
      return tokens.output_tokens;
    case "cache_write":
      return tokens.cache_write_tokens;
    case "cache_read":
      return tokens.cache_read_tokens;
  }
}

interface KindPairsProps {
  tokens: TokenInfo;
  total: ReactNode;
}

function KindPairs({ tokens, total }: KindPairsProps) {
  const rate = cacheHitRate(tokens.input_tokens, tokens.cache_read_tokens);
  return (
    <>
      {KIND_ORDER.map((kind) => (
        <Pair key={kind.key} label={<KindLabel kind={kind} />}>
          {figureText(counterOf(tokens, kind.key))}
        </Pair>
      ))}
      {rate === null ? undefined : <Pair label={LABEL_CACHE_HIT_RATE}>{fmtPercent(rate)}</Pair>}
      <Pair label={LABEL_TOKENS}>{total}</Pair>
    </>
  );
}

interface PricedFromPairProps {
  part: ModelUsageResponse;
}

function PricedFromPair({ part }: PricedFromPairProps) {
  const pricedFrom = priceSourceFigure(part);
  return pricedFrom === null ? undefined : (
    <Pair label={LABEL_PRICED_FROM}>
      <span className={part.price_source === null ? "text-ink-muted" : undefined}>
        <NotedFigure figure={pricedFrom} />
      </span>
    </Pair>
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
  const parts = run.model_usage ?? [];
  const onlyPart = parts.length === 1 ? parts[0] : undefined;

  return (
    <>
      {run.usage_completeness === "unmeasured" || total === null ? (
        <Pair label={LABEL_TOKENS}>
          <NotedFigure figure={tokens} />
        </Pair>
      ) : (
        <KindPairs
          tokens={run.tokens}
          total={
            <NotedFigure
              figure={{
                text: fmtCount(total),
                note: run.usage_completeness === "lower_bound" ? RUN_TOKENS_LOWER_BOUND : undefined,
              }}
            />
          }
        />
      )}
      <Pair label={LABEL_EST_COST}>
        <NotedFigure figure={runBaseCostFigure(run)} />
      </Pair>
      {onlyPart === undefined ? undefined : <PricedFromPair part={onlyPart} />}
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
        <KindPairs
          tokens={running.tokens}
          total={
            running.uncached_tokens === null
              ? NOT_MEASURED
              : `${fmtCount(running.uncached_tokens)} so far`
          }
        />
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
    <Panel isRuled title="Usage">
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
    <Panel isRuled title="Cost per model">
      <div className="flex flex-col gap-lg">
        {parts.map((part) => (
          <div key={part.model}>
            <Pairs>
              <Pair label={LABEL_MODEL}>
                {part.model === "" ? (
                  MODEL_NOT_REPORTED
                ) : (
                  <span className={`${MONO} wrap-anywhere`}>{part.model}</span>
                )}
              </Pair>
              <KindPairs tokens={part.tokens} total={fmtCount(part.uncached_tokens)} />
              <Pair label={LABEL_EST_COST}>
                <NotedFigure figure={partCostFigure(part)} />
              </Pair>
              <PricedFromPair part={part} />
            </Pairs>
          </div>
        ))}
      </div>
    </Panel>
  );
}

export interface SkillsPanelProps {
  skills: readonly SkillCall[];
}

export function SkillsPanel({ skills }: SkillsPanelProps) {
  return (
    <Panel isRuled title="Skills">
      <ol className="grid grid-cols-[auto_minmax(0,1fr)] items-baseline gap-x-md gap-y-xs text-body-md">
        {skills.map((call, index) => (
          <li key={index} className="contents">
            <span className="text-right text-ink-muted tabular-nums">{index + 1}</span>
            {call.tool_error === true ? (
              <StatusMark
                mark="failed"
                label={<ToolText text={call.skill} isMono />}
                className="min-w-0 *:min-w-0"
              />
            ) : (
              <span className="min-w-0 text-ink">
                <ToolText text={call.skill} isMono />
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
    <span title={full} className={MONO}>
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
    <Panel isRuled title="Details">
      <Pairs>
        <Pair label="Configured">
          {source.configured_model ? <NameText name={source.configured_model} /> : "agent default"}
        </Pair>
        <Pair label="Reported model">
          {source.model_name ? <NameText name={source.model_name} /> : MODEL_NOT_REPORTED}
        </Pair>
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
