import { createBrowserRouter } from "react-router";

import { NotFound } from "@/app/NotFound";
import { RouteError } from "@/app/RouteError";
import { Shell } from "@/app/Shell";
import {
  ActivityIcon,
  ChartColumnIcon,
  ChartGanttIcon,
  HistoryIcon,
  InboxIcon,
  SlidersHorizontalIcon,
  TagIcon,
} from "@/components/icons";

import type { ScreenEntry } from "@/app/Rail";

/** Each screen registers itself here. */
export const screens: ScreenEntry[] = [
  { key: "o", label: "Overview", path: "/", group: 0, icon: InboxIcon },
  { key: "p", label: "Pipeline", path: "/pipeline", group: 0, icon: ChartGanttIcon },
  { key: "h", label: "History", path: "/history", group: 0, icon: HistoryIcon },
  { key: "a", label: "Analytics", path: "/analytics", group: 0, icon: ChartColumnIcon },
  { key: "e", label: "Health", path: "/health", group: 1, icon: ActivityIcon },
  { key: "w", label: "Workflow", path: "/workflow", group: 1, icon: SlidersHorizontalIcon },
  { key: "r", label: "Prices", path: "/prices", group: 1, icon: TagIcon },
];

export const router = createBrowserRouter(
  [
    {
      path: "/",
      element: <Shell screens={screens} />,
      children: [
        {
          ErrorBoundary: RouteError,
          children: [
            {
              index: true,
              lazy: {
                Component: async () =>
                  (await import("@/screens/overview/OverviewScreen")).OverviewScreen,
              },
            },
            {
              path: "pipeline",
              lazy: {
                Component: async () =>
                  (await import("@/screens/pipeline/PipelineScreen")).PipelineScreen,
              },
            },
            {
              path: "issues/:identifier",
              lazy: {
                Component: async () => (await import("@/screens/issue/IssueScreen")).IssueScreen,
              },
            },
            {
              path: "issues/:identifier/sessions/:dispatchId",
              lazy: {
                Component: async () =>
                  (await import("@/screens/session/SessionScreen")).SessionScreen,
              },
            },
            {
              path: "history",
              lazy: {
                Component: async () =>
                  (await import("@/screens/history/HistoryScreen")).HistoryScreen,
              },
            },
            {
              path: "analytics",
              lazy: {
                Component: async () =>
                  (await import("@/screens/analytics/AnalyticsScreen")).AnalyticsScreen,
              },
            },
            {
              path: "health",
              lazy: {
                Component: async () => (await import("@/screens/health/HealthScreen")).HealthScreen,
              },
            },
            {
              path: "workflow",
              lazy: {
                Component: async () =>
                  (await import("@/screens/workflow/WorkflowScreen")).WorkflowScreen,
              },
            },
            {
              path: "prices",
              lazy: {
                Component: async () => (await import("@/screens/prices/PricesScreen")).PricesScreen,
              },
            },
            { path: "*", element: <NotFound /> },
          ],
        },
      ],
    },
  ],
  { basename: "/ui" },
);
