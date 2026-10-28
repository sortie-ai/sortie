import type {
  AgentEventFrame,
  RunEndedFrame,
  RunEventResponse,
  RunResetFrame,
} from "@/api/types.gen";

export type StreamStatus = "connecting" | "open" | "closed";

/** One retained event, or the run's events fromSeq..toSeq the server reported as not retained. */
export type RunItem =
  | { kind: "event"; event: RunEventResponse }
  | { kind: "gap"; fromSeq: number; toSeq: number };

/** What the worker or a tab holds of one run; items in seq order. */
export interface RunLog {
  /** At most `RUN_EVENT_LIMIT` events. */
  items: RunItem[];
  /** Highest seq any item covers; 0 before the first. */
  lastSeq: number;
  ended: boolean;
  notRetained: boolean;
}

/** Items to merge into one run's log; a receiver never loses what it holds. */
export interface RunMessage {
  kind: "run";
  dispatchId: string;
  items: RunItem[];
  ended: boolean;
  notRetained: boolean;
}

export type StreamMessage =
  | { kind: "status"; status: StreamStatus }
  | { kind: "change"; revision: string }
  | RunMessage;

/** Tab to worker: the run the tab's session view shows while the tab is visible, else null. */
export interface ShowMessage {
  kind: "show";
  dispatchId: string | null;
}

/** The most events a run log holds; mirrors the server's retained-event count bound. */
export const RUN_EVENT_LIMIT = 1_000;

/** The most `run` parameters the stream route accepts. */
export const MAX_RUN_SUBSCRIPTIONS = 16;

/** The longest a run's merged items wait for the `RunMessage` that carries them. */
export const RUN_FLUSH_MS = 50;

const EVENTS_PATH = "/api/v1/events";
const INITIAL_DELAY_MS = 1_000;
const MAX_DELAY_MS = 30_000;
const IDLE_TIMEOUT_MS = 45_000;
const DISPATCH_ID_PATTERN = /^[A-Z2-7]{1,64}$/;

/** Reports whether the value has the form of a dispatch ID, the only form the stream route accepts as a `run` parameter. */
export function isDispatchId(value: string): boolean {
  return DISPATCH_ID_PATTERN.test(value);
}

export function emptyRunLog(): RunLog {
  return { items: [], lastSeq: 0, ended: false, notRetained: false };
}

function dropOldestBeyondLimit(items: RunItem[]): RunItem[] {
  let excess = items.filter((item) => item.kind === "event").length - RUN_EVENT_LIMIT;
  if (excess <= 0) {
    return items;
  }
  let fromSeq: number | undefined;
  for (const [index, item] of items.entries()) {
    fromSeq ??= item.kind === "gap" ? item.fromSeq : item.event.seq;
    if (item.kind === "event" && --excess === 0) {
      return [{ kind: "gap", fromSeq, toSeq: item.event.seq }, ...items.slice(index + 1)];
    }
  }
  return items;
}

/** An item at or below `lastSeq` is ignored, a gap is clamped to start above `lastSeq`, and events beyond `RUN_EVENT_LIMIT` fold into the leading gap. Returns `log` itself when the merge changes nothing, so a replay of what a tab holds does not render. */
export function mergeRun(
  log: RunLog,
  change: Pick<RunMessage, "items" | "ended" | "notRetained">,
): RunLog {
  const appended: RunItem[] = [];
  let lastSeq = log.lastSeq;
  for (const item of change.items) {
    if (item.kind === "event") {
      if (item.event.seq > lastSeq) {
        appended.push(item);
        lastSeq = item.event.seq;
      }
    } else if (item.toSeq > lastSeq) {
      appended.push({
        kind: "gap",
        fromSeq: Math.max(item.fromSeq, lastSeq + 1),
        toSeq: item.toSeq,
      });
      lastSeq = item.toSeq;
    }
  }
  const ended = log.ended || change.ended;
  const notRetained = log.notRetained || change.notRetained;
  if (appended.length === 0 && ended === log.ended && notRetained === log.notRetained) {
    return log;
  }
  return { items: dropOldestBeyondLimit([...log.items, ...appended]), lastSeq, ended, notRetained };
}

// The server writes frame data in the generated frame shapes; the compiler cannot see across the wire.
function frameOf<T>(event: Event): T {
  return JSON.parse((event as MessageEvent<string>).data) as T;
}

/** One `EventSource` that reopens with a doubling delay and is dropped after 45 s without an event; `post` gets at most one `RunMessage` per run every `RUN_FLUSH_MS`. `watch` reopens the connection only when it changes the runs the URL names; `snapshot` returns what a watched run holds so a new receiver starts from it. */
export function createStream(post: (m: StreamMessage) => void): {
  open(): void;
  watch(dispatchIds: readonly string[]): void;
  snapshot(dispatchId: string): RunMessage | undefined;
} {
  const runs = new Map<string, RunLog>();
  const queued = new Map<string, RunMessage>();
  const flushTimers = new Map<string, ReturnType<typeof setTimeout>>();
  let delayMs = INITIAL_DELAY_MS;
  let source: EventSource | undefined;
  let idleTimer: ReturnType<typeof setTimeout> | undefined;
  let reopenTimer: ReturnType<typeof setTimeout> | undefined;

  const postStatus = (status: StreamStatus) => post({ kind: "status", status });

  const urlRuns = () =>
    [...runs].filter(([, log]) => !log.ended && !log.notRetained).slice(0, MAX_RUN_SUBSCRIPTIONS);

  const urlRunIds = () => urlRuns().map(([id]) => id);

  const url = () => {
    const params = urlRuns().map(
      ([id, log]) => `run=${id}${log.lastSeq > 0 ? `:${log.lastSeq}` : ""}`,
    );
    return params.length === 0 ? EVENTS_PATH : `${EVENTS_PATH}?${params.join("&")}`;
  };

  const stopIdle = () => {
    clearTimeout(idleTimer);
    idleTimer = undefined;
  };

  const reopenLater = () => {
    stopIdle();
    postStatus("closed");
    clearTimeout(reopenTimer);
    reopenTimer = setTimeout(() => {
      reopenTimer = undefined;
      open();
    }, delayMs);
    delayMs = Math.min(delayMs * 2, MAX_DELAY_MS);
  };

  const armIdle = (current: EventSource) => {
    stopIdle();
    idleTimer = setTimeout(() => {
      current.close();
      reopenLater();
    }, IDLE_TIMEOUT_MS);
  };

  // A stream not yet opened reads url() when it opens, and a pending backoff reopen builds url() itself.
  const reopen = () => {
    if (source !== undefined && reopenTimer === undefined) {
      open();
    }
  };

  const forget = (id: string) => {
    runs.delete(id);
    queued.delete(id);
    clearTimeout(flushTimers.get(id));
    flushTimers.delete(id);
  };

  const record = (id: string, change: Pick<RunMessage, "items" | "ended" | "notRetained">) => {
    const log = runs.get(id);
    if (log === undefined) {
      return;
    }
    runs.set(id, mergeRun(log, change));
    const message: RunMessage = queued.get(id) ?? {
      kind: "run",
      dispatchId: id,
      items: [],
      ended: false,
      notRetained: false,
    };
    message.items.push(...change.items);
    message.ended ||= change.ended;
    message.notRetained ||= change.notRetained;
    queued.set(id, message);
  };

  const flush = (id: string) => {
    clearTimeout(flushTimers.get(id));
    flushTimers.delete(id);
    const message = queued.get(id);
    if (message !== undefined) {
      queued.delete(id);
      post(message);
    }
  };

  const flushSoon = (id: string) => {
    if (!flushTimers.has(id)) {
      flushTimers.set(
        id,
        setTimeout(() => flush(id), RUN_FLUSH_MS),
      );
    }
  };

  // The run leaves the URL, so the stream reopens only when that lets a waiting run in.
  const settle = (id: string, change: Pick<RunMessage, "items" | "ended" | "notRetained">) => {
    const before = urlRunIds();
    record(id, change);
    flush(id);
    if (urlRunIds().some((other) => !before.includes(other))) {
      reopen();
    }
  };

  function open(): void {
    source?.close();
    const current = new EventSource(url());
    source = current;
    postStatus("connecting");
    current.onopen = () => {
      delayMs = INITIAL_DELAY_MS;
      postStatus("open");
      armIdle(current);
    };
    current.addEventListener("change", (event) => {
      armIdle(current);
      post({ kind: "change", revision: (event as MessageEvent).lastEventId });
    });
    current.addEventListener("heartbeat", () => armIdle(current));
    current.addEventListener("agent_event", (event) => {
      armIdle(current);
      const { dispatch_id: id, ...runEvent } = frameOf<AgentEventFrame>(event);
      const log = runs.get(id);
      if (log === undefined || runEvent.seq <= log.lastSeq) {
        return;
      }
      record(id, { items: [{ kind: "event", event: runEvent }], ended: false, notRetained: false });
      flushSoon(id);
    });
    current.addEventListener("run_ended", (event) => {
      armIdle(current);
      const frame = frameOf<RunEndedFrame>(event);
      settle(frame.dispatch_id, { items: [], ended: true, notRetained: false });
    });
    current.addEventListener("reset", (event) => {
      armIdle(current);
      const frame = frameOf<RunResetFrame>(event);
      const log = runs.get(frame.dispatch_id);
      if (log === undefined) {
        return;
      }
      if (frame.reason === "not_retained") {
        settle(frame.dispatch_id, { items: [], ended: false, notRetained: true });
      } else if (
        frame.reason === "cursor_expired" &&
        frame.first_seq !== undefined &&
        frame.first_seq - 1 > log.lastSeq
      ) {
        record(frame.dispatch_id, {
          items: [{ kind: "gap", fromSeq: log.lastSeq + 1, toSeq: frame.first_seq - 1 }],
          ended: false,
          notRetained: false,
        });
        flush(frame.dispatch_id);
      }
    });
    current.onerror = () => {
      if (current.readyState === EventSource.CLOSED) {
        reopenLater();
        return;
      }
      postStatus("connecting");
    };
  }

  function watch(dispatchIds: readonly string[]): void {
    const before = urlRunIds();
    const wanted = new Set(dispatchIds.filter(isDispatchId));
    for (const id of runs.keys()) {
      if (!wanted.has(id)) {
        forget(id);
      }
    }
    for (const id of wanted) {
      if (!runs.has(id)) {
        runs.set(id, emptyRunLog());
      }
    }
    if (before.join() !== urlRunIds().join()) {
      reopen();
    }
  }

  function snapshot(dispatchId: string): RunMessage | undefined {
    const log = runs.get(dispatchId);
    return log === undefined
      ? undefined
      : {
          kind: "run",
          dispatchId,
          items: log.items,
          ended: log.ended,
          notRetained: log.notRetained,
        };
  }

  return { open, watch, snapshot };
}
