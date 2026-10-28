export type ThemeChoice = "system" | "light" | "dark";

const STORAGE_KEY = "sortie.theme";
const ORDER: ThemeChoice[] = ["system", "light", "dark"];

function readStored(): ThemeChoice {
  try {
    const stored = localStorage.getItem(STORAGE_KEY);
    return stored === "light" || stored === "dark" ? stored : "system";
  } catch {
    return "system";
  }
}

let current: ThemeChoice | undefined;

export function getTheme(): ThemeChoice {
  current ??= readStored();
  return current;
}

/** "system" clears both the root attribute and the stored choice; calling `setTheme(getTheme())` before the first render applies the stored choice. */
export function setTheme(choice: ThemeChoice): void {
  current = choice;
  const root = document.documentElement;
  if (choice === "system") {
    root.removeAttribute("data-theme");
  } else {
    root.setAttribute("data-theme", choice);
  }
  try {
    if (choice === "system") {
      localStorage.removeItem(STORAGE_KEY);
    } else {
      localStorage.setItem(STORAGE_KEY, choice);
    }
  } catch {
    // The choice still holds for this page when storage throws.
  }
}

export function cycleTheme(): ThemeChoice {
  const next = ORDER[(ORDER.indexOf(getTheme()) + 1) % ORDER.length];
  setTheme(next);
  return next;
}
