import { keyTip } from "@/app/key-tips";
import { ExternalLinkIcon } from "@/components/icons";
import { IssueTitleLink } from "@/components/IssueTitleLink";
import { ServedText } from "@/components/ServedText";
import { Button, buttonStyles } from "@/components/ui/button";
import { linkActionOf } from "@/components/use-attention-actions";
import { cn } from "@/lib/cn";
import { fmtAgo } from "@/lib/format";
import { rowStyles, secondLineColor } from "@/lib/rows";

import type { MouseEvent } from "react";

import type { AttentionItemResponse } from "@/api/types.gen";

export interface AttentionItemProps {
  item: AttentionItemResponse;
  navKey: string;
  /** Server-clock time in epoch milliseconds that the item's age measures against. */
  now: number;
  isSelected: boolean;
  /** Whether the selected-row look shows, which it does from the first navigation key on. */
  showSelection: boolean;
  isPending: boolean;
  onOpen: (item: AttentionItemResponse) => void;
  onAcknowledge: (item: AttentionItemResponse) => void;
  onAction: (item: AttentionItemResponse) => void;
}

export function AttentionItem({
  item,
  navKey,
  now,
  isSelected,
  showSelection,
  isPending,
  onOpen,
  onAcknowledge,
  onAction,
}: AttentionItemProps) {
  const link = linkActionOf(item);
  const key = item.display_identifier || item.issue_identifier;
  const isCritical = item.severity === "critical";
  const isOpenable = Boolean(item.issue_identifier);
  const isLook = isSelected && showSelection;

  function handleClick(event: MouseEvent<HTMLLIElement>) {
    if (!isOpenable) {
      return;
    }
    if (event.target instanceof Element && event.target.closest("a, button") !== null) {
      return;
    }
    onOpen(item);
  }

  function handleLinkClick(event: MouseEvent<HTMLAnchorElement>) {
    if (isPending) {
      event.preventDefault();
      return;
    }
    onAction(item);
  }

  return (
    <li
      data-nav-key={navKey}
      onClick={handleClick}
      className={cn(
        "grid grid-cols-[minmax(0,1fr)_auto] gap-x-md gap-y-xxs p-md max-md:grid-cols-[minmax(0,1fr)]",
        rowStyles(isLook, isOpenable),
      )}
    >
      <p className="col-span-2 flex items-baseline justify-between gap-x-md max-md:col-span-1">
        <strong
          className={cn("text-body-md font-bold", isCritical ? "text-error" : "text-warning")}
        >
          <ServedText text={item.reason_text} />
        </strong>
        <span
          className={cn("text-body-sm whitespace-nowrap tabular-nums", secondLineColor(isLook))}
        >
          {item.since === null ? undefined : fmtAgo(item.since, now)}
        </span>
      </p>
      <p className="flex min-w-0 flex-wrap items-baseline gap-x-sm gap-y-xxs">
        {item.issue_title ? (
          <span className="text-body-md font-medium text-ink">
            {item.issue_identifier ? (
              <IssueTitleLink identifier={item.issue_identifier}>{item.issue_title}</IssueTitleLink>
            ) : (
              item.issue_title
            )}
          </span>
        ) : undefined}
        {key ? (
          <span
            className={cn("text-body-sm whitespace-nowrap tabular-nums", secondLineColor(isLook))}
          >
            {item.issue_identifier && !item.issue_title ? (
              <IssueTitleLink identifier={item.issue_identifier}>{key}</IssueTitleLink>
            ) : (
              key
            )}
          </span>
        ) : undefined}
      </p>
      <p className="text-body-md text-ink-body">
        <ServedText text={item.why_text} />
      </p>
      <p className={cn("text-body-sm", secondLineColor(isLook))}>
        <ServedText text={item.clears_text} />
      </p>
      <div className="col-start-2 row-span-3 row-start-2 flex items-end justify-end max-md:col-start-1 max-md:row-span-1 max-md:row-start-auto max-md:mt-xs max-md:justify-start">
        <span className="flex flex-wrap gap-xs">
          <Button
            variant="primary"
            disabled={isPending}
            title={keyTip("Acknowledge", "E")}
            onClick={() => onAcknowledge(item)}
          >
            {isPending ? "Acknowledging" : "Acknowledge"}
          </Button>
          {link === null ? undefined : (
            <a
              href={link.url}
              target="_blank"
              rel="noopener noreferrer"
              title={keyTip(link.title, "A")}
              aria-disabled={isPending}
              data-disabled={isPending ? "" : undefined}
              onClick={handleLinkClick}
              className={buttonStyles("secondary")}
            >
              <ExternalLinkIcon />
              {link.label}
            </a>
          )}
        </span>
      </div>
    </li>
  );
}
