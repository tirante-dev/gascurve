"use client";

import { SERIES_RANGES } from "@/lib/api/series";
import type { SeriesRange } from "@/types";
import { RangeTabs, type RangeOption } from "./RangeTabs";

const LABELS: Record<SeriesRange, string> = { "1h": "1h", "24h": "24h", "30d": "30d", all: "All" };

/** The history section's ranges, in the order the api lists them. */
export const HISTORY_OPTIONS: readonly RangeOption<SeriesRange>[] = SERIES_RANGES.map((range) => ({ value: range, label: LABELS[range] }));

export function HistoryTabs({ range, onChange, loading }: { range: SeriesRange; onChange: (range: SeriesRange) => void; loading?: boolean }) {
  return <RangeTabs options={HISTORY_OPTIONS} value={range} onChange={onChange} label="History range" loading={loading} />;
}
