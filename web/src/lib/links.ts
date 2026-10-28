/** Returns the URL only when its scheme is `http:` or `https:`: tracker and forge URLs are served data, so a `javascript:` URL must never reach an anchor. */
export function safeHttpUrl(url: string | null | undefined): string | null {
  if (url === null || url === undefined) {
    return null;
  }
  try {
    const { protocol } = new URL(url);
    return protocol === "http:" || protocol === "https:" ? url : null;
  } catch {
    return null;
  }
}
