import type { ReactNode } from "react";

export interface CodeProps {
  children: ReactNode;
}

export function Code({ children }: CodeProps) {
  return (
    <code className="rounded bg-surface-raised px-xxs font-mono text-[0.9em] wrap-anywhere">
      {children}
    </code>
  );
}
