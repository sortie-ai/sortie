import { useStateQuery } from "@/api/queries";
import { MONO } from "@/components/ui/mono";
import { usageByKind } from "@/lib/agent-kinds";
import { hasRateSource } from "@/lib/budget";
import { fmtCount, fmtMoney, fmtStamp, fmtTokens } from "@/lib/format";
import { rateText, UNSET_RATE, UNSET_RATE_FROM_INPUT } from "@/lib/rates";
import { Code } from "@/components/Code";
import { Pair, Pairs, Panel } from "@/components/Panel";
import { ScreenHeader } from "@/components/ScreenHeader";
import { StateBadge } from "@/components/StateBadge";

import { DispatchSection } from "./DispatchSection";
import { ReactionsSection } from "./ReactionsSection";
import { joinCode, msText, Note, usageText } from "./workflow-text";

import type { ConfigResponse, TokenRateResponse, WorkflowHealthResponse } from "@/api/types.gen";
import type { ReactNode } from "react";

const NONE = "none";

function standfirstOf(workflow: WorkflowHealthResponse | undefined): string | undefined {
  if (workflow?.last_success_at == null) {
    return undefined;
  }
  const loaded = `Configuration as of ${fmtStamp(workflow.last_success_at)}`;
  if (workflow.status === "fail") {
    return `${loaded}; no new issue starts until the newer edit loads.`;
  }
  return `${loaded}.`;
}

interface StateListProps {
  states: readonly string[] | null;
}

function StateList({ states }: StateListProps) {
  if (states === null || states.length === 0) {
    return NONE;
  }
  return (
    <span className="flex flex-wrap gap-xxs">
      {states.map((state) => (
        <StateBadge key={state} state={state} />
      ))}
    </span>
  );
}

interface ConfigProps {
  config: ConfigResponse;
}

function TrackerPanel({ config }: ConfigProps) {
  const { tracker, polling } = config;
  return (
    <Panel isRuled title="Tracker">
      <Pairs>
        <Pair label="Kind">
          <span className={MONO}>{tracker.kind}</span>
        </Pair>
        {tracker.project ? <Pair label="Project">{tracker.project}</Pair> : undefined}
        <Pair label="Active">
          <StateList states={tracker.active_states} />
        </Pair>
        {tracker.in_progress_state ? (
          <Pair label="In progress">
            <StateBadge state={tracker.in_progress_state} />
          </Pair>
        ) : undefined}
        {tracker.handoff_state ? (
          <Pair label="Handoff">
            <StateBadge state={tracker.handoff_state} />
          </Pair>
        ) : undefined}
        {tracker.no_change_state ? (
          <Pair label="No change">
            <StateBadge state={tracker.no_change_state} />
          </Pair>
        ) : undefined}
        <Pair label="Terminal">
          <StateList states={tracker.terminal_states} />
        </Pair>
        <Pair label="Poll">every {msText(polling.interval_ms)}</Pair>
      </Pairs>
    </Panel>
  );
}

function AgentPanel({ config }: ConfigProps) {
  const { agent, worker } = config;
  const byState = Object.entries(agent.max_concurrent_agents_by_state ?? {}).sort(([a], [b]) =>
    a < b ? -1 : a > b ? 1 : 0,
  );
  const hosts = worker.ssh_hosts ?? [];
  const perHost = worker.max_concurrent_agents_per_host;
  return (
    <Panel isRuled title="Agent">
      <Pairs>
        <Pair label="Kind">
          <span className={MONO}>{agent.kind}</span>
        </Pair>
        <Pair label="Max turns">{fmtCount(agent.max_turns)}</Pair>
        <Pair label="Stall timeout">
          {agent.stall_timeout_ms === null ? "off" : msText(agent.stall_timeout_ms)}
        </Pair>
        <Pair label="Retry backoff">up to {msText(agent.max_retry_backoff_ms)}</Pair>
        <Pair label="Concurrency">
          At most {fmtCount(agent.max_concurrent_agents)} at a time
          {byState.map(([state, cap]) => (
            <span key={state} className="block text-ink-body">
              {state}: at most {fmtCount(cap)}
            </span>
          ))}
        </Pair>
        <Pair label="Token budget">
          {agent.max_tokens === null ? NONE : `${fmtTokens(agent.max_tokens)} per issue`}
        </Pair>
        <Pair label="Money budget">
          {agent.max_cost_usd === null ? NONE : `${fmtMoney(agent.max_cost_usd)} per issue`}
        </Pair>
        <Pair label="Session budget">
          {agent.max_sessions === null ? NONE : `${fmtCount(agent.max_sessions)} per issue`}
        </Pair>
        <Pair label="Usage reporting">
          {usageByKind(config).map(([kind, arrival]) => (
            <span key={kind} className="block">
              {kind}: {usageText(arrival)}
            </span>
          ))}
        </Pair>
        {hosts.length === 0 ? undefined : (
          <Pair label="SSH workers">
            {joinCode(hosts, ", ")}
            <span className="block text-ink-body">
              {perHost === null ? "no limit per host" : `at most ${fmtCount(perHost)} per host`}
            </span>
          </Pair>
        )}
      </Pairs>
    </Panel>
  );
}

interface RateListProps {
  name: string;
  rate: TokenRateResponse;
}

function RateList({ name, rate }: RateListProps) {
  const unsetCache =
    rate.per_kind && rate.input_per_mtok !== null ? UNSET_RATE_FROM_INPUT : UNSET_RATE;
  return (
    <div className="flex flex-col gap-xs">
      <p className={`${MONO} text-ink`}>{name}</p>
      <Pairs>
        <Pair label="Input">{rateText(rate.input_per_mtok, UNSET_RATE)}</Pair>
        <Pair label="Output">{rateText(rate.output_per_mtok, UNSET_RATE)}</Pair>
        <Pair label="Cache read">{rateText(rate.cache_read_per_mtok, unsetCache)}</Pair>
        <Pair label="Cache write">{rateText(rate.cache_write_per_mtok, unsetCache)}</Pair>
      </Pairs>
      {rate.complete || !rate.per_kind ? undefined : (
        <Note>
          Not priced without <Code>input_per_mtok</Code> and <Code>output_per_mtok</Code>.
        </Note>
      )}
    </div>
  );
}

interface TokenRatesPanelProps extends ConfigProps {
  hasSource: boolean;
}

function TokenRatesPanel({ config, hasSource }: TokenRatesPanelProps) {
  const rates = config.token_rates;
  return (
    <Panel isRuled title="Token rates">
      {rates === null ? (
        <p className="text-body-md text-ink-body">
          {hasSource ? "Not set: costs come from the price table." : "Not set: no cost is shown."}
        </p>
      ) : (
        Object.keys(rates)
          .sort()
          .map((name) => <RateList key={name} name={name} rate={rates[name]} />)
      )}
    </Panel>
  );
}

function hasReactions(config: ConfigResponse): boolean {
  return Object.values(config.reactions).some((reaction) => reaction !== undefined);
}

export function WorkflowScreen(): ReactNode {
  const { data } = useStateQuery();
  if (data === undefined) {
    return <ScreenHeader title="Workflow" />;
  }
  const { config } = data;
  if (config === undefined) {
    return <ScreenHeader title="Workflow" standfirst="Sortie serves no configuration." />;
  }

  return (
    <div className="flex flex-col">
      <ScreenHeader title="Workflow" standfirst={standfirstOf(data.health?.workflow)} />
      <div className="grid grid-cols-1 items-start gap-x-xxxl gap-y-xl xl:grid-cols-[minmax(0,1fr)_var(--size-margin)]">
        <div className="flex min-w-0 flex-col gap-xl">
          <DispatchSection
            dispatch={config.dispatch}
            agent={config.agent}
            handoffState={config.tracker.handoff_state}
            hasReactions={hasReactions(config)}
          />
          <ReactionsSection
            reactions={config.reactions}
            handoffState={config.tracker.handoff_state}
          />
        </div>
        <div className="flex min-w-0 flex-col gap-xl">
          <TrackerPanel config={config} />
          <AgentPanel config={config} />
          <TokenRatesPanel config={config} hasSource={hasRateSource(data)} />
        </div>
      </div>
    </div>
  );
}
