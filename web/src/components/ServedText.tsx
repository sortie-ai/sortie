import { useState } from "react";

import { ChevronDownIcon } from "@/components/icons";
import { cn } from "@/lib/cn";
import { plural } from "@/lib/format";

export interface ServedTextProps {
  text: string;
}

// The word that may open a fenced block, such as "bash", which names its language.
const FENCE_INFO = /^[\w+-]*\n/;

// A block up to this many lines stays open; a longer one folds to its first line.
const OPEN_BLOCK_LINES = 3;

// Inline JSON past this many characters folds, so a long object never runs past the column.
const INLINE_JSON_CHARS = 80;

const CODE = "rounded bg-surface-raised font-mono leading-none";

// Reads code that parses as a JSON object or array, or `undefined` for any other code.
function jsonOf(code: string): unknown {
  const trimmed = code.trim();
  if (!trimmed.startsWith("{") && !trimmed.startsWith("[")) {
    return undefined;
  }
  try {
    return JSON.parse(trimmed) as unknown;
  } catch {
    return undefined;
  }
}

interface CodeFoldProps {
  /** The code as the open fold shows it. */
  code: string;
  /** The code on one line, as the closed fold shows it. */
  preview: string;
}

// Phrasing elements only, since a served sentence often sits in a paragraph. The closed fold lines
// its chevron up with the events pane's tool lines.
function CodeFold({ code, preview }: CodeFoldProps) {
  const [isOpen, setIsOpen] = useState(false);

  return (
    <span className="block">
      <button
        type="button"
        aria-expanded={isOpen}
        onClick={() => setIsOpen((wasOpen) => !wasOpen)}
        className="flex w-full min-w-0 cursor-pointer items-baseline gap-xs rounded text-left text-ink-body transition-colors duration-(--duration-feedback) hover:text-ink focus-visible:-outline-offset-2 focus-visible:outline-2 focus-visible:outline-focus-ring"
      >
        <ChevronDownIcon
          className={cn(
            "shrink-0 self-center transition-transform duration-(--duration-feedback)",
            !isOpen && "-rotate-90",
          )}
        />
        <span className="min-w-0 flex-1 truncate font-mono">{isOpen ? undefined : preview}</span>
        <span className="shrink-0 text-body-sm text-ink-muted tabular-nums">
          {plural(code.split("\n").length, "line", "lines")}
        </span>
      </button>
      {isOpen ? (
        <code className={cn(CODE, "mt-xs block px-xs py-xxs whitespace-pre-wrap wrap-anywhere")}>
          {code}
        </code>
      ) : undefined}
    </span>
  );
}

interface CodeSegmentProps {
  segment: string;
}

function CodeSegment({ segment }: CodeSegmentProps) {
  const isBlock = segment.includes("\n");
  const code = isBlock ? segment.replace(FENCE_INFO, "").trimEnd() : segment;
  const json = jsonOf(code);
  if (json !== undefined && (isBlock || code.length > INLINE_JSON_CHARS)) {
    return <CodeFold code={JSON.stringify(json, null, 2)} preview={JSON.stringify(json)} />;
  }
  if (isBlock && code.split("\n").length > OPEN_BLOCK_LINES) {
    return <CodeFold code={code} preview={code.replace(/\s+/g, " ").trim()} />;
  }
  return (
    <code
      className={cn(
        CODE,
        isBlock
          ? "block px-xs py-xxs whitespace-pre-wrap wrap-anywhere"
          : "px-xxs whitespace-nowrap",
      )}
    >
      {code}
    </code>
  );
}

/** Renders a sentence the server wrote as text, never as markup. Text between backticks becomes inline code; a block over three lines, and JSON too long for a line, fold to one line that opens on a click. */
export function ServedText({ text }: ServedTextProps) {
  const json = text.length > INLINE_JSON_CHARS ? jsonOf(text) : undefined;
  if (json !== undefined) {
    return <CodeFold code={JSON.stringify(json, null, 2)} preview={JSON.stringify(json)} />;
  }
  const segments = text.split("`");
  const last = segments.length - 1;

  return (
    <>
      {segments.map((segment, index) => {
        const isPaired = index % 2 === 1 && index < last;
        if (isPaired) {
          return segment === "" ? undefined : <CodeSegment key={index} segment={segment} />;
        }
        const prefix = index === last && index % 2 === 1 ? "`" : "";
        return prefix + segment === "" ? undefined : prefix + segment;
      })}
    </>
  );
}
