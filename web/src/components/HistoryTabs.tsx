"use client";

import { SERIES_RANGES } from "@/lib/api/series";
import type { SeriesRange } from "@/types";

const LABELS: Record<SeriesRange, string> = { "1h": "1h", "24h": "24h", "30d": "30d", all: "All" };

export function HistoryTabs({ range, onChange, loading }: { range: SeriesRange; onChange: (range: SeriesRange) => void; loading?: boolean }) {
  return (
    <div className="flex items-center gap-3" role="tablist" aria-label="History range">
      <div className="vw-control inline-flex p-0.5">
        {SERIES_RANGES.map((r) => {
          const selected = r === range;
          return (
            <button
              key={r}
              type="button"
              role="tab"
              aria-selected={selected}
              className={`num rounded-full px-3 py-1 text-sm ${selected ? "vw-tab-on bg-accent font-medium text-accent-ink" : "text-ink-2 hover:text-ink"}`}
              onClick={() => onChange(r)}
            >
              {LABELS[r]}
            </button>
          );
        })}
      </div>
      {loading ? <span className="text-xs text-ink-3">updating</span> : null}
    </div>
  );
}
