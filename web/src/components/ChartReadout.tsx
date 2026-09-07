"use client";

import { useState } from "react";
import { formatInteger } from "@/utils/format";
import { applicableRows, type TooltipRow } from "./ChartTooltip";

/** A named set of the rows a chart's tooltip reads out, shared with the inspector and the table, so
 * nothing a pointer can see is only available to a pointer. */
export type ReadoutGroup = { title: string; rows: TooltipRow[] };

/** The rows of every group, in order, with a key that stays unique across groups. */
function allRows(groups: readonly ReadoutGroup[]): { key: string; row: TooltipRow }[] {
  return groups.flatMap((g) => g.rows.map((row) => ({ key: `${g.title}-${row.label}`, row })));
}

/**
 * Keyboard access to every point of a chart: a native range input picks one and the rows its tooltip
 * would show are read out in a live region. Until the reader moves the slider it follows the newest
 * point; the first move pins it.
 */
export function PointInspector({
  points,
  groups,
  note,
  title,
  heading = "Point inspector",
  selectLabel = "Select a point to read its values",
}: {
  points: readonly Record<string, unknown>[];
  groups: readonly ReadoutGroup[];
  note?: (row: Record<string, unknown>) => string | null;
  /** What names the selected point: a bucket time, a block, a place on a relative axis. */
  title: (row: Record<string, unknown>) => string;
  heading?: string;
  selectLabel?: string;
}) {
  // Null follows the newest point; a number is the reader's own choice.
  const [pinned, setPinned] = useState<number | null>(null);
  if (points.length === 0) return null;
  const last = points.length - 1;
  const index = pinned === null ? last : Math.max(0, Math.min(last, pinned));
  const row = points[index];
  const extra = note?.(row) ?? null;
  return (
    <div className="vw-card p-3 text-xs text-ink-2">
      <label className="flex flex-wrap items-center gap-3">
        <span className="font-semibold text-ink">{heading}</span>
        <input
          type="range"
          min={0}
          max={last}
          value={index}
          onChange={(e) => setPinned(Number(e.target.value))}
          aria-label={selectLabel}
          className="min-w-[160px] flex-1"
        />
        <span className="num text-ink">{title(row)}</span>
      </label>
      <dl className="num mt-2 grid grid-cols-[auto_1fr] gap-x-3 gap-y-0.5" aria-live="polite">
        {groups.flatMap((g) =>
          applicableRows(g.rows, row).map((r) => (
            <div key={`${g.title}-${r.label}`} className="contents">
              <dt className="text-ink-3">{r.label}</dt>
              <dd className="text-ink">{r.value(row)}</dd>
            </div>
          )),
        )}
      </dl>
      {extra ? <p className="mt-1 border-t border-hairline pt-1">{extra}</p> : null}
    </div>
  );
}

/**
 * Every point of a chart as a table. Its points are taken when it is opened and held: a live chart moves
 * several times a second, and a table that rewrote itself under the reader would be unreadable.
 */
export function PointTable({
  points,
  groups,
  title,
  caption,
  summary,
  timeLabel = "point",
}: {
  points: readonly Record<string, unknown>[];
  groups: readonly ReadoutGroup[];
  title: (row: Record<string, unknown>) => string;
  caption: string;
  summary: string;
  timeLabel?: string;
}) {
  const [held, setHeld] = useState<readonly Record<string, unknown>[] | null>(null);
  const columns = allRows(groups);
  return (
    <details className="text-xs text-ink-2" onToggle={(e) => setHeld((e.currentTarget as HTMLDetailsElement).open ? [...points] : null)}>
      <summary className="cursor-pointer select-none">
        {summary} ({formatInteger(points.length)} points)
      </summary>
      {held ? (
        <div className="mt-2 max-h-[360px] overflow-auto">
          <table className="num w-full min-w-[420px] text-left">
            <caption className="sr-only">{caption}</caption>
            <thead className="sticky top-0 bg-surface text-ink-3">
              <tr>
                <th scope="col" className="py-1 pr-3 font-medium">
                  {timeLabel}
                </th>
                {columns.map((c) => (
                  <th key={c.key} scope="col" className="py-1 pr-3 font-medium">
                    {c.row.label}
                  </th>
                ))}
              </tr>
            </thead>
            <tbody>
              {held.map((row, i) => (
                <tr key={i} className="border-t border-hairline">
                  <th scope="row" className="py-1 pr-3 font-normal">
                    {title(row)}
                  </th>
                  {columns.map((c) => (
                    <td key={c.key} className="py-1 pr-3">
                      {c.row.when === undefined || c.row.when(row) ? c.row.value(row) : "n/a"}
                    </td>
                  ))}
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      ) : null}
    </details>
  );
}

/** A chart's values without a pointer: one point at a time in the inspector, all of them in the table. */
export function ChartReadout(props: {
  points: readonly Record<string, unknown>[];
  groups: readonly ReadoutGroup[];
  title: (row: Record<string, unknown>) => string;
  note?: (row: Record<string, unknown>) => string | null;
  heading?: string;
  selectLabel?: string;
  caption: string;
  summary: string;
  timeLabel?: string;
}) {
  if (props.points.length === 0) return null;
  return (
    <div className="flex flex-col gap-2">
      <PointInspector points={props.points} groups={props.groups} note={props.note} title={props.title} heading={props.heading} selectLabel={props.selectLabel} />
      <PointTable points={props.points} groups={props.groups} title={props.title} caption={props.caption} summary={props.summary} timeLabel={props.timeLabel} />
    </div>
  );
}
