import { ExternalLinkIcon } from "@/components/icons";
import { cn } from "@/lib/cn";

import type { ReactNode } from "react";

/** How a link out of the app reads: in its surroundings' color, inked and underlined on hover. */
const EXTERNAL_LINK =
  "inline-flex min-w-0 max-w-full items-center gap-xxs rounded-sm transition-colors duration-(--duration-feedback) hover:text-ink hover:underline hover:underline-offset-2 focus-visible:outline-2 focus-visible:outline-focus-ring";

export interface ExternalLinkProps {
  href: string;
  className?: string;
  children: ReactNode;
}

/** A link that opens in a new tab, its words followed by the external-link mark. */
export function ExternalLink({ href, className, children }: ExternalLinkProps) {
  return (
    <a
      href={href}
      target="_blank"
      rel="noopener noreferrer"
      className={cn(EXTERNAL_LINK, className)}
    >
      {children}
      <ExternalLinkIcon className="size-3 shrink-0" />
    </a>
  );
}
