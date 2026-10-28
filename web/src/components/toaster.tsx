import { useCallback, useEffect, useRef, useState, useSyncExternalStore } from "react";

import { CheckIcon, TriangleAlertIcon, XIcon } from "@/components/icons";
import { cn } from "@/lib/cn";

import type { FocusEvent, MouseEvent } from "react";

type ToastVariant = "success" | "error" | "info";

export type ToastCloseCause = "undo" | "dismiss" | "timeout";

/** A labelled control in the slot that Undo uses; pressing it leaves the toast open. */
export interface ToastAction {
  label: string;
  onSelect: () => void;
}

/** A toast takes `undo` or `action`, not both. */
export type ToastOptions = {
  /** Defaults to `success`. */
  variant?: ToastVariant;
  /** Called once, as the toast starts to leave, after `undo` when Undo caused it. */
  onClose?: (cause: ToastCloseCause) => void;
  /** Keeps the toast up with no countdown until the reader dismisses it. */
  persists?: boolean;
} & ({ undo?: () => void; action?: never } | { action?: ToastAction; undo?: never });

interface ToastItem {
  id: number;
  title: string;
  message: string;
  variant: ToastVariant;
  undo: (() => void) | undefined;
  action: ToastAction | undefined;
  onClose: ((cause: ToastCloseCause) => void) | undefined;
  persists: boolean;
}

type ToastPhase = "entering" | "running" | "paused" | "leaving";

const LIFE_MS = 5000;
const FADE_MS = 100;
const STEP_MS = 1000;

const BADGE: Record<ToastVariant, string> = {
  success: "bg-surface-raised text-success",
  error: "bg-error-container text-error",
  info: "bg-surface-raised text-ink-body",
};

const ACTION_STYLE =
  "shrink-0 cursor-pointer text-body-sm font-medium text-link underline underline-offset-2 hover:text-link-hover focus-visible:outline-2 focus-visible:outline-focus-ring active:text-link-active";

const NO_TOASTS: readonly ToastItem[] = [];

let toasts = NO_TOASTS;
let nextId = 0;
const listeners = new Set<() => void>();

function subscribe(listener: () => void): () => void {
  listeners.add(listener);
  return () => {
    listeners.delete(listener);
  };
}

function getSnapshot(): readonly ToastItem[] {
  return toasts;
}

function removeToast(id: number): void {
  toasts = toasts.filter((item) => item.id !== id);
  listeners.forEach((listener) => {
    listener();
  });
}

/** Call it from an event handler, an effect, or an async callback, never during render; the {@link Toaster} must be mounted for it to show. When the reader presses Undo, `onClose` runs after `undo`. */
export function toast(title: string, message = "", options: ToastOptions = {}): void {
  nextId += 1;
  toasts = [
    ...toasts,
    {
      id: nextId,
      title,
      message,
      variant: options.variant ?? "success",
      undo: options.undo,
      action: options.action,
      onClose: options.onClose,
      persists: options.persists ?? false,
    },
  ];
  listeners.forEach((listener) => {
    listener();
  });
}

function prefersReducedMotion(): boolean {
  return window.matchMedia("(prefers-reduced-motion: reduce)").matches;
}

interface ToastViewProps {
  item: ToastItem;
  onLeave: (id: number, element: HTMLElement) => void;
  onRemove: (id: number) => void;
}

function ToastView({ item, onLeave, onRemove }: ToastViewProps) {
  const [isEntering, setIsEntering] = useState(true);
  const [isLeaving, setIsLeaving] = useState(false);
  const [bar, setBar] = useState({ percent: 100, ms: 0 });

  const rootRef = useRef<HTMLDivElement>(null);
  const phase = useRef<ToastPhase>("entering");
  const remaining = useRef(LIFE_MS);
  const startedAt = useRef(0);
  const isHovered = useRef(false);
  const hasFocusWithin = useRef(false);
  const endTimer = useRef<ReturnType<typeof setTimeout>>(undefined);
  const stepTimer = useRef<ReturnType<typeof setInterval>>(undefined);
  const removeTimer = useRef<ReturnType<typeof setTimeout>>(undefined);

  const leave = useCallback(
    (cause: ToastCloseCause, beforeClose?: () => void) => {
      if (phase.current === "leaving") {
        return;
      }
      phase.current = "leaving";
      clearTimeout(endTimer.current);
      clearInterval(stepTimer.current);
      if (rootRef.current !== null) {
        onLeave(item.id, rootRef.current);
      }
      setIsLeaving(true);
      removeTimer.current = setTimeout(() => {
        onRemove(item.id);
      }, FADE_MS);
      beforeClose?.();
      item.onClose?.(cause);
    },
    [onLeave, onRemove, item],
  );

  // Reduced motion shortens every transition to nothing, which would empty the
  // bar at once; stepping it once a second keeps the countdown readable.
  const startStepping = useCallback(() => {
    stepTimer.current = setInterval(() => {
      const left = remaining.current - (performance.now() - startedAt.current);
      if (left > 0) {
        setBar({ percent: left / (LIFE_MS / 100), ms: 0 });
      }
    }, STEP_MS);
  }, []);

  const run = useCallback(() => {
    phase.current = "running";
    if (item.persists) {
      return;
    }
    startedAt.current = performance.now();
    if (prefersReducedMotion()) {
      setBar({ percent: remaining.current / (LIFE_MS / 100), ms: 0 });
      startStepping();
    } else {
      setBar({ percent: 0, ms: remaining.current });
    }
    endTimer.current = setTimeout(() => {
      leave("timeout");
    }, remaining.current);
  }, [item.persists, leave, startStepping]);

  const pause = useCallback(() => {
    phase.current = "paused";
    clearTimeout(endTimer.current);
    clearInterval(stepTimer.current);
    remaining.current = Math.max(remaining.current - (performance.now() - startedAt.current), 0);
    setBar({ percent: remaining.current / (LIFE_MS / 100), ms: 0 });
  }, []);

  const syncHold = useCallback(() => {
    if (item.persists) {
      return;
    }
    const isHeld = isHovered.current || hasFocusWithin.current;
    if (phase.current === "running" && isHeld) {
      pause();
    } else if (phase.current === "paused" && !isHeld) {
      run();
    }
  }, [item.persists, pause, run]);

  useEffect(() => {
    let innerFrame = 0;
    // Two frames let the toast paint at its starting offset before it rises.
    const outerFrame = requestAnimationFrame(() => {
      innerFrame = requestAnimationFrame(() => {
        setIsEntering(false);
        if (isHovered.current || hasFocusWithin.current) {
          phase.current = "paused";
        } else {
          run();
        }
      });
    });
    return () => {
      cancelAnimationFrame(outerFrame);
      cancelAnimationFrame(innerFrame);
      clearTimeout(endTimer.current);
      clearInterval(stepTimer.current);
      clearTimeout(removeTimer.current);
    };
  }, [run]);

  const handleMouseEnter = () => {
    isHovered.current = true;
    syncHold();
  };

  const handleMouseLeave = () => {
    isHovered.current = false;
    syncHold();
  };

  const handleFocus = () => {
    hasFocusWithin.current = true;
    syncHold();
  };

  const handleBlur = (event: FocusEvent<HTMLDivElement>) => {
    if (event.relatedTarget instanceof Node && event.currentTarget.contains(event.relatedTarget)) {
      return;
    }
    hasFocusWithin.current = false;
    syncHold();
  };

  // A pointer press must not focus the button: focus would hold the countdown
  // paused and, at removal, send focus on to the next toast.
  const handlePointerPress = (event: MouseEvent) => {
    event.preventDefault();
  };

  const handleUndo = () => {
    leave("undo", item.undo);
  };

  const handleDismiss = () => {
    leave("dismiss");
  };

  const handleAction = () => {
    item.action?.onSelect();
  };

  const Mark = item.variant === "error" ? TriangleAlertIcon : CheckIcon;

  return (
    <div
      ref={rootRef}
      data-toast-id={item.id}
      onMouseEnter={handleMouseEnter}
      onMouseLeave={handleMouseLeave}
      onFocus={handleFocus}
      onBlur={handleBlur}
      className={cn(
        "pointer-events-auto relative overflow-hidden rounded-xl border border-line bg-canvas px-md pt-sm pb-md shadow-lg transition-[opacity,translate] duration-(--duration-enter-small) ease-enter",
        isEntering && "translate-y-(--offset-centered) opacity-0",
        isLeaving && "opacity-0 duration-(--duration-exit) ease-exit",
      )}
    >
      <div className="flex items-start gap-sm">
        <span
          className={cn(
            "mt-xxs flex size-lg shrink-0 items-center justify-center rounded-full",
            BADGE[item.variant],
          )}
        >
          <Mark />
        </span>
        <div className="min-w-0 flex-1 wrap-anywhere">
          <p className="text-body-md font-medium text-ink">{item.title}</p>
          {item.message === "" ? undefined : (
            <p className="text-body-sm text-ink-muted">{item.message}</p>
          )}
        </div>
        {item.undo === undefined ? undefined : (
          <button
            type="button"
            onClick={handleUndo}
            onMouseDown={handlePointerPress}
            className={ACTION_STYLE}
          >
            Undo
          </button>
        )}
        {item.action === undefined ? undefined : (
          <button
            type="button"
            onClick={handleAction}
            onMouseDown={handlePointerPress}
            className={ACTION_STYLE}
          >
            {item.action.label}
          </button>
        )}
        <button
          type="button"
          onClick={handleDismiss}
          onMouseDown={handlePointerPress}
          aria-label="Dismiss"
          data-dismiss=""
          className="flex size-lg shrink-0 cursor-pointer items-center justify-center rounded-lg text-ink-muted hover:bg-state-hover focus-visible:outline-2 focus-visible:outline-focus-ring"
        >
          <XIcon />
        </button>
      </div>
      {item.persists ? undefined : (
        <span className="absolute inset-x-0 bottom-0 h-1 bg-surface-raised" aria-hidden="true">
          <span
            className="block h-full bg-primary transition-[width] ease-linear"
            style={{ width: `${bar.percent}%`, transitionDuration: `${bar.ms}ms` }}
          />
        </span>
      )}
    </div>
  );
}

/** Mount it once; Alt+T focuses the newest toast. */
export function Toaster() {
  const items = useSyncExternalStore(subscribe, getSnapshot);
  const regionRef = useRef<HTMLElement>(null);
  const leavingIds = useRef(new Set<number>());
  const returnTarget = useRef<HTMLElement | null>(null);

  const handleLeave = useCallback((id: number, element: HTMLElement) => {
    leavingIds.current.add(id);
    if (!element.contains(document.activeElement)) {
      return;
    }
    const stack = getSnapshot();
    const index = stack.findIndex((item) => item.id === id);
    const byProximity = [...stack.slice(index + 1), ...stack.slice(0, index).reverse()];
    const neighbor = byProximity.find((item) => !leavingIds.current.has(item.id));
    const target =
      neighbor === undefined
        ? returnTarget.current
        : regionRef.current?.querySelector<HTMLElement>(
            `[data-toast-id="${neighbor.id}"] [data-dismiss]`,
          );
    if (target?.isConnected === true) {
      target.focus();
    }
  }, []);

  const handleRemove = useCallback((id: number) => {
    leavingIds.current.delete(id);
    removeToast(id);
  }, []);

  useEffect(() => {
    const handleKeyDown = (event: KeyboardEvent) => {
      if (!event.altKey || event.code !== "KeyT") {
        return;
      }
      const newest = [...getSnapshot()].reverse().find((item) => !leavingIds.current.has(item.id));
      if (newest === undefined) {
        return;
      }
      event.preventDefault();
      regionRef.current
        ?.querySelector<HTMLElement>(`[data-toast-id="${newest.id}"] button`)
        ?.focus();
    };
    document.addEventListener("keydown", handleKeyDown);
    return () => {
      document.removeEventListener("keydown", handleKeyDown);
    };
  }, []);

  const handleFocus = (event: FocusEvent<HTMLElement>) => {
    if (event.relatedTarget instanceof Node && event.currentTarget.contains(event.relatedTarget)) {
      return;
    }
    returnTarget.current = event.relatedTarget instanceof HTMLElement ? event.relatedTarget : null;
  };

  return (
    <section
      ref={regionRef}
      onFocus={handleFocus}
      aria-label="Notifications (Alt+T)"
      aria-live="polite"
      className="pointer-events-none fixed right-md bottom-md z-(--z-toast) flex w-[min(24rem,calc(100vw-2*var(--spacing-md)))] flex-col gap-xs"
    >
      {items.map((item) => (
        <ToastView key={item.id} item={item} onLeave={handleLeave} onRemove={handleRemove} />
      ))}
    </section>
  );
}
