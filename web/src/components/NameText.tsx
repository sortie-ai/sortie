import { ServedText } from "@/components/ServedText";
import { MONO } from "@/components/ui/mono";
import { cn } from "@/lib/cn";

export interface NameTextProps {
  /** The name as the server serves it. */
  name: string;
  /** The wording for a name that has one; defaults to the name itself. */
  label?: string;
}

/** A rule, model or agent kind name in the monospace face; a name the client has no wording for reads as served. */
export function NameText({ name, label = name }: NameTextProps) {
  return (
    <span className={cn(label === name && MONO)}>
      <ServedText text={label} />
    </span>
  );
}
