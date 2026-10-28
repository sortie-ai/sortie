let offsetMs = 0;

/** Fixes the offset between the server clock and this browser's clock from a state response's `generated_at`. */
export function setServerOffset(generatedAt: string): void {
  const served = Date.parse(generatedAt);
  if (Number.isNaN(served)) {
    return;
  }
  offsetMs = served - Date.now();
}

/** Returns the server's current time as epoch milliseconds; the offset is 0 before the first state response. */
export function now(): number {
  return Date.now() + offsetMs;
}

export function localMidnight(at: number): Date {
  const day = new Date(at);
  return new Date(day.getFullYear(), day.getMonth(), day.getDate());
}
