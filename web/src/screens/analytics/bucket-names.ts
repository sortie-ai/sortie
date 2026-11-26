import { fmtUtcClock, fmtUtcDay, fmtUtcPeriod, fmtUtcPeriodTick, parseInstant } from "@/lib/format";
import { now } from "@/lib/server-clock";

import { PERIOD_MS, bucketEndOf } from "./activity";

import type { StatsWindow } from "@/api/queries";
import type { Period } from "./activity";

// The axis caption doubles as the chart's aria label and the Cache read plot's label.
export const PERIOD_NAMES: Record<Period, { per: string; axis: string }> = {
  five_minutes: { per: "5 minutes", axis: "UTC, 5 minutes" },
  quarter_hour: { per: "15 minutes", axis: "UTC, 15 minutes" },
  hour: { per: "hour", axis: "UTC hour" },
  day: { per: "day", axis: "UTC day" },
  month: { per: "month", axis: "UTC month" },
};

/** Names the figure a one-bucket selection is set against, and the buckets it averages, for each period. */
export const BASELINE_WORDS: Record<Period, { label: string; note: string }> = {
  five_minutes: { label: "Prior hour average", note: "the hour before" },
  quarter_hour: { label: "Prior hour average", note: "the hour before" },
  hour: { label: "Prior 24 hours average", note: "the 24 hours before" },
  day: { label: "Prior 7 days average", note: "the 7 days before" },
  month: { label: "Prior 3 months average", note: "the 3 months before" },
};

function isoOf(ms: number): string {
  return new Date(ms).toISOString();
}

function isSameUtcDay(a: number, b: number): boolean {
  return Math.floor(a / PERIOD_MS.day) === Math.floor(b / PERIOD_MS.day);
}

function unitWords(n: number, one: string, many: string): string {
  return `${n} ${n === 1 ? one : many}`;
}

/** Names a bucket in full: the UTC day and month above a day, else the day with the clock span of the bucket. */
export function bucketNameOf(start: string, period: Period): string {
  if (period === "day" || period === "month") {
    return fmtUtcPeriod(start, period);
  }
  const instant = parseInstant(start);
  if (instant === null) {
    return fmtUtcPeriod(start, "day");
  }
  const end = isoOf(instant + PERIOD_MS[period]);
  return `${fmtUtcPeriod(start, "day")}, ${fmtUtcClock(start)} to ${fmtUtcClock(end)} UTC`;
}

/** Names the span from the first selected bucket through the last, in the form of a single bucket's name when they are the same bucket. */
export function spanNameOf(first: number, last: number, period: Period): string {
  if (first === last) {
    return bucketNameOf(isoOf(first), period);
  }
  if (period === "day" || period === "month") {
    return `${fmtUtcPeriod(isoOf(first), period)} to ${fmtUtcPeriod(isoOf(last), period)}`;
  }
  const end = bucketEndOf(last, period);
  const from = `${fmtUtcPeriod(isoOf(first), "day")}, ${fmtUtcClock(isoOf(first))}`;
  const until = isSameUtcDay(first, end - 1)
    ? fmtUtcClock(isoOf(end))
    : `${fmtUtcPeriod(isoOf(end), "day")}, ${fmtUtcClock(isoOf(end))}`;
  return `${from} to ${until} UTC`;
}

/** Writes the short form of a bucket's start that follows "Peak": the clock alone on the server's current UTC day, else the day and the clock. */
export function peakAtOf(start: string, period: Period): string {
  if (period === "month") {
    return fmtUtcPeriod(start, "month");
  }
  if (period === "day") {
    return fmtUtcPeriodTick(start, "day");
  }
  const instant = parseInstant(start);
  if (instant === null || isSameUtcDay(instant, now())) {
    return fmtUtcClock(start);
  }
  return `${fmtUtcDay(start)}, ${fmtUtcClock(start)}`;
}

/** Writes the tooltip line of the bucket still under way, naming the clock time it closes at where that is known. */
export function inProgressTextOf(start: string, period: Period): string {
  if (period === "month") {
    return "In progress";
  }
  if (period === "day") {
    return "In progress, until 00:00 UTC";
  }
  const instant = parseInstant(start);
  return instant === null
    ? "In progress"
    : `In progress, until ${fmtUtcClock(isoOf(bucketEndOf(instant, period)))} UTC`;
}

/** Words how long `count` buckets last: minutes and hours for the fine periods, days and hours for hourly buckets from a day up. */
export function spanLengthWords(count: number, period: Period): string {
  if (period === "five_minutes" || period === "quarter_hour") {
    const minutes = count * (period === "five_minutes" ? 5 : 15);
    return [
      minutes >= 60 ? unitWords(Math.floor(minutes / 60), "hour", "hours") : "",
      minutes % 60 > 0 ? unitWords(minutes % 60, "minute", "minutes") : "",
    ]
      .filter(Boolean)
      .join(" ");
  }
  if (period === "hour" && count >= 24) {
    return [
      unitWords(Math.floor(count / 24), "day", "days"),
      count % 24 > 0 ? unitWords(count % 24, "hour", "hours") : "",
    ]
      .filter(Boolean)
      .join(" ");
  }
  const unit = period === "hour" ? "hour" : period === "day" ? "day" : "month";
  return unitWords(count, unit, `${unit}s`);
}

/** Labels the previous-span tile; a length of exactly one unit drops its "1", so one hour reads "Previous hour" and a day and an hour keeps its "1". */
export function previousLabelOf(count: number, period: Period): string {
  const words = spanLengthWords(count, period);
  return `Previous ${/^1 [a-z]+$/.test(words) ? words.slice(2) : words}`;
}

/** Words a length of time in days, hours, and minutes, rounded to the minute. */
export function durationWords(ms: number): string {
  const minutes = Math.round(ms / 60_000);
  const days = Math.floor(minutes / 1440);
  const hours = Math.floor((minutes % 1440) / 60);
  const rest = minutes % 60;
  return [
    days > 0 ? unitWords(days, "day", "days") : "",
    hours > 0 ? unitWords(hours, "hour", "hours") : "",
    rest > 0 ? unitWords(rest, "minute", "minutes") : "",
  ]
    .filter(Boolean)
    .join(" ");
}

/** Names a zoomed window as the range picker shows it, in UTC like the chart under it. */
export function zoomLabelOf(zoom: StatsWindow): string {
  const since = isoOf(zoom.since);
  const until = isoOf(zoom.until);
  const from = `${fmtUtcPeriodTick(since, "day")}, ${fmtUtcClock(since)}`;
  const to = isSameUtcDay(zoom.since, zoom.until - 1)
    ? fmtUtcClock(until)
    : `${fmtUtcPeriodTick(until, "day")}, ${fmtUtcClock(until)}`;
  return `${from} to ${to} UTC`;
}
