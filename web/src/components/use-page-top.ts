import { useLayoutEffect, useState } from "react";

import type { RefObject } from "react";

/** Returns how far an element stands from the top of the page in pixels, kept current; 0 until it mounts. */
export function usePageTop(ref: RefObject<HTMLElement | null>): number {
  const [top, setTop] = useState(0);
  useLayoutEffect(() => {
    const element = ref.current;
    if (element === null) {
      return;
    }
    const measure = () => setTop(element.getBoundingClientRect().top + window.scrollY);
    measure();
    // Whatever moves the element, such as a notice above it, changes the page's size.
    const observer = new ResizeObserver(measure);
    observer.observe(document.body);
    return () => observer.disconnect();
  }, [ref]);
  return top;
}
