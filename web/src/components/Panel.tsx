import type { ReactNode } from "react";

export interface PanelProps {
  title: string;
  right?: ReactNode;
  children: ReactNode;
}

export function Panel({ title, right, children }: PanelProps) {
  const heading = <h2 className="font-heading text-h7 font-semibold text-ink">{title}</h2>;

  return (
    <section className="flex min-w-0 flex-col gap-sm border-t border-line pt-md">
      {right === undefined ? (
        heading
      ) : (
        <div className="flex flex-wrap items-baseline gap-sm">
          {heading}
          <span className="ml-auto text-body-sm text-ink-muted">{right}</span>
        </div>
      )}
      {children}
    </section>
  );
}

export interface PairsProps {
  children: ReactNode;
}

export function Pairs({ children }: PairsProps) {
  return (
    <dl className="grid grid-cols-[120px_minmax(0,1fr)] gap-x-md gap-y-xs text-body-md">
      {children}
    </dl>
  );
}

export interface PairProps {
  label: string;
  children: ReactNode;
}

export function Pair({ label, children }: PairProps) {
  return (
    <>
      <dt className="text-ink-muted">{label}</dt>
      <dd className="text-ink wrap-anywhere">{children}</dd>
    </>
  );
}
