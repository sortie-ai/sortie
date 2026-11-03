import { Panel } from "@/components/Panel";

import { AttentionItem } from "./AttentionItem";
import { attentionNavKey } from "./rows";

import type { AttentionItemResponse } from "@/api/types.gen";

export interface AttentionListProps {
  /** The unacknowledged items in the order the server serves them. */
  items: readonly AttentionItemResponse[];
  now: number;
  selectedKey: string | undefined;
  showSelection: boolean;
  pending: ReadonlySet<string>;
  onOpen: (item: AttentionItemResponse) => void;
  onAcknowledge: (item: AttentionItemResponse) => void;
  onAction: (item: AttentionItemResponse) => void;
}

/** Its items in served order, never reordered by the client. */
export function AttentionList({
  items,
  now,
  selectedKey,
  showSelection,
  pending,
  onOpen,
  onAcknowledge,
  onAction,
}: AttentionListProps) {
  return (
    <Panel title="Needs attention" meta="critical first, then oldest">
      <ul className="divide-y divide-line overflow-hidden rounded-xl border border-line bg-canvas">
        {items.map((item) => {
          const navKey = attentionNavKey(item.id);
          return (
            <AttentionItem
              key={item.id}
              item={item}
              navKey={navKey}
              now={now}
              isSelected={selectedKey === navKey}
              showSelection={showSelection}
              isPending={pending.has(item.id)}
              onOpen={onOpen}
              onAcknowledge={onAcknowledge}
              onAction={onAction}
            />
          );
        })}
      </ul>
    </Panel>
  );
}
