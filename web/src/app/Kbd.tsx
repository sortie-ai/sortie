import type { ReactNode } from "react";

export interface KbdProps {
  children: ReactNode;
}

export function Kbd({ children }: KbdProps) {
  return (
    <kbd className="inline-flex h-(--size-kbd) min-w-(--size-kbd) items-center justify-center rounded border border-line bg-surface-raised px-xxs font-mono text-body-sm font-medium text-ink-body">
      {children}
    </kbd>
  );
}
