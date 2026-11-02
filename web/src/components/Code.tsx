import type { ReactNode } from "react";

export interface CodeProps {
  children: ReactNode;
}

export function Code({ children }: CodeProps) {
  return (
    <code className="rounded bg-surface-raised px-xxs font-mono leading-none wrap-anywhere">
      {children}
    </code>
  );
}
