import { ApiError } from "@/api/client";
import { useStateQuery, useStatsQuery } from "@/api/queries";
import { ServedText } from "@/components/ServedText";
import { StatusMark } from "@/components/StatusMark";
import { hasRateSource } from "@/lib/budget";
import { cn } from "@/lib/cn";
import { agentLabel, isNamedRuleGroup, modelLabel, ruleLabel, startLabel } from "@/lib/labels";
import { runStatus } from "@/lib/run-status";

import { ChainFunnel } from "./ChainFunnel";
import { GroupTable } from "./GroupTable";
import { OutcomeBar } from "./OutcomeBar";
import { RangePicker } from "./RangePicker";
import { SkillTable } from "./SkillTable";
import { SpendChart } from "./SpendChart";
import { SummaryTiles } from "./SummaryTiles";
import { useAnalyticsRange } from "./analytics-range";

import type { Group, Report } from "@/api/types.gen";
import type { NameColumn } from "./GroupTable";

const MESSAGE = "text-body-md text-ink-body";

function isUnavailable(error: unknown): boolean {
  return error instanceof ApiError && error.status === 503 && error.code === "history_unavailable";
}

interface NameTextProps {
  name: string;
  label: string;
}

// A name the client has no wording for reads as served, in the monospace face.
function NameText({ name, label }: NameTextProps) {
  return (
    <span className={cn(label === name && "font-mono text-body-sm")}>
      <ServedText text={label} />
    </span>
  );
}

const RULE_COLUMN: NameColumn<Group> = {
  header: "Rule",
  cell: (row) => <NameText name={row.name} label={ruleLabel(row.name)} />,
  sortText: (row) => ruleLabel(row.name),
};
const MODEL_COLUMN: NameColumn<Group> = {
  header: "Model",
  cell: (row) => <NameText name={row.name} label={modelLabel(row.name)} />,
  sortText: (row) => modelLabel(row.name),
};
const AGENT_COLUMN: NameColumn<Group> = {
  header: "Agent",
  cell: (row) => <NameText name={row.name} label={agentLabel(row.name)} />,
  sortText: (row) => agentLabel(row.name),
};
const STATUS_COLUMN: NameColumn<Group> = {
  header: "Status",
  cell: (row) => {
    const { word, mark } = runStatus(row.name);
    return mark === null ? (
      <ServedText text={word} />
    ) : (
      <StatusMark mark={mark} label={<ServedText text={word} />} />
    );
  },
  sortText: (row) => runStatus(row.name).word,
};
const TRIGGER_COLUMN: NameColumn<Group> = {
  header: "Trigger",
  cell: (row) => <ServedText text={startLabel(row.name, "trigger")} />,
  sortText: (row) => startLabel(row.name, "trigger"),
};
const ORIGIN_COLUMN: NameColumn<Group> = {
  header: "Started by",
  cell: (row) => <ServedText text={startLabel(row.name, "origin")} />,
  sortText: (row) => startLabel(row.name, "origin"),
};

interface BreakdownsProps {
  report: Report;
  hasRules: boolean;
  hasRates: boolean;
}

function Breakdowns({ report, hasRules, hasRates }: BreakdownsProps) {
  const tier = report.schema_tier;
  const common = { tier, hasRates };
  const byRule = report.by_rule ?? [];
  const byModel = report.by_model ?? [];
  const byAdapter = report.by_adapter ?? [];
  const byStatus = report.by_status ?? [];
  const byTrigger = report.by_trigger ?? [];
  const byOrigin = report.by_origin ?? [];
  const skillCalls = report.skill_calls;
  const showByRule =
    hasRules && byRule.some((group) => byRule.length > 1 || isNamedRuleGroup(group.name));

  return (
    <>
      {showByRule ? (
        <GroupTable
          title="By rule"
          sortParam="by-rule"
          nameColumns={[RULE_COLUMN]}
          rows={byRule}
          {...common}
        />
      ) : undefined}
      {byModel.length > 0 ? (
        <GroupTable
          title="By model"
          sortParam="by-model"
          nameColumns={[MODEL_COLUMN]}
          rows={byModel}
          {...common}
        />
      ) : undefined}
      {byAdapter.length > 0 ? (
        <GroupTable
          title="By agent"
          sortParam="by-agent"
          nameColumns={[AGENT_COLUMN]}
          rows={byAdapter}
          {...common}
        />
      ) : undefined}
      {byStatus.length > 0 ? (
        <GroupTable
          title="By run status"
          sortParam="by-status"
          nameColumns={[STATUS_COLUMN]}
          rows={byStatus}
          measure="share"
          {...common}
        />
      ) : undefined}
      {byTrigger.length > 0 ? (
        <GroupTable
          title="By retry trigger"
          sortParam="by-trigger"
          nameColumns={[TRIGGER_COLUMN]}
          rows={byTrigger}
          {...common}
        />
      ) : undefined}
      {byOrigin.length > 0 ? (
        <GroupTable
          title="Why runs started"
          sortParam="by-origin"
          nameColumns={[ORIGIN_COLUMN]}
          rows={byOrigin}
          {...common}
        />
      ) : undefined}
      {skillCalls !== null && (skillCalls.by_skill ?? []).length > 0 ? (
        <SkillTable report={skillCalls} />
      ) : undefined}
    </>
  );
}

export function AnalyticsScreen() {
  const { data: state } = useStateQuery();
  const config = state?.config;
  const [range] = useAnalyticsRange();
  const stats = useStatsQuery(range);
  const report = stats.data;

  return (
    <div className="flex flex-col gap-lg">
      <div className="flex items-center justify-between gap-md">
        <h1 className="font-heading text-h4 font-semibold text-ink">Analytics</h1>
        <RangePicker className="md:hidden" />
      </div>
      {report === undefined ? (
        stats.isError ? (
          <p className={MESSAGE}>
            {isUnavailable(stats.error)
              ? "Run history is not available."
              : "Run statistics could not be read."}
          </p>
        ) : (
          <p className={MESSAGE}>Loading</p>
        )
      ) : (
        <>
          {(report.warnings ?? []).map((warning, index) => (
            <p
              key={index}
              className="rounded-lg bg-warning-container px-md py-sm text-body-md text-ink"
            >
              <ServedText text={warning} />
            </p>
          ))}
          {report.summary.runs === 0 ? (
            <p className={MESSAGE}>
              {range === "all" ? "No run has finished yet." : "No run finished in this range."}
            </p>
          ) : (
            <div key={range} className="flex flex-col gap-xl">
              <SummaryTiles report={report} hasCost={hasRateSource(state)} />
              <OutcomeBar summary={report.summary} />
              {hasRateSource(state) &&
              range !== "24h" &&
              report.spend !== null &&
              (report.spend.buckets ?? []).length > 0 ? (
                <SpendChart spend={report.spend} since={report.since} />
              ) : undefined}
              {(report.chain_funnels ?? []).map((funnel) => (
                <ChainFunnel key={funnel.stages?.[0]?.rule} funnel={funnel} config={config} />
              ))}
              <Breakdowns
                report={report}
                hasRules={(config?.dispatch.rules ?? []).length > 0}
                hasRates={hasRateSource(state)}
              />
            </div>
          )}
        </>
      )}
    </div>
  );
}
