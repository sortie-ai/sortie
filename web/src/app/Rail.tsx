import { useState } from "react";
import { NavLink, useNavigate } from "react-router";

import { useStateQuery } from "@/api/queries";
import { BrandMark } from "@/app/BrandMark";
import { keyTip } from "@/app/key-tips";
import { StatusCard } from "@/app/StatusCard";
import {
  BookOpenIcon,
  CircleHelpIcon,
  ExternalLinkIcon,
  GithubIcon,
  SearchIcon,
  XIcon,
} from "@/components/icons";
import { Button, buttonStyles } from "@/components/ui/button";
import { cn } from "@/lib/cn";
import { fmtVersion, pluralVerb } from "@/lib/format";

import type { StateResponse } from "@/api/types.gen";
import type { IconProps } from "@/components/icons";
import type { KeyboardEvent, ReactNode } from "react";

/**
 * One screen the shell can open. `key` is the letter pressed after `G`, `path`
 * is relative to the application's base path, and screens with the same `group`
 * sit together, with a divider between groups in ascending order. The screen at
 * path `/` is Overview, which carries the attention badge.
 */
export interface ScreenEntry {
  key: string;
  label: string;
  path: string;
  group: number;
  icon: (props: IconProps) => ReactNode;
}

const OVERVIEW_PATH = "/";

const EXTERNAL_LINKS: { label: string; href: string; Icon: (props: IconProps) => ReactNode }[] = [
  { label: "Documentation", href: "https://docs.sortie-ai.com", Icon: BookOpenIcon },
  { label: "GitHub", href: "https://github.com/sortie-ai/sortie", Icon: GithubIcon },
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

function matching(screens: ScreenEntry[], query: string): ScreenEntry[] {
  const needle = query.trim().toLowerCase();
  return needle === ""
    ? screens
    : screens.filter((screen) => screen.label.toLowerCase().includes(needle));
}

interface NavFilterProps {
  query: string;
  firstMatch: ScreenEntry | undefined;
  onQueryChange: (query: string) => void;
}

// Enter opens the first match, so typing a few letters is enough to move.
function NavFilter({ query, firstMatch, onQueryChange }: NavFilterProps) {
  const navigate = useNavigate();

  function handleKeyDown(event: KeyboardEvent<HTMLInputElement>) {
    // Enter and Escape during IME composition confirm or cancel a candidate.
    if (event.nativeEvent.isComposing) {
      return;
    }
    if (event.key === "Enter" && firstMatch !== undefined) {
      onQueryChange("");
      void navigate(firstMatch.path);
    } else if (event.key === "Escape") {
      onQueryChange("");
      event.currentTarget.blur();
    }
  }

  return (
    <label className="relative mb-md flex items-center">
      <span className="sr-only">Filter screens</span>
      <SearchIcon className="pointer-events-none absolute left-sm text-ink-muted" />
      <input
        type="text"
        placeholder="Filter"
        autoComplete="off"
        value={query}
        onChange={(event) => onQueryChange(event.target.value)}
        onKeyDown={handleKeyDown}
        className="h-(--size-control) w-full rounded-lg border border-line bg-field pr-xl pl-[calc(var(--spacing-sm)+var(--size-icon)+var(--spacing-xs))] text-body-md text-ink outline-none transition-colors duration-(--duration-feedback) placeholder:text-ink-muted hover:border-line-medium focus-visible:border-line-medium focus-visible:outline-2 focus-visible:-outline-offset-1 focus-visible:outline-focus-ring"
      />
      {query === "" ? undefined : (
        <button
          type="button"
          aria-label="Clear filter"
          onClick={() => onQueryChange("")}
          className="absolute right-xxs inline-grid size-6 cursor-pointer place-items-center rounded text-ink-muted hover:text-ink focus-visible:outline-2 focus-visible:outline-focus-ring"
        >
          <XIcon />
        </button>
      )}
    </label>
  );
}

/** Props of {@link Rail}. */
export interface RailProps {
  screens: ScreenEntry[];
  onOpenShortcuts: () => void;
}

/** Renders the dark navigation rail: brand, screen filter, one entry per matching screen, the status card, the documentation and repository links, and the shortcuts button. */
export function Rail({ screens, onOpenShortcuts }: RailProps) {
  const { data } = useStateQuery();
  const [query, setQuery] = useState("");
  const shown = matching(screens, query);
  const groups = groupsOf(shown);
  const project = data?.config?.tracker.project;

  return (
    <aside
      data-theme="dark"
      aria-label="Sortie navigation"
      className="sticky top-0 hidden h-screen flex-col bg-surface px-sm py-lg text-ink [--ghost-hover:var(--color-item-hover-panel)] [--ghost-pressed:var(--color-item-selected-panel)] md:flex"
    >
      <div className="grid grid-cols-[auto_minmax(0,1fr)_auto] items-center gap-x-sm px-sm pb-md">
        <BrandMark />
        <span className="font-heading text-h7 font-semibold">Sortie</span>
        {data === undefined ? undefined : (
          <span
            title={data.version}
            className="justify-self-end whitespace-nowrap text-body-sm text-ink-muted"
          >
            {fmtVersion(data.version)}
          </span>
        )}
        {project === undefined || project === "" ? undefined : (
          <span
            title={project}
            className="col-span-2 col-start-2 truncate text-body-sm text-ink-muted"
          >
            {project}
          </span>
        )}
      </div>
      <NavFilter query={query} firstMatch={shown[0]} onQueryChange={setQuery} />
      <nav aria-label="Main" className="min-h-0 overflow-y-auto">
        {shown.length === 0 ? (
          <p className="px-sm text-body-md text-ink-muted">No screens match</p>
        ) : undefined}
        {groups.map((group, index) => (
          <div key={group[0].group}>
            {index > 0 ? <div role="separator" className="mx-sm mb-md h-px bg-line" /> : undefined}
            <div className="mb-md grid gap-xxs">
              {group.map((screen) => (
                <NavLink
                  key={screen.path}
                  to={screen.path}
                  end
                  className="flex h-(--size-nav-item) items-center gap-sm rounded-lg px-sm text-ink-body transition-colors duration-(--duration-feedback) hover:bg-item-hover-panel hover:text-ink focus-visible:-outline-offset-1 focus-visible:outline-2 focus-visible:outline-focus-ring active:bg-item-selected-panel aria-[current=page]:bg-item-selected-panel aria-[current=page]:font-medium aria-[current=page]:text-ink"
                >
                  <screen.icon />
                  {screen.label}
                  {screen.path === OVERVIEW_PATH && data !== undefined ? (
                    <AttentionBadge state={data} />
                  ) : undefined}
                </NavLink>
              ))}
            </div>
          </div>
        ))}
      </nav>
      <div className="mt-auto grid gap-xxs pt-md">
        <div className="mb-sm">
          <StatusCard />
        </div>
        {EXTERNAL_LINKS.map(({ label, href, Icon }) => (
          <a
            key={href}
            href={href}
            target="_blank"
            rel="noopener noreferrer"
            title={`${label}, opens in a new tab`}
            className={buttonStyles("ghost", "w-full justify-start font-regular")}
          >
            <Icon />
            {label}
            <ExternalLinkIcon className="ml-auto text-ink-muted" />
          </a>
        ))}
        <Button
          variant="ghost"
          className="w-full justify-start font-regular"
          onClick={onOpenShortcuts}
          title={keyTip("Shortcuts", "?")}
        >
          <CircleHelpIcon />
          Shortcuts
        </Button>
      </div>
    </aside>
  );
}
