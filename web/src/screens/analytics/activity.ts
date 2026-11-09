import type { StatsWindow } from "@/api/queries";
import type { SpanSession, Spend, SpendBucket } from "@/api/types.gen";
import type { Units } from "@/lib/units";

export type Period = Spend["period"];

type FixedPeriod = Exclude<Period, "month">;

/** The first and last selected bucket starts, epoch milliseconds; `first` is not after `last`. */
export interface Selection {
  first: number;
  last: number;
}

/** The most buckets the server's period rule lets a bounded range list. */
export const MAX_BUCKETS = 168;

/** The length of each fixed period; a month is a UTC calendar month and has no fixed length. */
export const PERIOD_MS: Record<FixedPeriod, number> = {
  five_minutes: 300_000,
  quarter_hour: 900_000,
  hour: 3_600_000,
  day: 86_400_000,
};

/** The buckets before a one-bucket selection that make up its baseline. */
export const BASELINE_BUCKETS: Record<Period, number> = {
  five_minutes: 12,
  quarter_hour: 4,
  hour: 24,
  day: 7,
  month: 3,
};

const FINE_TO_COARSE: readonly FixedPeriod[] = ["five_minutes", "quarter_hour", "hour", "day"];

/** Returns the period the server buckets a bounded span of this length by: the finest one that keeps it to at most `MAX_BUCKETS` buckets. */
export function periodOfSpan(spanMs: number): Period {
  return FINE_TO_COARSE.find((p) => spanMs <= MAX_BUCKETS * PERIOD_MS[p]) ?? "month";
}

function shiftMonths(start: number, months: number): number {
  const date = new Date(start);
  return Date.UTC(date.getUTCFullYear(), date.getUTCMonth() + months, 1);
}

/** Returns the first instant after the bucket that starts at `start`. */
export function bucketEndOf(start: number, period: Period): number {
  return period === "month" ? shiftMonths(start, 1) : start + PERIOD_MS[period];
}

/** Returns the start of the bucket `count` buckets before the one starting at `start`. */
export function bucketsBefore(start: number, period: Period, count: number): number {
  return period === "month" ? shiftMonths(start, -count) : start - count * PERIOD_MS[period];
}

/** Holds when no turn ended in the bucket and no run recorded before per-turn records spent in it; a bare turn count would hide the spend of that history. */
export function isEmptyBucket(bucket: SpendBucket): boolean {
  return bucket.turns === 0 && bucket.turns_unrecorded_runs === 0;
}

/** Returns the figure the chart scales by in the shown unit; a figure the server could not measure counts as 0. */
export function valueOf(bucket: SpendBucket, units: Units): number {
  return units === "tokens" ? (bucket.tokens?.uncached ?? 0) : (bucket.cost_usd ?? 0);
}

/** Averages the whole buckets, empty ones included, and finds the first whole bucket with the largest value; the bucket in progress joins neither. */
export function averageAndPeak(
  buckets: readonly SpendBucket[],
  units: Units,
): { average: number; peakIndex: number | null } {
  let sum = 0;
  let count = 0;
  let peakValue = 0;
  let peakIndex: number | null = null;
  buckets.forEach((bucket, index) => {
    if (bucket.in_progress) {
      return;
    }
    const value = valueOf(bucket, units);
    sum += value;
    count += 1;
    if (value > peakValue) {
      peakValue = value;
      peakIndex = index;
    }
  });
  return { average: count === 0 ? 0 : sum / count, peakIndex };
}

/** Returns the window from the first selected bucket's start to the last one's end. */
export function selectedWindowOf(selection: Selection, period: Period): StatsWindow {
  return { since: selection.first, until: bucketEndOf(selection.last, period) };
}

/** Returns the window of `count` buckets just before the one starting at `first`. */
export function previousWindowOf(first: number, count: number, period: Period): StatsWindow {
  return { since: bucketsBefore(first, period, count), until: first };
}

/** Returns the window of the baseline buckets just before the one starting at `first`. */
export function baselineWindowOf(first: number, period: Period): StatsWindow {
  return { since: bucketsBefore(first, period, BASELINE_BUCKETS[period]), until: first };
}

/** Holds when zooming into the window changes the chart: it is longer than one finest bucket and either narrower than the chart or served in a finer period. */
export function canZoomInto(
  window: StatsWindow,
  period: Period,
  selectedCount: number,
  bucketCount: number,
): boolean {
  const spanMs = window.until - window.since;
  return (
    spanMs > PERIOD_MS.five_minutes &&
    (selectedCount < bucketCount || periodOfSpan(spanMs) !== period)
  );
}

/** Orders sessions by their share of the shown unit, then by turns, both largest first; equal sessions keep the served order. */
export function sessionsByShare(sessions: readonly SpanSession[], units: Units): SpanSession[] {
  const shareOf = (session: SpanSession) =>
    (units === "tokens" ? session.tokens_share : session.cost_share) ?? 0;
  return [...sessions].sort((a, b) => shareOf(b) - shareOf(a) || (b.turns ?? 0) - (a.turns ?? 0));
}

/** Writes an instant as the UTC minute stamp the address carries, such as "2026-10-28T15:30". */
export function stampOf(ms: number): string {
  return new Date(ms).toISOString().slice(0, 16);
}

function parseStamp(text: string | undefined): number {
  return text === undefined ? Number.NaN : Date.parse(`${text}Z`);
}

/** Reads the `zoom` parameter as windows, outermost first; a level that does not parse or does not run forward is dropped. */
export function parseZoom(text: string | null): StatsWindow[] {
  if (text === null || text === "") {
    return [];
  }
  const windows: StatsWindow[] = [];
  for (const level of text.split(",")) {
    const [from, to] = level.split("/");
    const since = parseStamp(from);
    const until = parseStamp(to);
    if (since < until) {
      windows.push({ since, until });
    }
  }
  return windows;
}

/** Writes zoom windows as the `zoom` parameter; no window has no parameter. */
export function formatZoom(zooms: readonly StatsWindow[]): string | null {
  return zooms.length === 0
    ? null
    : zooms.map((level) => `${stampOf(level.since)}/${stampOf(level.until)}`).join(",");
}

/** Reads the `at` and `to` parameters as a selection; an absent `to` selects one bucket, and an unparsable stamp selects none. */
export function parseSelection(at: string | null, to: string | null): Selection | null {
  if (at === null) {
    return null;
  }
  const from = parseStamp(at);
  const end = to === null ? from : parseStamp(to);
  if (Number.isNaN(from) || Number.isNaN(end)) {
    return null;
  }
  return { first: Math.min(from, end), last: Math.max(from, end) };
}

/** Returns the indexes of the selection's first and last buckets, or null when either is not a start on the chart. */
export function spanIndexesOf(
  starts: readonly number[],
  selection: Selection | null,
): [number, number] | null {
  if (selection === null) {
    return null;
  }
  const first = starts.indexOf(selection.first);
  const last = starts.indexOf(selection.last);
  return first < 0 || last < 0 ? null : [first, last];
}
