import { useMemo } from "react";
import { useSearchParams } from "react-router";

import { formatZoom, parseSelection, parseZoom, stampOf } from "./activity";
import { durationWords } from "./bucket-names";

import type { AnalyticsRange, StatsWindow } from "@/api/queries";
import type { Selection } from "./activity";

const RANGE_PARAM = "range";
const AT_PARAM = "at";
const TO_PARAM = "to";
const ZOOM_PARAM = "zoom";
const DEFAULT_RANGE: AnalyticsRange = "7d";

export const RANGE_OPTIONS: readonly { value: AnalyticsRange; label: string }[] = [
  { value: "24h", label: "Last 24 hours" },
  { value: "7d", label: "Last 7 days" },
  { value: "30d", label: "Last 30 days" },
  { value: "90d", label: "Last 90 days" },
  { value: "all", label: "All time" },
];

/** Returns the range a URL parameter names, and the default range for any other value. */
export function rangeOf(value: string | null): AnalyticsRange {
  return RANGE_OPTIONS.find((option) => option.value === value)?.value ?? DEFAULT_RANGE;
}

const PREVIOUS_PERIOD: Record<AnalyticsRange, string | null> = {
  "24h": "previous 24 hours",
  "7d": "previous 7 days",
  "30d": "previous 30 days",
  "90d": "previous 90 days",
  all: null,
};

/** Names the window of equal length before a range or a zoomed window, lowercase for use inside a sentence; all time has none. */
export function previousPeriodOf(range: AnalyticsRange, zoom: StatsWindow | null): string | null {
  return zoom === null
    ? PREVIOUS_PERIOD[range]
    : `previous ${durationWords(zoom.until - zoom.since)}`;
}

/** What the address says the Analytics screen shows. */
export interface AnalyticsView {
  range: AnalyticsRange;
  /** Zoom levels from the address, outermost first; empty when not zoomed. */
  zooms: readonly StatsWindow[];
  /** The innermost zoom level; null when not zoomed. */
  zoom: StatsWindow | null;
  selection: Selection | null;
}

/** Changes the address; every action replaces the history entry and keeps the parameters it does not own. */
export interface AnalyticsViewActions {
  /** Picks a range and drops the zoom and the selection; does nothing when the range is already shown unzoomed. */
  pickRange: (range: AnalyticsRange) => void;
  select: (selection: Selection | null) => void;
  /** Zooms into a window inside the innermost level and drops the selection. */
  zoomIn: (level: StatsWindow) => void;
  zoomOut: () => void;
  resetZoom: () => void;
}

function setOrDelete(params: URLSearchParams, name: string, value: string | null) {
  if (value === null) {
    params.delete(name);
  } else {
    params.set(name, value);
  }
}

/**
 * Reads the range, the zoom levels, and the selection from the address and returns the actions that write them.
 *
 * The returned view and actions keep their identity while the address is unchanged.
 */
export function useAnalyticsView(): [AnalyticsView, AnalyticsViewActions] {
  const [params, setParams] = useSearchParams();
  const range = rangeOf(params.get(RANGE_PARAM));
  const zoomText = params.get(ZOOM_PARAM);
  const atText = params.get(AT_PARAM);
  const toText = params.get(TO_PARAM);

  const view = useMemo<AnalyticsView>(() => {
    const zooms = parseZoom(zoomText);
    return {
      range,
      zooms,
      zoom: zooms[zooms.length - 1] ?? null,
      selection: parseSelection(atText, toText),
    };
  }, [range, zoomText, atText, toText]);

  const actions = useMemo<AnalyticsViewActions>(() => {
    function write(change: (next: URLSearchParams) => void) {
      setParams(
        (previous) => {
          const next = new URLSearchParams(previous);
          change(next);
          return next;
        },
        { replace: true },
      );
    }

    function writeZoom(next: URLSearchParams, zooms: readonly StatsWindow[]) {
      setOrDelete(next, ZOOM_PARAM, formatZoom(zooms));
      next.delete(AT_PARAM);
      next.delete(TO_PARAM);
    }

    return {
      pickRange(picked) {
        if (picked === range && view.zooms.length === 0) {
          return;
        }
        write((next) => {
          next.set(RANGE_PARAM, picked);
          writeZoom(next, []);
        });
      },
      select(selection) {
        write((next) => {
          setOrDelete(next, AT_PARAM, selection === null ? null : stampOf(selection.first));
          setOrDelete(
            next,
            TO_PARAM,
            selection === null || selection.last === selection.first
              ? null
              : stampOf(selection.last),
          );
        });
      },
      zoomIn(level) {
        write((next) => writeZoom(next, [...parseZoom(next.get(ZOOM_PARAM)), level]));
      },
      zoomOut() {
        write((next) => writeZoom(next, parseZoom(next.get(ZOOM_PARAM)).slice(0, -1)));
      },
      resetZoom() {
        write((next) => writeZoom(next, []));
      },
    };
  }, [setParams, range, view]);

  return [view, actions];
}
