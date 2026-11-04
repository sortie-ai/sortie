import { useState } from "react";

import { ApiError } from "@/api/client";
import { useStateQuery, useStatsQuery } from "@/api/queries";
import { ScreenHeader } from "@/components/ScreenHeader";
import { ServedText } from "@/components/ServedText";
import { StatusMark } from "@/components/StatusMark";
import { NameText } from "@/components/NameText";
import { UnitsSwitch } from "@/components/UnitsSwitch";
import { agentLabel, isNamedRuleGroup, modelLabel, ruleLabel, startLabel } from "@/lib/labels";
import { runStatus } from "@/lib/run-status";
import { useUnits } from "@/lib/units";
import { Panel } from "@/components/Panel";

import { ChainFunnel } from "./ChainFunnel";
import { GroupTable } from "./GroupTable";
import { OutcomeBar } from "./OutcomeBar";
import { RangePicker } from "./RangePicker";
import { SkillTable } from "./SkillTable";
import { SummaryTiles } from "./SummaryTiles";
import { UsageChart } from "./UsageChart";
import { RANGE_OPTIONS, useAnalyticsRange } from "./analytics-range";

import type { AnalyticsRange } from "@/api/queries";
import type { Group, Report } from "@/api/types.gen";
import type { Units } from "@/lib/units";
import type { NameColumn } from "./GroupTable";

const MESSAGE = "text-body-md text-ink-body";

function isUnavailable(error: unknown): boolean {
  return error instanceof ApiError && error.status === 503 && error.code === "history_unavailable";
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
  units: Units;
}

function Breakdowns({ report, hasRules, units }: BreakdownsProps) {
  const tier = report.schema_tier;
  const common = { tier, units };
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

function standfirstOf(range: AnalyticsRange): string {
  const label = RANGE_OPTIONS.find((option) => option.value === range)?.label ?? "";
  return range === "all" ? "Every run on record." : `Runs finished in the ${label.toLowerCase()}.`;
}

export function AnalyticsScreen() {
  const { data: state } = useStateQuery();
  const config = state?.config;
  const [range] = useAnalyticsRange();
  const stats = useStatsQuery(range);
  const report = stats.data;
  const [hasLoadedBefore, setHasLoadedBefore] = useState(false);
  if (report !== undefined && !hasLoadedBefore) {
    setHasLoadedBefore(true);
  }
  const hasReport =
    (hasLoadedBefore || report !== undefined) && !(stats.isError && report === undefined);
  const unitsView = useUnits(state, hasReport);
  const units = unitsView.shown;

  return (
    <div className="flex flex-col">
      <ScreenHeader
        title="Analytics"
        standfirst={standfirstOf(range)}
        actions={
          <div className="flex flex-wrap items-center gap-xs">
            <UnitsSwitch view={unitsView} />
            <RangePicker />
          </div>
        }
      />
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
              className="rounded-xl bg-warning-container px-md py-sm text-body-md text-ink"
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
              <Panel title="Summary">
                <SummaryTiles report={report} units={units} />
              </Panel>
              <OutcomeBar summary={report.summary} />
              {range !== "24h" &&
              report.schema_tier === "full" &&
              (report.spend.buckets ?? []).length > 0 ? (
                <UsageChart spend={report.spend} since={report.since} units={units} />
              ) : undefined}
              {(report.chain_funnels ?? []).map((funnel) => (
                <ChainFunnel key={funnel.stages?.[0]?.rule} funnel={funnel} config={config} />
              ))}
              <Breakdowns
                report={report}
                hasRules={(config?.dispatch.rules ?? []).length > 0}
                units={units}
              />
            </div>
          )}
        </>
      )}
    </div>
  );
}
