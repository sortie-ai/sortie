import { Link, useLocation, useMatch } from "react-router";

import { useStateQuery } from "@/api/queries";
import { Kbd } from "@/app/Kbd";
import { keyTip } from "@/app/key-tips";
import { modifierLabel } from "@/app/keys";
import { LiveIndicator } from "@/app/LiveIndicator";
import { SearchIcon } from "@/components/icons";
import { ThemeMenu } from "@/components/ThemeMenu";
import { useIssueSummary } from "@/components/use-issue-summary";
import { useSessionRun } from "@/components/use-session-run";
import { cn } from "@/lib/cn";
import { backOf } from "@/lib/issue-link";
import { RangePicker } from "@/screens/analytics/RangePicker";

import type { ScreenEntry } from "@/app/Rail";

const FALLBACK_LABEL = "Overview";

interface BackCrumbProps {
  screens: ScreenEntry[];
}

function BackCrumb({ screens }: BackCrumbProps) {
  const back = backOf(useLocation().state);
  const backPath = back.split(/[?#]/, 1)[0];
  const label = screens.find((screen) => screen.path === backPath)?.label ?? FALLBACK_LABEL;

  return (
    <>
      <Link
        to={back}
        className="text-ink-muted transition-colors duration-(--duration-feedback) hover:text-ink hover:underline hover:underline-offset-2"
      >
        {label}
      </Link>
      <span aria-hidden="true">/</span>
    </>
  );
}

interface BreadcrumbProps {
  identifier: string;
  screens: ScreenEntry[];
}

function Breadcrumb({ identifier, screens }: BreadcrumbProps) {
  const row = useIssueSummary(identifier);

  return (
    <nav aria-label="Breadcrumb" className="flex items-center gap-xs">
      <BackCrumb screens={screens} />
      <span className="text-ink wrap-anywhere">{row?.displayKey ?? identifier}</span>
    </nav>
  );
}

interface SessionBreadcrumbProps {
  identifier: string;
  dispatchId: string;
  screens: ScreenEntry[];
}

function SessionBreadcrumb({ identifier, dispatchId, screens }: SessionBreadcrumbProps) {
  const session = useSessionRun(identifier, dispatchId, { drivePages: false });
  const back = backOf(useLocation().state);
  const isResolved = session.kind === "live" || session.kind === "finished";

  return (
    <nav aria-label="Breadcrumb" className="flex items-center gap-xs">
      <BackCrumb screens={screens} />
      <Link
        to={`/issues/${encodeURIComponent(identifier)}`}
        state={{ back }}
        className="text-ink-muted wrap-anywhere transition-colors duration-(--duration-feedback) hover:text-ink hover:underline hover:underline-offset-2"
      >
        {isResolved ? session.key : identifier}
      </Link>
      <span aria-hidden="true">/</span>
      <span className="text-ink">{isResolved ? `Session ${session.number}` : "Session"}</span>
    </nav>
  );
}

function AgentsStatus() {
  const { data } = useStateQuery();
  if (data === undefined) {
    return undefined;
  }
  const running = data.counts.running;
  const cap = data.config?.agent.max_concurrent_agents;

  return (
    <span
      title={
        cap === undefined
          ? `${running} ${running === 1 ? "agent" : "agents"} running`
          : `${running} of ${cap} ${cap === 1 ? "agent" : "agents"} running`
      }
      className="flex items-center gap-xs whitespace-nowrap text-body-sm max-lg:hidden"
    >
      <span className="text-ink-muted">Agents</span>
      <span className="font-medium text-ink tabular-nums">
        {cap === undefined ? running : `${running}/${cap}`}
      </span>
    </span>
  );
}

export interface PageBreadcrumbProps {
  screens: ScreenEntry[];
}

export function PageBreadcrumb({ screens }: PageBreadcrumbProps) {
  const identifier = useMatch("/issues/:identifier")?.params.identifier;
  const sessionParams = useMatch("/issues/:identifier/sessions/:dispatchId")?.params;

  if (sessionParams?.identifier !== undefined && sessionParams.dispatchId !== undefined) {
    return (
      <div className="mb-md text-body-sm text-ink-muted max-md:hidden">
        <SessionBreadcrumb
          identifier={sessionParams.identifier}
          dispatchId={sessionParams.dispatchId}
          screens={screens}
        />
      </div>
    );
  }
  if (identifier === undefined) {
    return undefined;
  }
  return (
    <div className="mb-md text-body-sm text-ink-muted max-md:hidden">
      <Breadcrumb identifier={identifier} screens={screens} />
    </div>
  );
}

export interface TopBarProps {
  className: string;
  /** True while the peek holds Overview still. */
  isHolding: boolean;
  onTogglePause: () => void;
  onOpenPalette: () => void;
}

export function TopBar({ className, isHolding, onTogglePause, onOpenPalette }: TopBarProps) {
  const isAnalytics = useMatch("/analytics") !== null;

  return (
    <header className="sticky top-0 z-(--z-sticky) h-(--size-header) border-b border-line bg-canvas max-md:hidden">
      <div className={cn("flex h-full items-center gap-lg text-body-sm", className)}>
        <button
          type="button"
          onClick={onOpenPalette}
          title={keyTip("Search or run a command", modifierLabel(), "K")}
          className="inline-flex h-(--size-control) min-w-0 max-w-2xl flex-1 cursor-pointer items-center gap-xs rounded-lg border border-line bg-field px-sm text-body-md text-ink-muted transition-colors duration-(--duration-feedback) hover:border-line-medium focus-visible:-outline-offset-1 focus-visible:outline-2 focus-visible:outline-focus-ring active:bg-state-pressed"
        >
          <SearchIcon className="shrink-0" />
          <span className="min-w-0 flex-1 truncate text-left">Search or run a command</span>
          <span className="flex shrink-0 gap-xxs max-lg:hidden">
            <Kbd>{modifierLabel()}</Kbd>
            <Kbd>K</Kbd>
          </span>
        </button>
        <div className="ml-auto flex shrink-0 items-center gap-lg">
          <div className="flex items-center">
            <AgentsStatus />
            <LiveIndicator isHolding={isHolding} onTogglePause={onTogglePause} />
          </div>
          <div className="flex items-center gap-xs">
            {isAnalytics ? <RangePicker /> : undefined}
            <ThemeMenu />
          </div>
        </div>
      </div>
    </header>
  );
}
