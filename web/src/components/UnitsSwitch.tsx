import type { Units, UnitsView } from "@/lib/units";

interface UnitsSwitchProps {
  view: UnitsView;
}

const OPTIONS: readonly { value: Units; label: string }[] = [
  { value: "tokens", label: "Tokens" },
  { value: "cost", label: "Cost" },
];

const OPTION =
  "inline-flex h-lg items-center rounded-lg px-sm text-body-md font-medium transition-colors duration-(--duration-feedback) focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-focus-ring aria-pressed:bg-canvas aria-pressed:text-ink not-aria-pressed:text-ink-body not-aria-pressed:hover:bg-state-hover not-aria-pressed:hover:text-ink not-aria-pressed:active:bg-state-pressed";

/** Renders the Tokens and Cost choice, and nothing while the screen offers none. */
export function UnitsSwitch({ view }: UnitsSwitchProps) {
  if (!view.isOffered) {
    return null;
  }
  return (
    <span
      role="group"
      aria-label="Units"
      className="inline-flex gap-xxs rounded-xl bg-surface-sunken p-xxs"
    >
      {OPTIONS.map((option) => (
        <button
          key={option.value}
          type="button"
          aria-pressed={option.value === view.chosen}
          className={OPTION}
          onClick={() => {
            view.choose(option.value);
          }}
        >
          {option.label}
        </button>
      ))}
    </span>
  );
}
