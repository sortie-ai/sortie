import { Button as BaseButton } from "@base-ui/react/button";

import { cn } from "@/lib/cn";

/** The visual weight of a button; `primary` marks the one action of a view. */
export type ButtonVariant = "primary" | "secondary" | "ghost";

const BASE =
  "inline-flex h-(--size-control) items-center justify-center gap-xs rounded px-sm text-body-md font-medium whitespace-nowrap select-none cursor-pointer transition-colors duration-(--duration-feedback) focus-visible:outline-2 focus-visible:outline-focus-ring data-disabled:cursor-default";

// Ghost buttons on the dark rail hover with the rail's panel colors: the rail sets
// --ghost-hover and --ghost-pressed, and everywhere else the fallbacks apply.
const VARIANTS: Record<ButtonVariant, string> = {
  primary:
    "bg-primary text-on-primary focus-visible:outline-offset-2 not-data-disabled:hover:bg-primary-hover not-data-disabled:active:bg-primary-active data-disabled:bg-disabled data-disabled:text-on-disabled",
  secondary:
    "border border-line-medium bg-transparent text-ink focus-visible:-outline-offset-1 not-data-disabled:hover:bg-state-hover not-data-disabled:active:bg-state-pressed data-disabled:border-line data-disabled:bg-disabled data-disabled:text-on-disabled",
  ghost:
    "bg-transparent text-ink-body focus-visible:-outline-offset-1 not-data-disabled:hover:bg-(--ghost-hover,var(--color-state-hover)) not-data-disabled:hover:text-ink not-data-disabled:active:bg-(--ghost-pressed,var(--color-state-pressed)) data-disabled:text-on-disabled",
};

/** Returns the class list of a button variant, for triggers that must look like a button. */
export function buttonStyles(variant: ButtonVariant, className?: string): string {
  return cn(BASE, VARIANTS[variant], className);
}

/** To render a link, pass `render={<a href={url} />}` with `nativeButton={false}`. */
export interface ButtonProps extends Omit<BaseButton.Props, "className"> {
  variant?: ButtonVariant;
  className?: string;
}

export function Button({ variant = "secondary", className, type, ...rest }: ButtonProps) {
  return (
    <BaseButton type={type ?? "button"} className={buttonStyles(variant, className)} {...rest} />
  );
}
