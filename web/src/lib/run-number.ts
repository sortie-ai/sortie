import { parseInstant } from "@/lib/format";

import type { RunResponse } from "@/api/types.gen";

export interface NumberedRun {
  run: RunResponse;
  number: number;
}

/** `total` is the count of all the issue's runs when `runs` holds only the newest of them, so the oldest run held is not numbered 1 while older runs exist. The stored `attempt` is the orchestrator's retry counter and says nothing of order. */
export function numberedRunsOf(runs: readonly RunResponse[], total: number): NumberedRun[] {
  const ordered = [...runs].sort(
    (a, b) =>
      (parseInstant(a.started_at) ?? 0) - (parseInstant(b.started_at) ?? 0) || a.run_id - b.run_id,
  );
  const unlisted = Math.max(0, total - ordered.length);
  return ordered.map((run, index) => ({ run, number: unlisted + index + 1 }));
}
