import { useLocation, useMatch } from "react-router";

import { useStateQuery } from "@/api/queries";
import { BrandLink } from "@/app/BrandLink";
import { ProjectName } from "@/app/ProjectName";
import { StatusCard } from "@/app/StatusCard";
import { ExternalLink } from "@/components/ExternalLink";
import { NavLink } from "@/components/carried-links";
import { cn } from "@/lib/cn";
import { fmtVersion, pluralVerb } from "@/lib/format";
import { backOf } from "@/lib/issue-link";

import type { StateResponse } from "@/api/types.gen";
import type { IconProps } from "@/components/icons";
import type { ReactNode } from "react";

/** `key` is the letter pressed after `G`, `path` is relative to the application's base path, and screens with the same `group` sit together; the screen at `/` is Overview, which carries the attention badge. */
export interface ScreenEntry {
  key: string;
  label: string;
  path: string;
  group: number;
  icon: (props: IconProps) => ReactNode;
}

const OVERVIEW_PATH = "/";

const EXTERNAL_LINKS: { label: string; href: string }[] = [
  { label: "Home", href: "https://sortie-ai.com" },
  { label: "Docs", href: "https://docs.sortie-ai.com" },
  { label: "GitHub", href: "https://github.com/sortie-ai/sortie" },
];

function hasCriticalItem(state: StateResponse): boolean {
  return (state.attention ?? []).some(
    (item) => item.acknowledged_at === null && item.severity === "critical",
  );
}

function groupsOf(screens: ScreenEntry[]): ScreenEntry[][] {
  const numbers = [...new Set(screens.map((screen) => screen.group))].sort((a, b) => a - b);
  return numbers.map((group) => screens.filter((screen) => screen.group === group));
}

interface AttentionBadgeProps {
  state: StateResponse;
}

function AttentionBadge({ state }: AttentionBadgeProps) {
  const count = state.counts.attention;
  if (count <= 0) {
    return undefined;
  }
  const attentionPhrase = pluralVerb(count, ["item", "items"], ["needs", "need"]);
  const tone = hasCriticalItem(state)
    ? "border border-error-border bg-error-container text-error"
    : "border border-warning-border bg-warning-container text-warning";
  return (
    <span
      title={`${attentionPhrase.subject} ${attentionPhrase.verb} attention`}
      className={cn(
        "ml-auto inline-grid h-(--size-kbd) min-w-(--size-kbd) place-items-center rounded-full px-xs text-body-sm font-medium tabular-nums",
        tone,
      )}
    >
      {count}
    </span>
  );
}

export interface RailProps {
  screens: ScreenEntry[];
}

export function Rail({ screens }: RailProps) {
  const { data } = useStateQuery();
  // On an issue or a session, the screen it was opened from keeps an outline.
  const isDetail = useMatch("/issues/*") !== null;
  const back = backOf(useLocation().state);
  const origin = isDetail ? back.split(/[?#]/, 1)[0] : null;
  const groups = groupsOf(screens);

  return (
    <aside
      aria-label="Sortie navigation"
      className="sticky top-0 hidden h-screen flex-col border-r border-line bg-surface px-sm pb-lg text-ink [--ghost-hover:var(--color-item-hover-panel)] [--ghost-pressed:var(--color-item-selected-panel)] md:flex"
    >
      {/* The brand row is as tall as the header and the project row as tall as the band over a screen's title; the name and version share the search field's baseline. */}
      <div className="grid shrink-0 grid-cols-[auto_minmax(0,1fr)] grid-rows-[var(--size-header)_var(--size-crumbs)] items-center gap-x-sm px-sm">
        <div className="col-span-2 flex min-w-0 items-baseline gap-sm">
          <BrandLink />
          {data === undefined ? undefined : (
            <span title={data.version} className="ml-auto truncate text-body-sm text-ink-muted">
              {fmtVersion(data.version)}
            </span>
          )}
        </div>
        <div className="col-span-2 flex min-w-0">
          <ProjectName />
        </div>
      </div>
      {/* The padding below the card puts the first screen link on the line of the first panel's title. */}
      <div className="pb-[calc(var(--spacing-head)-var(--spacing-md)-(var(--size-nav-item)-var(--text-h7--line-height))/2)]">
        <StatusCard />
      </div>
      <nav aria-label="Main" className="min-h-0 overflow-y-auto">
        {groups.map((group, index) => (
          <div key={group[0].group}>
            {index > 0 ? <div role="separator" className="mx-sm mb-md h-px bg-line" /> : undefined}
            <div className="mb-md grid gap-xxs">
              {group.map((screen) => (
                <NavLink
                  key={screen.path}
                  to={screen.path}
                  end
                  className={cn(
                    "flex h-(--size-nav-item) items-center gap-sm rounded-lg px-sm text-ink-body transition-colors duration-(--duration-feedback) hover:bg-item-hover-panel hover:text-ink focus-visible:-outline-offset-1 focus-visible:outline-2 focus-visible:outline-focus-ring active:bg-item-selected-panel aria-[current=page]:bg-item-selected-panel aria-[current=page]:font-medium aria-[current=page]:text-ink",
                    screen.path === origin && "ring-1 ring-line-medium ring-inset",
                  )}
                >
                  <screen.icon />
                  {/* A line in a panel title's face carries the label, so it stands on the title's baseline. */}
                  <span className="font-heading text-h7">
                    <span className="font-sans text-body-md tracking-normal">{screen.label}</span>
                  </span>
                  {screen.path === OVERVIEW_PATH && data !== undefined ? (
                    <AttentionBadge state={data} />
                  ) : undefined}
                </NavLink>
              ))}
            </div>
          </div>
        ))}
      </nav>
      <div className="mt-auto px-sm pt-md text-body-sm text-ink-muted">
        {/* The first link starts at the rail's left edge and the last ends at its right. */}
        <p className="flex items-center justify-between">
          {EXTERNAL_LINKS.map(({ label, href }) => (
            <ExternalLink key={href} href={href}>
              {label}
            </ExternalLink>
          ))}
        </p>
      </div>
    </aside>
  );
}
