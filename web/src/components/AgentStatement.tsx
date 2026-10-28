import { ServedText } from "@/components/ServedText";
import { cn } from "@/lib/cn";

export interface AgentStatementProps {
  text: string;
  className?: string;
}

/** The text reaches the page only through {@link ServedText}. */
export function AgentStatement({ text, className }: AgentStatementProps) {
  return (
    <span className={cn("block text-ink-body wrap-anywhere", className)}>
      {"“"}
      <ServedText text={text} />
      {"”"}
    </span>
  );
}
