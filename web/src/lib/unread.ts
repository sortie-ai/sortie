import { useSyncExternalStore } from "react";

const STORAGE_KEY = "sortie.read";
const STORAGE_VERSION = 1;

const listeners = new Set<() => void>();
let current: ReadonlySet<string> | undefined;

export function attentionKey(id: string): string {
  return `a:${id}`;
}

/** Returns the read key of an ended issue; a new ending of the same issue is unread again. */
export function endedKey(issueId: string, observedAt: string): string {
  return `o:${issueId}:${observedAt}`;
}

function parseStored(raw: string | null): Set<string> {
  if (raw === null) {
    return new Set();
  }
  try {
    const parsed: unknown = JSON.parse(raw);
    if (typeof parsed !== "object" || parsed === null) {
      return new Set();
    }
    const { v, keys } = parsed as { v?: unknown; keys?: unknown };
    if (v !== STORAGE_VERSION || !Array.isArray(keys)) {
      return new Set();
    }
    return new Set(keys.filter((key): key is string => typeof key === "string"));
  } catch {
    return new Set();
  }
}

function load(): Set<string> {
  try {
    return parseStored(localStorage.getItem(STORAGE_KEY));
  } catch {
    return new Set();
  }
}

function snapshot(): ReadonlySet<string> {
  current ??= load();
  return current;
}

function commit(next: ReadonlySet<string>): void {
  current = next;
  try {
    localStorage.setItem(STORAGE_KEY, JSON.stringify({ v: STORAGE_VERSION, keys: [...next] }));
  } catch {
    // Throwing storage leaves the set in page memory, which still serves this tab.
  }
  for (const listener of listeners) {
    listener();
  }
}

function subscribe(listener: () => void): () => void {
  listeners.add(listener);
  const handleStorage = (event: StorageEvent) => {
    if (event.key !== STORAGE_KEY && event.key !== null) {
      return;
    }
    current = load();
    listener();
  };
  window.addEventListener("storage", handleStorage);
  return () => {
    listeners.delete(listener);
    window.removeEventListener("storage", handleStorage);
  };
}

/** Adds the keys to the read set and stores it, keeping keys another tab wrote since the last load. */
export function markRead(keys: Iterable<string>): void {
  const next = new Set([...snapshot(), ...load()]);
  const held = next.size;
  for (const key of keys) {
    next.add(key);
  }
  if (next.size !== held || next.size !== snapshot().size) {
    commit(next);
  }
}

export function isRead(key: string): boolean {
  return snapshot().has(key);
}

/** Keeps only the given keys in the read set so it cannot outgrow the current items. */
export function pruneRead(keep: Iterable<string>): void {
  const kept = new Set(keep);
  const next = new Set([...snapshot()].filter((key) => kept.has(key)));
  if (next.size !== snapshot().size) {
    commit(next);
  }
}

/** Subscribes the component to the read set; the returned set is replaced, never mutated. */
export function useReadSet(): ReadonlySet<string> {
  return useSyncExternalStore(subscribe, snapshot);
}
