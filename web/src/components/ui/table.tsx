import { ArrowDownIcon, ArrowUpIcon, ChevronsUpDownIcon } from "@/components/icons";
import { Card } from "@/components/ui/card";
import { HelpHint } from "@/components/ui/hint";
import { cn } from "@/lib/cn";

import type { TableSort } from "@/components/use-table-sort";
import type { ComponentProps, ReactNode } from "react";

/** Props of {@link Table}. */
export interface TableProps extends Omit<ComponentProps<"table">, "title"> {
  /** The heading over the table, drawn as a title bar; leave it out when the table sits under a heading that covers more than the table. */
  title?: ReactNode;
  /** Keeps the table from narrowing below a width that fits its columns, so it scrolls sideways instead. */
  isWide?: boolean;
  /** Narrows the side padding of every cell, for a table in a narrow column. */
  isCompact?: boolean;
}

/**
 * Renders every table of the dashboard: a rounded frame with an optional title bar around a
 * sideways-scrolling `<table>`. Every other prop reaches the `<table>`. A look a table needs
 * that the parts below lack is added to them, never drawn on a screen.
 */
export function Table({
  title,
  isWide = false,
  isCompact = false,
  className,
  ...props
}: TableProps) {
  return (
    <Card title={title}>
      <div className="overflow-x-auto">
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

/** Props of {@link TableHeader}. */
export interface TableHeaderProps {
  children: ReactNode;
}

/** Renders the single header row; its children are {@link TableHead} or {@link SortableHead}. */
export function TableHeader({ children }: TableHeaderProps) {
  return (
    <thead className="bg-surface text-body-sm font-medium text-ink-muted">
      <tr>{children}</tr>
    </thead>
  );
}

/** Renders one body. A table holds several when it groups its rows. */
export function TableBody({ className, ...props }: ComponentProps<"tbody">) {
  return (
    <tbody className={cn("divide-y divide-line border-t border-line", className)} {...props} />
  );
}

const ROW_LOOK = {
  plain: "",
  link: "cursor-pointer",
  group: "cursor-pointer bg-surface",
} as const;

/** Props of {@link TableRow}. */
export interface TableRowProps extends ComponentProps<"tr"> {
  /** `link` opens something; `group` is that on a row that heads a body's rows. Every row tints under the pointer. */
  look?: keyof typeof ROW_LOOK;
}

/** Renders a body row. */
export function TableRow({ look = "plain", className, ...props }: TableRowProps) {
  return (
    <tr
      className={cn(
        "outline-focus-ring transition-colors duration-(--duration-feedback) hover:bg-state-hover focus-visible:outline-2 focus-visible:-outline-offset-2",
        ROW_LOOK[look],
        className,
      )}
      {...props}
    />
  );
}

/** Props of {@link TableHead}. */
export interface TableHeadProps {
  align?: "left" | "right";
  /** The column's definition, shown as a tooltip. */
  title?: string;
  /** Carries the column's width only. */
  className?: string;
  children: ReactNode;
}

/** Renders the header cell of a column that does not sort. */
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

/** Props of {@link SortableHead}. */
export interface SortableHeadProps extends TableHeadProps {
  /** The column's id in the sort. */
  columnId: string;
  sort: TableSort;
  /** A note behind a question-mark icon after the header, for a definition too long for the header itself. */
  help?: string;
}

/** Renders the header cell of a column that sorts: a button that sorts by the column, with `aria-sort` on the cell. */
export function SortableHead({
  columnId,
  sort,
  align = "left",
  title,
  help,
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
      {help === undefined ? undefined : (
        <span className="ml-xxs inline-flex align-middle">
          <HelpHint hint={help} label="About this column" />
        </span>
      )}
    </th>
  );
}

const CELL_LOOK = {
  text: "",
  muted: "text-ink-muted",
  number: "text-right",
  "number-muted": "text-right text-ink-muted",
} as const;

/** Props of {@link TableCell}. */
export interface TableCellProps extends ComponentProps<"td"> {
  /** `number` right-aligns a figure; `muted` and `number-muted` quiet a cell that holds nothing worth reading, such as a zero. */
  look?: keyof typeof CELL_LOOK;
  /** A muted fact after the content on its line, such as the reason beside a count. */
  aside?: ReactNode;
  /** A muted second line under the content. */
  note?: ReactNode;
  /** Lets a value that can run long wrap, so the table fits its frame instead of scrolling sideways. */
  isWrapping?: boolean;
}

/** Renders a body cell; figures use tabular digits and only an `isWrapping` cell wraps. */
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
        "px-md py-sm align-baseline tabular-nums",
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

/** Props of {@link TableSpanCell}. */
export interface TableSpanCellProps {
  /** The number of columns the table has. */
  colSpan: number;
  /** Trims the block padding for a cell that holds a button. */
  isCompact?: boolean;
  children: ReactNode;
}

/** Renders a cell across the whole row that lays out its own content, such as a group's heading. */
export function TableSpanCell({ colSpan, isCompact = false, children }: TableSpanCellProps) {
  return (
    <td colSpan={colSpan} className={cn("px-md", isCompact ? "py-xs" : "py-sm")}>
      {children}
    </td>
  );
}
