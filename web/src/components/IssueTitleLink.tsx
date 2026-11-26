import { useLocation } from "react-router";

import { Link } from "@/components/carried-links";
import { issueLinkOf } from "@/lib/issue-link";

import type { ReactNode } from "react";

export interface IssueTitleLinkProps {
  identifier: string;
  children: ReactNode;
}

/** A row opens its quick look or its page on a click; the title is a link to the page, so the page opens in a new tab and from the keyboard, and the row stays still. */
export function IssueTitleLink({ identifier, children }: IssueTitleLinkProps) {
  const location = useLocation();
  const link = issueLinkOf(identifier, location);

  return (
    <Link
      to={link.to}
      state={link.state}
      onClick={(event) => event.stopPropagation()}
      className="rounded hover:underline hover:underline-offset-2 focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-focus-ring"
    >
      {children}
    </Link>
  );
}
