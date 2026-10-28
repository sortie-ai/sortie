import { useEffect, useRef, useState } from "react";
import { Outlet, useLocation, useMatch, useNavigate, useSearchParams } from "react-router";
import { useQueryClient } from "@tanstack/react-query";

import { ApiError, postRefresh } from "@/api/client";
import { queryKeys, useStateQuery } from "@/api/queries";
import { CommandPalette } from "@/app/CommandPalette";
import { DegradedBanner } from "@/app/DegradedBanner";
import { useDocumentStatus } from "@/app/document-status";
import { useShortcuts } from "@/app/keys";
import { useLive } from "@/app/live";
import { NarrowBar } from "@/app/NarrowBar";
import { Rail } from "@/app/Rail";
import { ShortcutsDialog } from "@/app/ShortcutsDialog";
import { PageBreadcrumb, TopBar } from "@/app/TopBar";
import { activeViewPath } from "@/components/ActiveHead";
import { IssuePeek } from "@/components/IssuePeek";
import { toast, Toaster } from "@/components/toaster";
import { usePeek } from "@/components/use-peek";
import { cn } from "@/lib/cn";
import { condition } from "@/lib/condition";
import { buildEnded } from "@/lib/ended";
import { backOf } from "@/lib/issue-link";
import { localMidnight, now } from "@/lib/server-clock";
import { cycleTheme } from "@/lib/theme";
import { attentionKey, endedKey, markRead } from "@/lib/unread";

import type { Binding } from "@/app/keys";
import type { ScreenEntry } from "@/app/Rail";
import type { RunsResponse } from "@/api/types.gen";

const PAGE_COLUMN = "mx-auto w-full px-xxxl max-xl:px-lg max-md:px-md";
const WIDE_PAGE_COLUMN = cn(
  PAGE_COLUMN,
  "max-w-[calc(var(--container-wide)+2*var(--spacing-xxxl))]",
);

const NO_ANSWER = "Sortie did not answer. Nothing changed.";

function failureText(error: unknown): string {
  if (error instanceof ApiError && error.status !== 0 && error.serverMessage !== null) {
    return error.serverMessage;
  }
  return NO_ANSWER;
}

interface ShellFrameProps {
  screens: ScreenEntry[];
}

function ShellFrame({ screens }: ShellFrameProps) {
  const { data, status } = useStateQuery();
  const { togglePause, versionNotice, dismissVersionNotice } = useLive();
  const navigate = useNavigate();
  const { state: routeState } = useLocation();
  const client = useQueryClient();
  const peek = usePeek();
  const [search] = useSearchParams();
  const isOverview = useMatch("/") !== null;
  const isPipeline = useMatch("/pipeline") !== null;
  const isIssueRoute = useMatch("/issues/:identifier") !== null;
  const sessionIdentifier = useMatch("/issues/:identifier/sessions/:dispatchId")?.params.identifier;
  const [isPaletteOpen, setIsPaletteOpen] = useState(false);
  const [isShortcutsOpen, setIsShortcutsOpen] = useState(false);
  const noticedVersion = useRef<string | null>(null);
  const isOverlayOpen = isPaletteOpen || isShortcutsOpen;

  useDocumentStatus(
    data === undefined ? undefined : condition(data),
    data?.counts.attention ?? 0,
    data?.config?.tracker.project,
  );

  useEffect(() => {
    if (versionNotice === null || noticedVersion.current === versionNotice) {
      return;
    }
    noticedVersion.current = versionNotice;
    toast(versionNotice, "", {
      action: { label: "Reload", onSelect: () => location.reload() },
      persists: true,
    });
    dismissVersionNotice();
  }, [versionNotice, dismissVersionNotice]);

  const handleRefresh = async () => {
    try {
      const { body } = await postRefresh();
      toast(body.outcome_text);
    } catch (error) {
      toast(failureText(error), "", { variant: "error" });
    }
  };

  const handleTogglePause = () => {
    const isPaused = togglePause();
    toast(isPaused ? "Live updates paused. Press P to resume." : "Live updates resumed.");
  };

  const handleMarkAllRead = () => {
    if (data === undefined) {
      return;
    }
    const keys = (data.attention ?? []).map((item) => attentionKey(item.id));
    const pages = client.getQueryData<RunsResponse[]>(
      queryKeys.ended(localMidnight(now()).toISOString()),
    );
    if (pages !== undefined) {
      for (const ended of buildEnded(pages, data)) {
        keys.push(endedKey(ended.issueId, ended.outcome.observed_at));
      }
    }
    markRead(keys);
    toast("Marked everything read.");
  };

  const handleEscape = () => {
    if (isOverlayOpen) {
      return;
    }
    if (peek.identifier !== null) {
      peek.close();
    } else if (isIssueRoute) {
      void navigate(backOf(routeState));
    } else if (sessionIdentifier !== undefined) {
      void navigate(`/issues/${encodeURIComponent(sessionIdentifier)}`, {
        state: { back: backOf(routeState) },
      });
    }
  };

  const whenIdle = (run: () => void) => () => {
    if (!isOverlayOpen) {
      run();
    }
  };

  const handleSwitchView = () => {
    if (isOverview) {
      void navigate(activeViewPath("pipeline", search));
    } else if (isPipeline) {
      void navigate(activeViewPath("list", search));
    }
  };

  const bindings: Binding[] = [
    { keys: ["r"], run: whenIdle(() => void handleRefresh()) },
    { keys: ["p"], run: whenIdle(handleTogglePause) },
    { keys: ["t"], run: whenIdle(() => void cycleTheme()) },
    { keys: ["l"], run: whenIdle(handleSwitchView) },
    { keys: ["u"], shift: true, run: whenIdle(handleMarkAllRead) },
    {
      keys: ["?"],
      run: () => {
        setIsPaletteOpen(false);
        setIsShortcutsOpen((isOpen) => !isOpen);
      },
    },
    {
      keys: ["k"],
      mod: true,
      run: () => {
        setIsShortcutsOpen(false);
        setIsPaletteOpen((isOpen) => !isOpen);
      },
    },
    { keys: ["Escape"], run: handleEscape },
    ...screens.map((screen) => ({
      keys: [screen.key],
      chord: true,
      run: whenIdle(() => void navigate(screen.path)),
    })),
  ];
  useShortcuts(bindings);

  const handleOpenPalette = () => setIsPaletteOpen(true);
  const pageColumn = isPipeline ? PAGE_COLUMN : WIDE_PAGE_COLUMN;

  return (
    <div className="grid min-h-screen grid-cols-1 md:grid-cols-[var(--spacing-nav-rail)_minmax(0,1fr)]">
      <Rail screens={screens} onOpenShortcuts={() => setIsShortcutsOpen(true)} />
      <div className="min-w-0">
        <NarrowBar onOpenPalette={handleOpenPalette} />
        <TopBar
          className={pageColumn}
          isHolding={(isOverview || isPipeline) && peek.identifier !== null}
          onTogglePause={handleTogglePause}
          onOpenPalette={handleOpenPalette}
        />
        <div className={cn(pageColumn, "pt-xl pb-xxxl max-md:pt-md")}>
          <PageBreadcrumb screens={screens} />
          <DegradedBanner />
          <main id="main">
            {data !== undefined ? (
              <Outlet />
            ) : status === "error" ? (
              <p className="text-body-md text-ink-body">
                Sortie is not answering. The dashboard keeps trying.
              </p>
            ) : (
              <p className="text-body-md text-ink-body">Loading</p>
            )}
          </main>
        </div>
      </div>
      <CommandPalette
        screens={screens}
        open={isPaletteOpen}
        onOpenChange={setIsPaletteOpen}
        onRefresh={() => void handleRefresh()}
        onTogglePause={handleTogglePause}
        onMarkAllRead={handleMarkAllRead}
        onOpenShortcuts={() => setIsShortcutsOpen(true)}
      />
      <IssuePeek />
      <ShortcutsDialog screens={screens} open={isShortcutsOpen} onOpenChange={setIsShortcutsOpen} />
    </div>
  );
}

export interface ShellProps {
  screens: ScreenEntry[];
}

/** Must sit inside the query client, live-update, and router providers. */
export function Shell({ screens }: ShellProps) {
  return (
    <>
      <ShellFrame screens={screens} />
      <Toaster />
    </>
  );
}
