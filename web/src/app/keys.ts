import { useEffect, useRef } from "react";

/** `keys` are `KeyboardEvent.key` values compared case-insensitively; `shift` requires Shift; `mod` binds Ctrl or Command and works inside inputs; `chord` binds the key pressed within 1.2 s after `G`. */
export interface Binding {
  keys: string[];
  shift?: boolean;
  mod?: boolean;
  chord?: boolean;
  run: (event: KeyboardEvent) => void;
}

const CHORD_WINDOW_MS = 1_200;
const CHORD_LEADER = "g";
const NATIVE_ACTIVATION = "a, button, summary, [role='button']";
const ACTIVATION_KEYS = new Set(["enter", " "]);

export function modifierLabel(): string {
  return /Mac|iPhone|iPad|iPod/.test(navigator.platform) ? "⌘" : "Ctrl";
}

function isEditable(target: EventTarget | null): boolean {
  if (!(target instanceof HTMLElement)) {
    return false;
  }
  return target.isContentEditable || ["INPUT", "TEXTAREA", "SELECT"].includes(target.tagName);
}

function keepsNativeActivation(target: EventTarget | null, key: string): boolean {
  return (
    ACTIVATION_KEYS.has(key) &&
    target instanceof Element &&
    target.closest(NATIVE_ACTIVATION) !== null
  );
}

function matches(binding: Binding, event: KeyboardEvent, key: string): boolean {
  if (!binding.keys.some((candidate) => candidate.toLowerCase() === key)) {
    return false;
  }
  if (binding.shift === true && !event.shiftKey) {
    return false;
  }
  return binding.mod === true ? event.ctrlKey || event.metaKey : true;
}

/** Keys are ignored in inputs, textareas, and editable elements, and while Ctrl, Meta, or Alt is held, except for `mod` bindings. */
export function useShortcuts(bindings: Binding[]): void {
  const latest = useRef(bindings);
  const chordAt = useRef<number | null>(null);

  useEffect(() => {
    latest.current = bindings;
  });

  useEffect(() => {
    const handleKeyDown = (event: KeyboardEvent) => {
      if (event.defaultPrevented) {
        return;
      }
      const key = event.key.toLowerCase();
      const mod = event.ctrlKey || event.metaKey;
      const paletteKey = latest.current.find((b) => b.mod === true && matches(b, event, key));
      if (paletteKey !== undefined) {
        event.preventDefault();
        paletteKey.run(event);
        return;
      }
      if (mod || event.altKey || isEditable(event.target)) {
        return;
      }
      const inChord = chordAt.current !== null && Date.now() - chordAt.current <= CHORD_WINDOW_MS;
      chordAt.current = null;
      if (inChord) {
        const chord = latest.current.find((b) => b.chord === true && matches(b, event, key));
        if (chord !== undefined) {
          event.preventDefault();
          chord.run(event);
        }
        return;
      }
      if (key === CHORD_LEADER && !event.shiftKey) {
        chordAt.current = Date.now();
        return;
      }
      if (keepsNativeActivation(event.target, key)) {
        return;
      }
      const binding = latest.current.find(
        (b) => b.mod !== true && b.chord !== true && matches(b, event, key),
      );
      if (binding !== undefined) {
        event.preventDefault();
        binding.run(event);
      }
    };
    document.addEventListener("keydown", handleKeyDown);
    return () => document.removeEventListener("keydown", handleKeyDown);
  }, []);
}
