import type { ReactNode } from "react";

export interface CardProps {
  children: ReactNode;
}

/** Frames content such as a table or a chart; the Panel it stands in carries its title. */
export function Card({ children }: CardProps) {
  return (
    <div className="max-w-full overflow-clip rounded-xl border border-line bg-canvas">
      {children}
    </div>
  );
}
