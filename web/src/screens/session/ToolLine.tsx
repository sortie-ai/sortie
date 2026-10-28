import { BookOpenIcon, FileIcon, FilePenIcon, TerminalIcon, WrenchIcon } from "@/components/icons";
import { StatusMark } from "@/components/StatusMark";
import { fmtDuration, plural } from "@/lib/format";

import type { RunEventResponse } from "@/api/types.gen";

type FoldKind = "shell" | "read" | "edit" | "other";

const FOLD_NOUNS: Record<FoldKind, readonly [one: string, many: string]> = {
  shell: ["command", "commands"],
  read: ["file read", "file reads"],
  edit: ["file edit", "file edits"],
  other: ["tool call", "tool calls"],
};

function KindIcon({ kind }: { kind: string | undefined }) {
  switch (kind) {
    case "shell":
      return <TerminalIcon className="text-ink-muted" />;
    case "read":
      return <FileIcon className="text-ink-muted" />;
    case "edit":
      return <FilePenIcon className="text-ink-muted" />;
    case "skill":
      return <BookOpenIcon className="text-ink-muted" />;
    default:
      return <WrenchIcon className="text-ink-muted" />;
  }
}

function foldKindOf(call: RunEventResponse): FoldKind {
  switch (call.tool_kind) {
    case "shell":
    case "read":
    case "edit":
      return call.tool_kind;
    default:
      return "other";
  }
}

export interface ToolTextProps {
  text: string;
}

export function ToolText({ text }: ToolTextProps) {
  return (
    <span title={text} className="block min-w-0 truncate">
      {text}
    </span>
  );
}

export interface ToolLineProps {
  call: RunEventResponse;
}

export function ToolLine({ call }: ToolLineProps) {
  const line = <ToolText text={call.tool_line ?? ""} />;

  return (
    <span className="flex items-center gap-sm text-ink">
      <KindIcon kind={call.tool_kind} />
      {call.tool_error === true ? (
        <StatusMark mark="failed" label={line} className="min-w-0 *:min-w-0" />
      ) : (
        line
      )}
      {call.duration_ms === undefined ? undefined : (
        <span className="whitespace-nowrap text-ink-body">
          {fmtDuration(call.duration_ms / 1000)}
        </span>
      )}
    </span>
  );
}

export function foldPhraseOf(calls: readonly RunEventResponse[]): string {
  const counts = new Map<FoldKind, number>();
  for (const call of calls) {
    const kind = foldKindOf(call);
    counts.set(kind, (counts.get(kind) ?? 0) + 1);
  }
  return [...counts].map(([kind, count]) => plural(count, ...FOLD_NOUNS[kind])).join(", ");
}
