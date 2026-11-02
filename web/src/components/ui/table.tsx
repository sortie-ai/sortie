import { ArrowDownIcon, ArrowUpIcon, ChevronsUpDownIcon } from "@/components/icons";
import { Card } from "@/components/ui/card";
import { cn } from "@/lib/cn";

import type { TableSort } from "@/components/use-table-sort";
import type { ComponentProps, FocusEvent, ReactNode, UIEvent } from "react";

export interface TableProps extends ComponentProps<"table"> {
  /** Keeps the table from narrowing below a width that fits its columns, so it scrolls sideways instead. */
  isWide?: boolean;
  isCompact?: boolean;
  /** How many leading columns name the row and stay in view while the table scrolls sideways. */
  frozenColumns?: 1 | 2;
}

/** Records where a second frozen column starts and how far a focused cell must stay from the leading edge to clear them all. */
function measureFrozen(frame: HTMLDivElement) {
  const heads = [...frame.querySelectorAll("thead th")].slice(0, Number(frame.dataset.frozen));
  const isFrozen = heads.length > 0 && getComputedStyle(heads[0]).position === "sticky";
  const widths = heads.map((head) => (isFrozen ? (head as HTMLElement).offsetWidth : 0));
  frame.style.setProperty("--frozen-lead", `${widths[0] ?? 0}px`);
  frame.style.setProperty("--frozen-width", `${widths.reduce((sum, width) => sum + width, 0)}px`);
}

/** Marks a scrolled frame, which draws the frozen edge. */
function onFrozenScroll(event: UIEvent<HTMLDivElement>) {
  const frame = event.currentTarget;
  measureFrozen(frame);
  frame.toggleAttribute("data-scrolled", frame.scrollLeft > 0);
}

/** A cell the browser counts as in view can still sit under a frozen column; scrolling it again with the frozen width as scroll padding brings it clear. */
function onFrozenFocus(event: FocusEvent<HTMLDivElement>) {
  measureFrozen(event.currentTarget);
  event.target.scrollIntoView({ block: "nearest", inline: "nearest" });
}

/** Every other prop reaches the `<table>`. A look a table needs that the parts below lack is added to them, never drawn on a screen. */
export function Table({
  isWide = false,
  isCompact = false,
  frozenColumns,
  className,
  ...props
}: TableProps) {
  return (
    <Card>
      <div
        className={frozenColumns === undefined ? "overflow-x-auto" : "frozen-table"}
        data-frozen={frozenColumns}
        onScroll={frozenColumns === undefined ? undefined : onFrozenScroll}
        onFocus={frozenColumns === undefined ? undefined : onFrozenFocus}
      >
        <table
          className={cn(
            "w-full text-left text-body-md text-ink-body",
            isWide && "min-w-4xl",
            isCompact && "[&_td]:px-xs [&_th]:px-xs",
            className,
          )}
          {...props}
        />
      </div>
    </Card>
  );
}

export interface TableHeaderProps {
  children: ReactNode;
}

export function TableHeader({ children }: TableHeaderProps) {
  return (
    <thead className="bg-surface text-body-sm font-medium text-ink-muted">
      <tr>{children}</tr>
    </thead>
  );
}

/** Renders one body. A table holds several when it groups its rows; a rule sets each off, and a table without a head lets the frame draw its top. */
export function TableBody({ className, ...props }: ComponentProps<"tbody">) {
  return (
    <tbody
      className={cn(
        "divide-y divide-line-solid border-t border-line-solid first:border-t-0",
        className,
      )}
      {...props}
    />
  );
}

const ROW_LOOK = {
  plain: "",
  link: "cursor-pointer",
  group: "cursor-pointer bg-surface",
} as const;

export interface TableRowProps extends ComponentProps<"tr"> {
  /** `link` opens something; `group` is that on a row that heads a body's rows. Every row tints under the pointer. */
  look?: keyof typeof ROW_LOOK;
}

export function TableRow({ look = "plain", className, ...props }: TableRowProps) {
  return (
    <tr
      className={cn(
        "outline-focus-ring transition-[background-color] duration-(--duration-feedback) hover:bg-state-hover focus-visible:outline-2 focus-visible:-outline-offset-2",
        ROW_LOOK[look],
        className,
      )}
      {...props}
    />
  );
}

export interface TableHeadProps {
  align?: "left" | "right";
  title?: string;
  /** Carries the column's width only. */
  className?: string;
  children: ReactNode;
}

export function TableHead({ align = "left", title, className, children }: TableHeadProps) {
  return (
    <th
      scope="col"
      title={title}
      className={cn(
        "px-md py-sm font-medium whitespace-nowrap",
        align === "right" && "text-right",
        className,
      )}
    >
      {children}
    </th>
  );
}

export interface SortableHeadProps extends TableHeadProps {
  columnId: string;
  sort: TableSort;
}

export function SortableHead({
  columnId,
  sort,
  align = "left",
  title,
  className,
  children,
}: SortableHeadProps) {
  const direction = sort.directionOf(columnId);
  const Arrow =
    direction === "ascending"
      ? ArrowUpIcon
      : direction === "descending"
        ? ArrowDownIcon
        : ChevronsUpDownIcon;

  return (
    <th
      scope="col"
      title={title}
      aria-sort={direction ?? "none"}
      className={cn(
        "px-md py-sm font-medium whitespace-nowrap",
        align === "right" && "text-right",
        className,
      )}
    >
      <button
        type="button"
        onClick={() => sort.toggle(columnId)}
        className={cn(
          "inline-flex cursor-pointer items-center gap-xxs rounded transition-colors duration-(--duration-feedback) hover:text-ink focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-focus-ring",
          direction !== undefined && "text-ink",
        )}
      >
        {children}
        <Arrow className={cn("size-3", direction === undefined && "text-line-strong")} />
      </button>
    </th>
  );
}

const CELL_LOOK = {
  text: "",
  muted: "text-ink-muted",
  number: "text-right tabular-nums",
  "number-muted": "text-right text-ink-muted tabular-nums",
} as const;

export interface TableCellProps extends ComponentProps<"td"> {
  /** `number` right-aligns a figure in tabular digits; `muted` and `number-muted` quiet an empty cell, such as a zero. */
  look?: keyof typeof CELL_LOOK;
  /** A muted fact after the content on its line, such as the reason beside a count. */
  aside?: ReactNode;
  note?: ReactNode;
  /** Lets a value that can run long wrap, so the table fits its frame instead of scrolling sideways. */
  isWrapping?: boolean;
}

export function TableCell({
  look = "text",
  aside,
  note,
  isWrapping = false,
  className,
  children,
  ...props
}: TableCellProps) {
  return (
    <td
      className={cn(
        "px-md py-sm align-baseline",
        isWrapping ? "whitespace-normal" : "whitespace-nowrap",
        CELL_LOOK[look],
        className,
      )}
      {...props}
    >
      {children}
      {aside === undefined ? undefined : (
        <>
          {" "}
          <span className="text-ink-muted">{aside}</span>
        </>
      )}
      {note === undefined ? undefined : (
        <span className="block text-body-sm text-ink-muted">{note}</span>
      )}
    </td>
  );
}

export interface TableSpanCellProps {
  colSpan: number;
  isCompact?: boolean;
  children: ReactNode;
}

export function TableSpanCell({ colSpan, isCompact = false, children }: TableSpanCellProps) {
  return (
    <td colSpan={colSpan} className={cn("px-md", isCompact ? "py-xs" : "py-sm")}>
      {children}
    </td>
  );
}
