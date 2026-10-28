import { useQueryClient } from "@tanstack/react-query";
import { useCallback, useRef, useState } from "react";

import { ApiError, postCheckNow, postRetryNow } from "@/api/client";
import { queryKeys } from "@/api/queries";
import { toast } from "@/components/toaster";
import { failureMessage } from "@/components/use-attention-actions";

export interface IssueActions {
  retryNow(identifier: string): void;
  checkNow(identifier: string, kind: string): void;
  isRetrying(identifier: string): boolean;
  isChecking(identifier: string, kind: string): boolean;
}

function retryKey(identifier: string): string {
  return JSON.stringify(["retry", identifier]);
}

function checkKey(identifier: string, kind: string): string {
  return JSON.stringify(["check", identifier, kind]);
}

/** A request for a key already running is ignored, so a double activation sends one request; any answer refetches the issue and the state, paused or not. */
export function useIssueActions(): IssueActions {
  const client = useQueryClient();
  const [pending, setPending] = useState<ReadonlySet<string>>(new Set());
  const inFlight = useRef(new Set<string>());

  const run = useCallback(
    async (
      key: string,
      identifier: string,
      call: () => Promise<{ body: { outcome_text: string } }>,
    ) => {
      if (inFlight.current.has(key)) {
        return;
      }
      inFlight.current.add(key);
      setPending(new Set(inFlight.current));
      let isAnswered = true;
      try {
        const { body } = await call();
        toast(body.outcome_text);
      } catch (error) {
        toast(failureMessage(error), "", { variant: "error" });
        isAnswered = error instanceof ApiError && error.status !== 0;
      } finally {
        inFlight.current.delete(key);
        setPending(new Set(inFlight.current));
      }
      if (isAnswered) {
        await Promise.all([
          client.refetchQueries({ queryKey: queryKeys.issue(identifier) }),
          client.refetchQueries({ queryKey: queryKeys.state() }),
        ]);
      }
    },
    [client],
  );

  const retryNow = useCallback(
    (identifier: string) => {
      void run(retryKey(identifier), identifier, () => postRetryNow(identifier));
    },
    [run],
  );

  const checkNow = useCallback(
    (identifier: string, kind: string) => {
      void run(checkKey(identifier, kind), identifier, () => postCheckNow(identifier, kind));
    },
    [run],
  );

  const isRetrying = useCallback(
    (identifier: string) => pending.has(retryKey(identifier)),
    [pending],
  );

  const isChecking = useCallback(
    (identifier: string, kind: string) => pending.has(checkKey(identifier, kind)),
    [pending],
  );

  return { retryNow, checkNow, isRetrying, isChecking };
}
