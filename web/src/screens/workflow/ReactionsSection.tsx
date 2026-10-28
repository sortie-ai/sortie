import { fmtCount } from "@/lib/format";
import { Panel } from "@/components/Panel";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";

import { escalationText, joinLabels } from "./workflow-text";

import type { ShapeReactions } from "@/api/types.gen";
import type { ReactNode } from "react";

interface Row {
  kind: string;
  limit: ReactNode;
  escalation: ReactNode;
}

function fixes(count: number): string {
  return `${fmtCount(count)} ${count === 1 ? "fix" : "fixes"}`;
}

function fixRuns(count: number): string {
  return `${fmtCount(count)} ${count === 1 ? "fix run" : "fix runs"}`;
}

function reactionRows(reactions: ShapeReactions): Row[] {
  const escalation = (reaction: { escalation: string; escalation_label?: string }) =>
    escalationText(reaction.escalation, reaction.escalation_label);
  const rows: Row[] = [];
  const {
    ci_failure,
    review_comments,
    bot_review,
    merge_conflicts,
    auto_merge,
    merge_completion,
    label_commands,
  } = reactions;
  if (ci_failure !== undefined) {
    rows.push({
      kind: "ci_failure",
      limit: fixes(ci_failure.max_retries),
      escalation: escalation(ci_failure),
    });
  }
  if (review_comments !== undefined) {
    rows.push({
      kind: "review_comments",
      limit: fixRuns(review_comments.max_continuation_turns),
      escalation: escalation(review_comments),
    });
  }
  if (bot_review !== undefined) {
    rows.push({
      kind: "bot_review",
      limit: fixRuns(bot_review.max_continuation_turns),
      escalation: escalation(bot_review),
    });
  }
  if (merge_conflicts !== undefined) {
    rows.push({
      kind: "merge_conflicts",
      limit: fixes(merge_conflicts.max_retries),
      escalation: escalation(merge_conflicts),
    });
  }
  if (auto_merge !== undefined) {
    rows.push({
      kind: "auto_merge",
      limit: `${auto_merge.strategy} merge`,
      escalation: escalation(auto_merge),
    });
  }
  if (merge_completion !== undefined) {
    rows.push({
      kind: "merge_completion",
      limit: `moves to ${merge_completion.target_state}`,
      escalation: escalation(merge_completion),
    });
  }
  if (label_commands !== undefined) {
    const labels = [label_commands.review_label, label_commands.fix_label].filter(
      (label): label is string => label !== undefined && label !== "",
    );
    rows.push({
      kind: "label_commands",
      limit: labels.length === 0 ? "–" : joinLabels(labels, " "),
      escalation: "–",
    });
  }
  return rows;
}

/** Props of {@link ReactionsSection}. */
export interface ReactionsSectionProps {
  reactions: ShapeReactions;
  handoffState: string | undefined;
}

/** Renders Reactions: one table row per configured pull request reaction, in a fixed order, above the restart note. */
export function ReactionsSection({ reactions, handoffState }: ReactionsSectionProps) {
  const rows = reactionRows(reactions);
  const handoff =
    handoffState === undefined || handoffState === "" ? "the handoff state" : handoffState;

  return (
    <Panel title="Reactions">
      {rows.length === 0 ? (
        <p className="text-body-md text-ink-body">No reactions: Sortie's part ends at {handoff}.</p>
      ) : (
        <Table>
          <TableHeader>
            <TableHead>Kind</TableHead>
            <TableHead>Limit</TableHead>
            <TableHead>Escalation</TableHead>
          </TableHeader>
          <TableBody>
            {rows.map((row) => (
              <TableRow key={row.kind}>
                <TableCell>
                  <span className="font-mono">{row.kind}</span>
                </TableCell>
                <TableCell>{row.limit}</TableCell>
                <TableCell>{row.escalation}</TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      )}
    </Panel>
  );
}
