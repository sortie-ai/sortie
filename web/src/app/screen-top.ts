import { useLayoutEffect, useRef } from "react";
import { useLocation, useNavigationType } from "react-router";

/** The link state of a return to the screen a page was opened from, such as the breadcrumb or `Esc`. */
export const RETURN = { isReturn: true } as const;

function isReturn(state: unknown): boolean {
  return typeof state === "object" && state !== null && "isReturn" in state;
}

/**
 * Opens a screen at its top whenever the reader goes to it, and again on a link to the screen already
 * open; a hash opens it at its section. A return (browser history or {@link RETURN}) and a change of
 * the query alone keep the position ScrollRestoration brings back.
 */
export function useScreenTop() {
  const location = useLocation();
  const type = useNavigationType();
  const previous = useRef(location);

  useLayoutEffect(() => {
    const from = previous.current;
    previous.current = location;
    if (type === "POP" || isReturn(location.state)) {
      return;
    }
    const isQueryChange = from.pathname === location.pathname && from.search !== location.search;
    if (isQueryChange) {
      return;
    }
    const section = location.hash === "" ? null : document.getElementById(location.hash.slice(1));
    if (section === null) {
      window.scrollTo({ top: 0 });
    } else {
      section.scrollIntoView();
    }
  }, [location, type]);
}
