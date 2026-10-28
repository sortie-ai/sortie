import { Fragment } from "react";

import { Code } from "@/components/Code";
import { ArrowRightIcon } from "@/components/icons";
import { fmtCount } from "@/lib/format";
import { Panel } from "@/components/Panel";
import { StateBadge } from "@/components/StateBadge";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";

import { Note, selectionText } from "./workflow-text";

import type { ShapeAgent, ShapeDispatch, ShapeRule } from "@/api/types.gen";
import type { ReactNode } from "react";

function ruleName(rule: ShapeRule): ReactNode {
  return rule.name === undefined || rule.name === "" ? (
    <span className="text-ink-muted">unnamed</span>
  ) : (
    <span className="font-mono">{rule.name}</span>
  );
}

interface FallbackProps {
  dispatch: ShapeDispatch;
  agent: ShapeAgent;
}

function Fallback({ dispatch, agent }: FallbackProps) {
  return dispatch.default?.agent_kind ?? agent.kind;
}

interface StepProps {
  marker: ReactNode;
  hasNext: boolean;
  title: ReactNode;
  aside?: ReactNode;
  detail: ReactNode;
}

function Step({ marker, hasNext, title, aside, detail }: StepProps) {
  return (
    <li className="relative flex gap-sm pb-md last:pb-0">
      {hasNext ? (
        <span aria-hidden="true" className="absolute top-6 bottom-0 left-3 w-px bg-line-medium" />
      ) : undefined}
      <span className="relative flex size-6 shrink-0 items-center justify-center rounded-full border border-line-medium bg-canvas text-body-sm text-ink-muted tabular-nums">
        {marker}
      </span>
      <div className="flex min-w-0 flex-col pt-xxs">
        <span className="flex flex-wrap items-baseline gap-x-sm">
          <span className="text-body-md text-ink">{title}</span>
          {aside === undefined ? undefined : (
            <span className="text-body-sm text-ink-muted">{aside}</span>
          )}
        </span>
        <span className="text-body-sm text-ink-muted wrap-anywhere">{detail}</span>
      </div>
    </li>
  );
}

interface Join {
  chain: number;
  stage: string;
  position: number;
  length: number;
}

interface ChainPlan {
  number: number;
  /** The stages this chain lists itself, from its entry up to the stage where it joins another. */
  stages: readonly string[];
  length: number;
  join?: Join;
}

// A chain whose second stage an earlier chain already lists shows as that
// stage's "also entered from" line rather than as a chain of its own.
function planChains(chains: readonly (readonly string[])[]): ChainPlan[] {
  const listed = new Map<string, Omit<Join, "stage">>();
  const plans: ChainPlan[] = [];
  for (const chain of chains) {
    if (chain.length > 1 && listed.has(chain[1])) {
      continue;
    }
    const number = plans.length + 1;
    const stages: string[] = [];
    let join: Join | undefined;
    for (const [index, name] of chain.entries()) {
      const earlier = listed.get(name);
      if (index > 0 && earlier !== undefined) {
        join = { ...earlier, stage: name };
        break;
      }
      listed.set(name, { chain: number, position: index + 1, length: chain.length });
      stages.push(name);
    }
    plans.push(
      join === undefined
        ? { number, stages, length: chain.length }
        : { number, stages, length: chain.length, join },
    );
  }
  return plans;
}

interface ChainListProps {
  plan: ChainPlan;
  rules: readonly ShapeRule[];
  handoff: string;
  hasReactions: boolean;
}

function ChainList({ plan, rules, handoff, hasReactions }: ChainListProps) {
  const { stages, join } = plan;
  const steps: ReactNode[] = stages.map((name, index) => {
    const rule = rules.find((candidate) => candidate.name === name);
    const previous = stages[index - 1];
    const alsoFrom =
      index === 0 ? [] : rules.filter((other) => other.next === name && other.name !== previous);
    return (
      <Step
        key={name}
        marker={index + 1}
        hasNext
        title={<span className="font-mono">{name}</span>}
        aside={rule === undefined ? undefined : `${rule.agent_kind} agent`}
        detail={
          <>
            {index === 0 ? "Starts on " : undefined}
            {rule === undefined ? undefined : selectionText(rule)}
            {alsoFrom.length === 0 ? undefined : (
              <span className="block">
                Also entered from{" "}
                {alsoFrom.map((other, at) => (
                  <Fragment key={at}>
                    {at === 0 ? undefined : ", "}
                    {ruleName(other)} ({selectionText(other)}, {other.agent_kind} agent)
                  </Fragment>
                ))}
              </span>
            )}
          </>
        }
      />
    );
  });
  steps.push(
    join === undefined ? (
      <Step
        key="handoff"
        marker={<ArrowRightIcon />}
        hasNext={false}
        title={<StateBadge state={handoff} />}
        detail={`Handoff state, then ${hasReactions ? "the reactions take over" : "a person takes it from there"}`}
      />
    ) : (
      <Step
        key="join"
        marker={<ArrowRightIcon />}
        hasNext={false}
        title={
          <>
            Joins chain {join.chain} at <span className="font-mono">{join.stage}</span>
          </>
        }
        detail={`Stage ${join.position} of ${join.length}`}
      />
    ),
  );
  return (
    <div className="flex flex-col gap-sm">
      <h3 className="text-body-md font-medium text-ink">
        Chain {plan.number}
        <span className="ml-xs font-regular text-ink-muted">{fmtCount(plan.length)} stages</span>
      </h3>
      <ol>{steps}</ol>
    </div>
  );
}

function hopNote(dispatch: ShapeDispatch): ReactNode {
  return (
    <Note>
      At most {fmtCount(dispatch.hop_ceiling)} automatic hops in a row per issue (
      <Code>dispatch.max_consecutive_hops</Code>
      {dispatch.max_consecutive_hops === null ? ", default" : undefined}).
    </Note>
  );
}

export interface DispatchSectionProps {
  dispatch: ShapeDispatch;
  agent: ShapeAgent;
  handoffState: string | undefined;
  hasReactions: boolean;
}

export function DispatchSection({
  dispatch,
  agent,
  handoffState,
  hasReactions,
}: DispatchSectionProps) {
  const rules = dispatch.rules ?? [];
  const chains = dispatch.chains ?? [];
  const handoff =
    handoffState === undefined || handoffState === "" ? "the handoff state" : handoffState;

  if (rules.length === 0) {
    return (
      <Panel title="Dispatch">
        <Note>
          No dispatch rules: every issue runs on <Fallback dispatch={dispatch} agent={agent} />.
        </Note>
      </Panel>
    );
  }

  const inChains = new Set(chains.flat());
  const others = rules.filter((rule) => rule.name === undefined || !inChains.has(rule.name));
  const hasCatchAll = rules.some((rule) => rule.selected_by === "catch_all");
  const lists = planChains(chains).map((plan) => (
    <ChainList
      key={plan.number}
      plan={plan}
      rules={rules}
      handoff={handoff}
      hasReactions={hasReactions}
    />
  ));

  return (
    <Panel title="Dispatch">
      {others.length === 0 ? undefined : (
        <Table>
          <TableHeader>
            <TableHead>Rule</TableHead>
            <TableHead>Selected by</TableHead>
            <TableHead>Agent</TableHead>
          </TableHeader>
          <TableBody>
            {others.map((rule, index) => (
              <TableRow key={index}>
                <TableCell>{ruleName(rule)}</TableCell>
                <TableCell>{selectionText(rule)}</TableCell>
                <TableCell>{rule.agent_kind}</TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      )}
      {lists}
      {hasCatchAll ? undefined : (
        <Note>
          Issues no rule selects run on <Fallback dispatch={dispatch} agent={agent} />.
        </Note>
      )}
      {lists.length === 0 ? (
        <Note>No stage chains: every rule ends on {handoff}.</Note>
      ) : (
        hopNote(dispatch)
      )}
    </Panel>
  );
}
