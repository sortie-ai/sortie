import { useEffect, useRef } from "react";

import { useLive } from "@/app/live";
import { conditionWord } from "@/lib/condition";

import type { Condition } from "@/lib/condition";

const BASE_TITLE = "Sortie";
const DOT_COLORS: Record<Condition, string | null> = {
  degraded: "#EF4444",
  needs: "#EAB308",
  held: null,
  clear: null,
};

const LOGO_PATHS =
  '<path d="M772.92 1045.61 698.57 1119.11 624.55 1119.56 699.37 1045.61 624.55 971.65 698.57 972.11Z"/>' +
  '<path d="M500 954.39 574.35 880.89 648.36 880.44 573.54 954.39 648.36 1028.35 574.35 1027.89Z"/>' +
  '<path d="M638.73 1072.39 591 1119.56 500 1119.56 500 1091.95 547.17 1045.33 547.17 1072.39Z"/>' +
  '<path d="M634.18 927.61 681.91 880.44 772.91 880.44 772.91 908.05 725.74 954.67 725.74 927.61Z"/>';

function faviconUrl(condition: Condition | undefined): string {
  const color = condition === undefined ? null : DOT_COLORS[condition];
  const dot =
    color === null
      ? ""
      : `<circle cx="200" cy="56" r="48" fill="${color}" stroke="#fff" stroke-width="16"/>`;
  const svg =
    `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 256 256">` +
    `<rect width="256" height="256" rx="38" fill="#00112B"/>` +
    `<g transform="translate(-319.75,-577.01) scale(0.70351)" fill="#00BDFF">` +
    `${LOGO_PATHS}</g>${dot}</svg>`;
  return `data:image/svg+xml,${encodeURIComponent(svg)}`;
}

function titleFor(
  condition: Condition | undefined,
  attention: number,
  project: string | undefined,
): string {
  if (condition === undefined) {
    return BASE_TITLE;
  }
  const prefix = attention > 0 ? `(${attention}) ` : "";
  const named = project === undefined || project === "" ? "" : `${project} - `;
  return `${prefix}${conditionWord(condition)} - ${named}${BASE_TITLE}`;
}

function iconLink(): HTMLLinkElement {
  const existing = document.head.querySelector<HTMLLinkElement>("link[rel='icon'][data-sortie]");
  if (existing !== null) {
    return existing;
  }
  const link = document.createElement("link");
  link.rel = "icon";
  link.type = "image/svg+xml";
  link.dataset.sortie = "";
  document.head.append(link);
  return link;
}

/** Pass `undefined` for the condition before the first state response and for the project while the state serves none; while live updates are paused the last title and icon stay. */
export function useDocumentStatus(
  condition: Condition | undefined,
  attention: number,
  project: string | undefined,
): void {
  const { paused: frozen } = useLive();
  const applied = useRef<{ title: string; icon: string } | null>(null);

  useEffect(() => {
    if (frozen && applied.current !== null) {
      return;
    }
    const next = {
      title: titleFor(condition, attention, project),
      icon: faviconUrl(condition),
    };
    applied.current = condition === undefined ? null : next;
    document.title = next.title;
    iconLink().href = next.icon;
  }, [condition, attention, project, frozen]);
}
