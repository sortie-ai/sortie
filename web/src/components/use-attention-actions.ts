import { useQueryClient } from "@tanstack/react-query";
import { useCallback, useRef, useState } from "react";

import { ApiError, postAcknowledge } from "@/api/client";
import { queryKeys } from "@/api/queries";
import { toast } from "@/components/toaster";
import { safeHttpUrl } from "@/lib/links";
import { attentionKey, markRead } from "@/lib/unread";

import type { AttentionItemResponse } from "@/api/types.gen";

const NO_ANSWER = "Sortie did not answer. Nothing changed.";
const CLEARED = "This item cleared before it was acknowledged.";

export interface LinkAction {
  url: string;
  label: string;
  /** Tooltip without the key hint. */
  title: string;
}

/** Gives the link action of an item, or `null` when it has none or its URL is not `http:` or `https:`. */
export function linkActionOf(item: AttentionItemResponse): LinkAction | null {
  const url = safeHttpUrl(item.action.url);
  if (url === null) {
    return null;
  }
  if (item.action.kind === "open_issue") {
    const key = item.display_identifier || item.issue_identifier;
    return {
      url,
      label: "Open issue",
      title: key ? `Open ${key} in the tracker` : "Open issue",
    };
  }
  if (item.action.kind === "open_pull_request") {
    const number = item.evidence.pull_request?.number;
    const label = number === undefined ? "Open pull request" : `Open pull request #${number}`;
    return { url, label, title: label };
  }
  return null;
}

export function failureMessage(error: unknown): string {
  if (!(error instanceof ApiError) || error.status === 0) {
    return NO_ANSWER;
  }
  if (error.status === 404 && error.code === "attention_item_not_found") {
    return CLEARED;
  }
  return error.serverMessage ?? NO_ANSWER;
}

export interface AttentionActions {
  pending: ReadonlySet<string>;
  acknowledge: (item: AttentionItemResponse) => Promise<void>;
  runAction: (item: AttentionItemResponse) => void;
}

/** Acknowledging refetches the state after the response, paused or not, and toasts the outcome. */
export function useAttentionActions(): AttentionActions {
  const client = useQueryClient();
  const [pending, setPending] = useState<ReadonlySet<string>>(new Set());
  const inFlight = useRef(new Set<string>());

  const acknowledge = useCallback(
    async (item: AttentionItemResponse) => {
      if (inFlight.current.has(item.id)) {
        return;
      }
      inFlight.current.add(item.id);
      setPending(new Set(inFlight.current));
      try {
        const { status, body } = await postAcknowledge(item.id);
        if (status === 200) {
          markRead([attentionKey(item.id)]);
        }
        toast(body.outcome_text);
        if (status === 200) {
          await client.refetchQueries({ queryKey: queryKeys.state() });
        }
      } catch (error) {
        toast(failureMessage(error), "", { variant: "error" });
        if (error instanceof ApiError && error.code === "attention_item_not_found") {
          await client.refetchQueries({ queryKey: queryKeys.state() });
        }
      } finally {
        inFlight.current.delete(item.id);
        setPending(new Set(inFlight.current));
      }
    },
    [client],
  );

  const runAction = useCallback(
    (item: AttentionItemResponse) => {
      const link = linkActionOf(item);
      if (link === null) {
        if (item.action.kind === "acknowledge") {
          void acknowledge(item);
        }
        return;
      }
      markRead([attentionKey(item.id)]);
      window.open(link.url, "_blank", "noopener,noreferrer");
    },
    [acknowledge],
  );

  return { pending, acknowledge, runAction };
}
