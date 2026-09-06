"use client";

import type { TooltipContentProps } from "recharts";
import { Swatch } from "./primitives";

export type TooltipRow = {
  label: string;
  color?: string;
  kind?: "rect" | "line";
  value: (row: Record<string, unknown>) => string;
};

/**
 * One tooltip, every series: values lead in the mono face, series names follow.
 * `rows` read from the hovered data row so the readout never depends on
 * which mark the pointer landed on.
 */
export function ChartTooltip({ active, payload, label, rows, title, note }: Partial<TooltipContentProps> & { rows: TooltipRow[]; title: (label: number) => string; note?: (row: Record<string, unknown>) => string | null }) {
  if (!active || !payload || payload.length === 0) return null;
  const row = (payload[0]?.payload ?? {}) as Record<string, unknown>;
  const extra = note?.(row) ?? null;
  return (
    <div className="rounded-md border border-hairline bg-surface px-3 py-2 text-xs shadow-sm">
      <div className="mb-1 text-ink-3">{title(Number(label))}</div>
      <table className="border-separate border-spacing-y-0.5">
        <tbody>
          {rows.map((r) => (
            <tr key={r.label}>
              <td className="num pr-3 text-right font-medium text-ink">{r.value(row)}</td>
              <td className="text-ink-2">
                {r.color ? (
                  <span className="mr-1.5">
                    <Swatch color={r.color} kind={r.kind ?? "line"} />
                  </span>
                ) : null}
                {r.label}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
      {extra ? <div className="mt-1 max-w-[28ch] border-t border-hairline pt-1 text-ink-2">{extra}</div> : null}
    </div>
  );
}
