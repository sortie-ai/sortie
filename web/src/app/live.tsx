import { useQueryClient } from "@tanstack/react-query";
import {
  createContext,
  use,
  useCallback,
  useEffect,
  useMemo,
  useRef,
  useState,
  useSyncExternalStore,
} from "react";

import { useStateQuery } from "@/api/queries";
import { parseInstant } from "@/lib/format";
import { now } from "@/lib/server-clock";
import { connectChangeStream } from "@/stream/client";

import type { Query, QueryClient } from "@tanstack/react-query";
import type { ReactNode } from "react";

import type { RunningEntryResponse } from "@/api/types.gen";
import type { RunMessage, StreamMessage, StreamStatus } from "@/stream/core";

const STATE_KEY_ROOT = "state";

function floorOf(query: Query): number | undefined {
  const floor = query.meta?.refetchFloorMs;
  return typeof floor === "number" ? floor : undefined;
}

function skipsChangeSignal(query: Query): boolean {
  return query.meta?.skipsChangeSignal === true;
}

interface FloorGate {
  request: (query: Query) => void;
  /** Starts watching the cache; the returned function stops it and clears every timer and mark. */
  watch: () => () => void;
}

/** A running fetch is never canceled: a signal during it is remembered and the query is checked again once the fetch ends, because its result can predate the signal. */
function createFloorGate(
  client: QueryClient,
  isPaused: () => boolean,
  holdChange: () => void,
): FloorGate {
  const timers = new Map<Query, ReturnType<typeof setTimeout>>();
  const deferred = new Set<Query>();

  function forget(query: Query): void {
    clearTimeout(timers.get(query));
    timers.delete(query);
    deferred.delete(query);
  }

  // A fresh query needs nothing: another path, such as a window focus, may have refetched it meanwhile.
  function recheck(query: Query): void {
    if (isPaused()) {
      holdChange();
      return;
    }
    if (query.isActive() && query.isStale() && document.visibilityState !== "hidden") {
      request(query);
    }
  }

  function request(query: Query): void {
    const floor = floorOf(query);
    if (floor === undefined) {
      return;
    }
    if (query.state.fetchStatus !== "idle") {
      deferred.add(query);
      return;
    }
    const due = Math.max(query.state.dataUpdatedAt, query.state.errorUpdatedAt) + floor;
    // The library stamps updates with Date.now(), so the floor reads the same clock.
    const wait = due - Date.now();
    if (wait <= 0) {
      void client.refetchQueries(
        { predicate: (candidate) => candidate === query },
        { cancelRefetch: false },
      );
    } else if (!timers.has(query)) {
      timers.set(
        query,
        setTimeout(() => {
          timers.delete(query);
          recheck(query);
        }, wait),
      );
    }
  }

  function watch(): () => void {
    const unsubscribe = client.getQueryCache().subscribe((event) => {
      if (event.type === "removed") {
        forget(event.query);
      } else if (
        event.type === "updated" &&
        deferred.has(event.query) &&
        event.query.state.fetchStatus === "idle"
      ) {
        deferred.delete(event.query);
        event.query.invalidate();
        recheck(event.query);
      }
    });
    return () => {
      unsubscribe();
      for (const query of [...timers.keys(), ...deferred]) {
        forget(query);
      }
    };
  }

  return { request, watch };
}

let streamStatus: StreamStatus = "connecting";
let streamStarted = false;
let connection: ReturnType<typeof connectChangeStream> | undefined;
let shownRun: string | null = null;
const statusListeners = new Set<() => void>();
const changeListeners = new Set<() => void>();
const runListeners = new Set<(message: RunMessage) => void>();

function handleStreamMessage(message: StreamMessage): void {
  switch (message.kind) {
    case "status":
      streamStatus = message.status;
      for (const listener of statusListeners) {
        listener();
      }
      return;
    case "change":
      for (const listener of changeListeners) {
        listener();
      }
      return;
    case "run":
      for (const listener of runListeners) {
        listener(message);
      }
  }
}

/** Later calls do nothing; call it before React renders so the first change signal is not missed. */
export function startLiveStream(): void {
  if (streamStarted) {
    return;
  }
  streamStarted = true;
  connection = connectChangeStream(handleStreamMessage);
  if (shownRun !== null) {
    connection.show(shownRun);
  }
}

export function subscribeRunMessages(listener: (m: RunMessage) => void): () => void {
  runListeners.add(listener);
  return () => runListeners.delete(listener);
}

/** A call made before `startLiveStream` applies when the stream starts. */
export function showRun(dispatchId: string | null): void {
  shownRun = dispatchId;
  connection?.show(dispatchId);
}

function subscribeStatus(listener: () => void): () => void {
  statusListeners.add(listener);
  return () => statusListeners.delete(listener);
}

function getStreamStatus(): StreamStatus {
  return streamStatus;
}

function quietKeyOf(row: RunningEntryResponse): string {
  return `${row.dispatch_id}|${row.quiet_at}`;
}

export interface LiveValue {
  paused: boolean;
  /** Flips the pause and returns the new value; resuming replays a change signal that arrived while paused. */
  togglePause: () => boolean;
  streamStatus: StreamStatus;
  /** The toast text once a state response carries another `version` than the first one; `null` before and after dismissal. */
  versionNotice: string | null;
  dismissVersionNotice: () => void;
}

const LiveContext = createContext<LiveValue | undefined>(undefined);

export function LiveProvider({ children }: LiveProviderProps) {
  const client = useQueryClient();
  const { data } = useStateQuery();
  const [paused, setPaused] = useState(false);
  const [versionNotice, setVersionNotice] = useState<string | null>(null);
  const pausedRef = useRef(false);
  const pendingChange = useRef(false);
  const floorGate = useRef<FloorGate | undefined>(undefined);
  const requestedQuiet = useRef(new Set<string>());
  const firstVersion = useRef<string | null>(null);
  const noticedVersion = useRef<string | null>(null);
  const status = useSyncExternalStore(subscribeStatus, getStreamStatus);

  const handleChange = useCallback(() => {
    if (pausedRef.current) {
      pendingChange.current = true;
      return;
    }
    void client.invalidateQueries({ refetchType: "none" });
    void client.refetchQueries({ queryKey: [STATE_KEY_ROOT] });
    if (document.visibilityState === "hidden") {
      return;
    }
    const others = { type: "active", stale: true } as const;
    void client.refetchQueries({
      ...others,
      predicate: (query) =>
        query.queryKey[0] !== STATE_KEY_ROOT &&
        floorOf(query) === undefined &&
        !skipsChangeSignal(query),
    });
    for (const query of client.getQueryCache().findAll({
      ...others,
      predicate: (candidate) => floorOf(candidate) !== undefined,
    })) {
      floorGate.current?.request(query);
    }
  }, [client]);

  useEffect(() => {
    const gate = createFloorGate(
      client,
      () => pausedRef.current,
      () => {
        pendingChange.current = true;
      },
    );
    const stopWatching = gate.watch();
    floorGate.current = gate;
    return () => {
      stopWatching();
      floorGate.current = undefined;
    };
  }, [client]);

  useEffect(() => {
    startLiveStream();
    changeListeners.add(handleChange);
    return () => {
      changeListeners.delete(handleChange);
    };
  }, [handleChange]);

  const running = data?.running;
  useEffect(() => {
    const rows = running ?? [];
    const requested = requestedQuiet.current;
    const held = new Set(rows.map(quietKeyOf));
    for (const key of requested) {
      if (!held.has(key)) {
        requested.delete(key);
      }
    }
    // A row that still serves no stall text after its refetch is not asked again, or the refetch would loop.
    const due = rows.flatMap((row) => {
      const at = parseInstant(row.quiet_at);
      const key = quietKeyOf(row);
      return at !== null && row.stall_text === null && !requested.has(key) ? [{ key, at }] : [];
    });
    if (due.length === 0) {
      return;
    }
    const timer = setTimeout(
      () => {
        for (const { key, at } of due) {
          if (at <= now()) {
            requested.add(key);
          }
        }
        handleChange();
      },
      Math.max(0, Math.min(...due.map(({ at }) => at)) - now()),
    );
    return () => clearTimeout(timer);
  }, [running, handleChange]);

  const version = data?.version;
  useEffect(() => {
    if (version === undefined) {
      return;
    }
    if (firstVersion.current === null) {
      firstVersion.current = version;
    } else if (version !== firstVersion.current && version !== noticedVersion.current) {
      noticedVersion.current = version;
      setVersionNotice(`Sortie ${version} is running. Reload to update the dashboard.`);
    }
  }, [version]);

  const togglePause = useCallback(() => {
    const next = !pausedRef.current;
    pausedRef.current = next;
    setPaused(next);
    if (!next && pendingChange.current) {
      pendingChange.current = false;
      handleChange();
    }
    return next;
  }, [handleChange]);

  const dismissVersionNotice = useCallback(() => setVersionNotice(null), []);

  const value = useMemo<LiveValue>(
    () => ({
      paused,
      togglePause,
      streamStatus: status,
      versionNotice,
      dismissVersionNotice,
    }),
    [paused, togglePause, status, versionNotice, dismissVersionNotice],
  );

  return <LiveContext value={value}>{children}</LiveContext>;
}

interface LiveProviderProps {
  children: ReactNode;
}

/** @throws When used outside `LiveProvider`. */
export function useLive(): LiveValue {
  const value = use(LiveContext);
  if (value === undefined) {
    throw new Error("useLive must be used inside LiveProvider");
  }
  return value;
}

/** Returns the value as it was when `held` turned true for as long as `held` stays true, else the live value; pass one object bundling everything a view renders so the whole view holds still together. */
export function useHold<T>(value: T, held: boolean): T {
  const [snapshot, setSnapshot] = useState<{ value: T } | null>(null);
  if (held && snapshot === null) {
    setSnapshot({ value });
  } else if (!held && snapshot !== null) {
    setSnapshot(null);
  }
  return held && snapshot !== null ? snapshot.value : value;
}

/** Returns the server-clock time in epoch milliseconds, refreshed every second and frozen while paused. */
export function useNow(): number {
  const { paused } = useLive();
  const [tick, setTick] = useState(now);

  useEffect(() => {
    if (paused) {
      return;
    }
    const id = setInterval(() => setTick(now()), 1_000);
    return () => clearInterval(id);
  }, [paused]);

  return tick;
}
