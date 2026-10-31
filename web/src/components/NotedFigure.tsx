import { Hint } from "@/components/ui/hint";

import type { Figure } from "@/lib/figures";

export interface NotedFigureProps {
  figure: Figure;
}

/** Renders a figure's text, underlined with its note on hover when it has one, and plain otherwise. */
export function NotedFigure({ figure }: NotedFigureProps) {
  return figure.note === undefined ? figure.text : <Hint hint={figure.note}>{figure.text}</Hint>;
}
