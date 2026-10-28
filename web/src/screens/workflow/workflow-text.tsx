import { Fragment } from "react";

import { Code } from "@/components/Code";
import { TrackerLabel } from "@/components/TrackerLabel";
import { fmtDuration } from "@/lib/format";

import type { ShapeMatch, ShapePriority, ShapeRule } from "@/api/types.gen";
import type { ReactNode } from "react";

export function joinLabels(labels: readonly string[], separator: string): ReactNode {
  return joinNodes(
    labels.map((label) => <TrackerLabel key={label}>{label}</TrackerLabel>),
    separator,
  );
}

/** Joins nodes with a separator text, keying each so the result renders as siblings. */
export function joinNodes(nodes: readonly ReactNode[], separator: string): ReactNode {
  return nodes.map((node, index) => (
    <Fragment key={index}>
      {index === 0 ? undefined : separator}
      {node}
    </Fragment>
  ));
}

export function joinCode(values: readonly string[], separator: string): ReactNode {
  return values.map((value, index) => (
    <Fragment key={index}>
      {index === 0 ? undefined : separator}
      <Code>{value}</Code>
    </Fragment>
  ));
}

function priorityClause({ op, value, values }: ShapePriority): ReactNode {
  switch (op) {
    case "eq":
      return `priority ${value}`;
    case "in":
      return `priority ${(values ?? []).join(" or ")}`;
    case "lt":
      return `priority below ${value}`;
    case "lte":
      return `priority ${value} or below`;
    case "gt":
      return `priority above ${value}`;
    case "gte":
      return `priority ${value} or above`;
    default:
      return `priority ${op} ${value}`;
  }
}

function matchClauses(match: ShapeMatch): ReactNode[] {
  const clauses: ReactNode[] = [];
  if (match.labels !== undefined && match.labels.length > 0) {
    clauses.push(joinLabels(match.labels, " or "));
  }
  if (match.issue_type !== undefined && match.issue_type.length > 0) {
    clauses.push(`issue type ${match.issue_type.join(" or ")}`);
  }
  if (match.priority !== undefined) {
    clauses.push(priorityClause(match.priority));
  }
  if (match.identifier !== undefined && match.identifier.length > 0) {
    clauses.push(<>identifier {joinCode(match.identifier, " or ")}</>);
  }
  if (match.assignee !== undefined && match.assignee.length > 0) {
    clauses.push(`assignee ${match.assignee.join(" or ")}`);
  }
  if (match.title !== undefined && match.title.length > 0) {
    clauses.push(`title ${match.title.map((phrase) => `"${phrase}"`).join(" or ")}`);
  }
  return clauses;
}

/** Words how a dispatch rule selects an issue; a selection kind the client has no wording for reads as served. */
export function selectionText(rule: Pick<ShapeRule, "selected_by" | "stage" | "match">): ReactNode {
  switch (rule.selected_by) {
    case "stage":
      return rule.stage === undefined ? (
        rule.selected_by
      ) : (
        <TrackerLabel>{rule.stage}</TrackerLabel>
      );
    case "catch_all":
      return "as the catch-all";
    case "match": {
      const clauses = rule.match === undefined ? [] : matchClauses(rule.match);
      return clauses.length === 0 ? rule.selected_by : joinNodes(clauses, " and ");
    }
    default:
      return rule.selected_by;
  }
}

/** Words whether an agent kind reports token usage; an arrival the client has no wording for reads "not declared". */
export function usageText(arrival: string): string {
  switch (arrival) {
    case "incremental":
    case "turn_end":
      return "reports tokens";
    case "none":
      return "reports none";
    default:
      return "not declared";
  }
}

export function msText(ms: number): string {
  return fmtDuration(ms / 1000);
}

/** Words where a reaction escalates; an escalation the client has no wording for reads as served. */
export function escalationText(escalation: string, label: string | undefined): ReactNode {
  switch (escalation) {
    case "label":
      return label === undefined ? "label" : <TrackerLabel>{label}</TrackerLabel>;
    case "none":
      return "–";
    default:
      return escalation;
  }
}

export interface NoteProps {
  children: ReactNode;
}

export function Note({ children }: NoteProps) {
  return <p className="text-body-md text-ink-body">{children}</p>;
}
