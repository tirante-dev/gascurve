"use client";

import { memo, useMemo, useState, type ReactNode } from "react";
import { Area, AreaChart, CartesianGrid, ComposedChart, Line, ReferenceLine, ResponsiveContainer, Tooltip, XAxis, YAxis } from "recharts";
import type { PricerModel, Series, SeriesRange } from "@/types";
import { chartView } from "@/lib/chartViews";
import { bucketNote, describeAction, feeChartData, feeTooltipRows, formatFloor, type DrawnRow } from "@/lib/feeChart";
import { emptyRangeNote, type GapModel, type GapWindow } from "@/lib/gaps";
import { throughputAxis, throughputTick, type ThroughputAxis } from "@/lib/hero";
import { missingRuns, withMissingNote, withMissingNotes, type MissingRun, type MissingSeries, type Present } from "@/lib/missing";
import {
  hasUnknownSets,
  hasUnrecordedSplit,
  MARKER_COLOR,
  NULL_SPLIT_LABEL,
  segmentsFor,
  seriesColor,
  seriesCount,
  slotLabel,
  targetKey,
  UNKNOWN_COLOR,
  UNKNOWN_KEY,
  UNKNOWN_LABEL,
  unknownBacklogKey,
  type ChartPoint,
  type Segment,
} from "@/utils/chart";
import { formatDateTime, formatGas, formatGasPerSecond, formatInteger, formatSignificant, formatTick, unbroken } from "@/utils/format";
import { EnlargeLink } from "./ChartActions";
import { gapBands, GapNote, MissingDots, missingBandAreas, MissingNote } from "./ChartGaps";
import { ChartTooltip, type TooltipRow } from "./ChartTooltip";
import { PointInspector } from "./ChartReadout";
import { ChartFrame, Legend, TIME_AXIS_RIGHT, type ChartHeight } from "./primitives";

const SYNC_ID = "history";

/**
 * Gas ticks carry their unit ("30 Mgas"), so the axis reserves the width for
 * one, at the widest a backlog tick gets ("1.05 Ggas") rather than the
 * narrowest: a clipped tick label is not a label.
 */
const GAS_AXIS_WIDTH = 74;

/** The heights the history charts stand at on the network page; the enlarged views pass their own. */
export const SERIES_CHART_HEIGHT = 220;
export const BACKLOG_CHART_HEIGHT = 140;

/** True when segment `s` is the one in force for the hovered or selected row. */
function inForce(s: Segment): (row: Record<string, unknown>) => boolean {
  return (row) => Number(row.constraintSetId) === s.setId;
}

/**
 * True when `s` has a per-constraint contribution to show for the row: its
 * set is in force and the split was recorded. A point with a known set but a
 * null split has no per-constraint values at all, and zero is not one.
 */
function splitInForce(s: Segment): (row: Record<string, unknown>) => boolean {
  return (row) => inForce(s)(row) && row.splitKnown === true && typeof row[s.key] === "number";
}

/** A nullable chart value: the number when it is one, "n/a" when it was never recorded. */
function known(value: unknown, render: (v: number) => string): string {
  return typeof value === "number" ? render(value) : "n/a";
}

/** "C1 0.0034 · C2 3.2391" for the set in force at the point, or the unknown-split note (set unknown, or split not recorded). */
export function describeSplit(row: ChartPoint, segments: readonly Segment[]): string {
  if (!row.setKnown) return `${UNKNOWN_LABEL}: ${row.x.toFixed(4)}`;
  if (!row.splitKnown) return `${NULL_SPLIT_LABEL}: ${row.x.toFixed(4)}`;
  const own = segments.filter((s) => s.setId === row.constraintSetId);
  return own.map((s) => `C${s.index + 1} ${known(row[s.key], (v) => v.toFixed(4))}`).join(" · ");
}

/** "0.1234" for a known fee part, "n/a" when the destination split was not recorded. */
function formatFeePart(eth: number | null): string {
  return eth === null ? "n/a" : formatSignificant(eth, 4);
}

/**
 * Everything the history charts draw, derived once from a Series: the rows
 * themselves, the constraint segments the sets divide them into, and the
 * tooltip rows and legends that follow from those. The network page and the
 * enlarged view of a single chart both build this, so the two draw the same
 * marks from the same numbers.
 */
export type SeriesModel = {
  points: ChartPoint[];
  drawn: DrawnRow[];
  markers: ReturnType<typeof feeChartData>["markers"];
  span: number;
  /** The window the range asked for and the spans of it with nothing indexed. */
  gaps: GapModel;
  /** The gas-per-second axis: one unit for every label, shared by the live and the bucketed view. */
  gasAxis: ThroughputAxis;
  segments: Segment[];
  /** True when some point's constraint set is not known, so its backlogs go under the unlabelled slots. */
  unknown: boolean;
  /** True when some point's set is known but its per-constraint split was never recorded. */
  unrecorded: boolean;
  count: number;
  indices: number[];
  hasTargets: boolean;
  note: (row: Record<string, unknown>) => string | null;
  /**
   * The buckets whose compute gas rate has no receipts behind it, and the
   * tooltip note that says so. Whole buckets with a null rate, which neither
   * the gap shading nor the partial hatch speaks for.
   */
  gasMissing: MissingRun[];
  gasNote: (row: Record<string, unknown>) => string | null;
  /** `note` plus the cause for every series with nothing in the bucket: what the bucket inspector reads out. */
  inspectorNote: (row: Record<string, unknown>) => string | null;
  /** The same for one backlog slot: the buckets that recorded no backlog for it, and its note. */
  backlogMissingFor: (index: number) => MissingRun[];
  backlogNoteFor: (index: number) => (row: Record<string, unknown>) => string | null;
  feeRows: TooltipRow[];
  contributionRows: TooltipRow[];
  gasRows: TooltipRow[];
  backlogRows: TooltipRow[];
  backlogRowsFor: (index: number) => TooltipRow[];
  contributionLegend: { label: string; color: string; kind?: "rect" | "line" }[];
  gasLegend: { label: string; color: string; kind?: "rect" | "line" }[];
};

export function buildSeriesModel(series: Series, model: PricerModel): SeriesModel {
  // The charts draw `drawn`, which duplicates each bucket where the set in
  // force changes so a replacement is a vertical edge rather than a slope
  // across the bucket; `points` is one row per bucket, for the table and the
  // inspector. The base fee itself is drawn by the hero, at every range, so
  // it is not here.
  const { points, drawn, markers, span, bucketSeconds, gaps } = feeChartData(series, model);
  const segments = segmentsFor(series, model);
  // Two ways a point ends up in the unknown-split series: its set is not
  // known (backlogs then go under the unlabelled slots too), or its set is
  // known but its per-constraint split was never recorded.
  const unknown = hasUnknownSets(series, model);
  const unrecorded = hasUnrecordedSplit(series);
  const count = seriesCount(series, model);
  const indices = Array.from({ length: count }, (_, i) => i);
  // A boundary partial bucket and a bounded block gap have measured coverage
  // and a compute-gas rate over that span. An unbounded gap has a null chart
  // rate, so it breaks instead of reading as low throughput.
  const note = bucketNote(markers, bucketSeconds);
  // Two more ways a series has nothing to draw in a bucket that is otherwise
  // whole, neither of which the gap tint or the partial hatch speaks for: a
  // bucket aggregates poster gas as null whenever one of its blocks was
  // stored without receipts, which leaves its compute gas rate null at full
  // coverage, and a bucket can record no backlog for a slot. Both used to
  // break the line and say nothing at all.
  const gasPresent: Present = (row) => typeof row.gps === "number";
  const backlogPresent = (i: number): Present => (row) => {
    const own = segments.filter((s) => s.index === i && s.setId === row.constraintSetId);
    // A slot the row's own set never defined is not missing data: the
    // constraint did not exist then, and a panel with nothing to draw over
    // those buckets is the truth rather than a hole in the record.
    if (row.setKnown === true && own.length === 0) return true;
    return own.some((s) => typeof row[s.backlogKey] === "number") || typeof row[unknownBacklogKey(i)] === "number";
  };
  // An api older than the rate sends no compute gas at all, which the chart
  // rows cannot tell from a rate the api reports as null. That is the client
  // meeting an older api rather than a chain whose receipts are missing, so
  // it earns no band: the chart simply has nothing to draw.
  const ratesReported = series.points.some((p) => p.computeGasPerSecond !== undefined);
  // Read from the buckets rather than from `drawn`: the duplicate a set
  // boundary inserts would otherwise count its bucket twice.
  const gasMissing = ratesReported ? missingRuns(points, gasPresent, bucketSeconds, "receipts") : [];
  const backlogMissing = indices.map((i) => missingRuns(points, backlogPresent(i), bucketSeconds, "backlog"));
  const backlogNotes = indices.map((i) => withMissingNote(note, backlogPresent(i), "backlog"));
  // The inspector stands in for every chart at once, and it is how a reader
  // without a pointer reads a bucket, so it carries the cause for each series
  // that has nothing in one.
  const inspected: MissingSeries[] = [...(ratesReported ? [{ present: gasPresent, kind: "receipts" as const }] : []), ...indices.map((i) => ({ present: backlogPresent(i), kind: "backlog" as const }))];
  // The contribution chart needs none of this: a null in one constraint's
  // series means another set is in force, which the neighbouring series
  // draws, and every bucket's x lands either under its own set or under the
  // unknown-split series, so the stack is never silently empty.

  const contributionRows: TooltipRow[] = segments.map((s) => ({
    label: s.label,
    color: s.color,
    kind: "rect",
    value: (r) => known(r[s.key], (v) => v.toFixed(4)),
    when: splitInForce(s),
  }));
  if (unknown) contributionRows.push({ label: UNKNOWN_LABEL, color: UNKNOWN_COLOR, kind: "rect", value: (r) => known(r[UNKNOWN_KEY], (v) => v.toFixed(4)), when: (r) => r.setKnown === false });
  if (unrecorded) contributionRows.push({ label: NULL_SPLIT_LABEL, color: UNKNOWN_COLOR, kind: "rect", value: (r) => known(r[UNKNOWN_KEY], (v) => v.toFixed(4)), when: (r) => r.setKnown === true && r.splitKnown === false });
  contributionRows.push({ label: "x total", value: (r) => Number(r.x).toFixed(4) });

  const gasRows: TooltipRow[] = [{ label: "compute gas per second", color: "var(--series-1)", value: (r) => known(r.gps, (v) => formatGasPerSecond(v)) }];
  indices.forEach((i) => gasRows.push({ label: `target C${i + 1} in force`, color: seriesColor(i), value: (r) => formatGasPerSecond(Number(r[targetKey(i)])), when: (r) => typeof r[targetKey(i)] === "number" }));

  const backlogRowsFor = (i: number): TooltipRow[] => [
    ...segments
      .filter((s) => s.index === i)
      .map((s): TooltipRow => ({
        label: `backlog ${s.label}`,
        color: s.color,
        value: (r) => known(r[s.backlogKey], (v) => formatGas(v)),
        when: (r) => inForce(s)(r) && typeof r[s.backlogKey] === "number",
      })),
    ...(unknown
      ? [
          {
            label: `backlog C${i + 1} (set unknown)`,
            color: UNKNOWN_COLOR,
            value: (r: Record<string, unknown>) => known(r[unknownBacklogKey(i)], (v) => formatGas(v)),
            when: (r: Record<string, unknown>) => r.setKnown === false && typeof r[unknownBacklogKey(i)] === "number",
          },
        ]
      : []),
  ];

  const contributionLegend = [
    ...segments.map((s) => ({ label: s.label, color: s.color })),
    ...(unknown ? [{ label: UNKNOWN_LABEL, color: UNKNOWN_COLOR }] : []),
    ...(unrecorded ? [{ label: NULL_SPLIT_LABEL, color: UNKNOWN_COLOR }] : []),
  ];
  const hasTargets = segments.some((s) => s.constraint !== null);
  // The targets are drawn as thresholds on the same axis as the rate, so the
  // axis has to hold the taller of the two. Its unit is named once, in the
  // legend, because every tick on it is a bare figure.
  const gasAxis = throughputAxis(Math.max(0, ...points.flatMap((p) => (p.gps === null ? [] : [p.gps])), ...points.flatMap((p) => indices.map((i) => p[targetKey(i)] ?? 0))));
  const gasLegend = [{ label: `compute gas per second (${gasAxis.unit})`, color: "var(--series-1)", kind: "line" as const }, ...(hasTargets ? indices.map((i) => ({ label: `target C${i + 1} (stepped, per set)`, color: seriesColor(i), kind: "line" as const })) : [])];

  return {
    points,
    drawn,
    markers,
    span,
    gaps,
    gasAxis,
    segments,
    unknown,
    unrecorded,
    count,
    indices,
    hasTargets,
    note,
    gasMissing,
    gasNote: ratesReported ? withMissingNote(note, gasPresent, "receipts") : note,
    inspectorNote: withMissingNotes(note, inspected),
    backlogMissingFor: (i) => backlogMissing[i] ?? [],
    backlogNoteFor: (i) => backlogNotes[i] ?? note,
    feeRows: feeTooltipRows(),
    contributionRows,
    gasRows,
    backlogRows: indices.flatMap(backlogRowsFor),
    backlogRowsFor,
    contributionLegend,
    gasLegend,
  };
}

/**
 * The time axis every history chart shares, so hovering one lines up with the
 * rest. It spans the window the range asked for, never the extent of the
 * buckets that happen to exist: two hours of history on a 24h range draw over
 * the last twelfth of the axis, which is where they happened.
 */
function timeAxis(span: number, window: GapWindow) {
  return <XAxis dataKey="t" type="number" domain={[window.from, window.to]} tickFormatter={(t: number) => formatTick(t, span)} tickLine={false} axisLine={false} minTickGap={48} />;
}

/** Owner actions as dashed rules, numbered chronologically; the list under the charts decodes them. */
function markerLines(markers: SeriesModel["markers"]) {
  return markers.map((m, i) => (
    <ReferenceLine key={`${m.t}-${m.action.txHash}`} x={m.t} stroke={MARKER_COLOR} strokeWidth={1} strokeDasharray="2 3" label={{ value: String(i + 1), position: "insideTopLeft", fill: "var(--ink-2)", fontSize: 10 }} />
  ));
}

const bucketTitle = (t: number) => formatDateTime(t);

/** What names a bucket in the inspector and the data table: the time it starts at. */
export const bucketRowTitle = (row: Record<string, unknown>) => formatDateTime(Number(row.t));

/** Each constraint's share of the exponent, stacked, one series per constraint set. */
export const ContributionChart = memo(function ContributionChart({ m, height = SERIES_CHART_HEIGHT }: { m: SeriesModel; height?: ChartHeight }) {
  return (
    <>
      <ChartFrame height={height} label="Stacked per-constraint contribution to the exponent, one series per constraint set">
        <ResponsiveContainer width="100%" height="100%">
          <AreaChart data={m.drawn} syncId={SYNC_ID} margin={{ top: 12, right: TIME_AXIS_RIGHT, bottom: 0, left: 0 }}>
            <CartesianGrid vertical={false} />
            {gapBands(m.gaps.gaps, m.gaps.window)}
            {timeAxis(m.span, m.gaps.window)}
            <YAxis tickFormatter={(v: number) => formatSignificant(v, 2)} tickLine={false} axisLine={false} width={48} />
            <Tooltip isAnimationActive={false} content={(props) => <ChartTooltip {...props} title={bucketTitle} rows={m.contributionRows} note={m.note} />} />
            {m.segments.map((s) => (
              <Area key={s.key} type="monotone" dataKey={s.key} stackId="x" connectNulls={false} stroke="var(--chart)" strokeWidth={1} fill={s.color} fillOpacity={0.85} isAnimationActive={false} activeDot={false} />
            ))}
            {m.unknown || m.unrecorded ? (
              <Area type="monotone" dataKey={UNKNOWN_KEY} stackId="x" connectNulls={false} stroke="var(--chart)" strokeWidth={1} fill={UNKNOWN_COLOR} fillOpacity={0.5} isAnimationActive={false} activeDot={false} />
            ) : null}
            {markerLines(m.markers)}
          </AreaChart>
        </ResponsiveContainer>
      </ChartFrame>
      <GapNote gaps={m.gaps} />
    </>
  );
});

/**
 * Gas carried per second against the target of every constraint in force, at
 * whatever size its frame is: the hero draws it under the base fee, its own
 * page draws it large, and both read the same rows and the same axis. The
 * axis carries one unit for the whole scale, named in the caption beside the
 * chart, so every label is a bare figure of the same width.
 */
export const GasPerSecondChart = memo(function GasPerSecondChart({ m, height = SERIES_CHART_HEIGHT, axisWidth = GAS_AXIS_WIDTH, minWidth }: { m: SeriesModel; height?: ChartHeight; axisWidth?: number; minWidth?: number }) {
  return (
    <>
      <ChartFrame height={height} minWidth={minWidth} label={`Compute gas used per second in ${m.gasAxis.unit} with each constraint target in force drawn as a stepped line`}>
        <ResponsiveContainer width="100%" height="100%">
          <ComposedChart data={m.drawn} syncId={SYNC_ID} margin={{ top: 12, right: TIME_AXIS_RIGHT, bottom: 0, left: 0 }}>
            {m.gasMissing.length > 0 ? (
              <defs>
                <MissingDots />
              </defs>
            ) : null}
            <CartesianGrid vertical={false} />
            {gapBands(m.gaps.gaps, m.gaps.window)}
            {missingBandAreas(m.gasMissing, m.gaps.window)}
            {timeAxis(m.span, m.gaps.window)}
            <YAxis domain={[0, m.gasAxis.top]} ticks={m.gasAxis.ticks} tickFormatter={(v: number) => throughputTick(v, m.gasAxis)} tickLine={false} axisLine={false} width={axisWidth} />
            <Tooltip isAnimationActive={false} filterNull={false} content={(props) => <ChartTooltip {...props} title={bucketTitle} rows={m.gasRows} note={m.gasNote} />} />
            <Area type="monotone" dataKey="gps" connectNulls={false} stroke="var(--series-1)" strokeWidth={2} fill="var(--series-1)" fillOpacity={0.1} isAnimationActive={false} activeDot={false} />
            {m.hasTargets ? m.indices.map((i) => <Line key={i} type="stepAfter" dataKey={targetKey(i)} connectNulls={false} stroke={seriesColor(i)} strokeDasharray="4 3" dot={false} isAnimationActive={false} />) : null}
          </ComposedChart>
        </ResponsiveContainer>
      </ChartFrame>
      <GapNote gaps={m.gaps} />
      <MissingNote runs={m.gasMissing} />
    </>
  );
});

/** One slot's backlog over time, on its own scale. A replaced constraint starts a new series. */
export const BacklogChart = memo(function BacklogChart({ m, index, label, height = BACKLOG_CHART_HEIGHT }: { m: SeriesModel; index: number; label: string; height?: ChartHeight }) {
  return (
    <>
      <ChartFrame height={height} minWidth={260} label={`Backlog of ${label} over time`}>
        <ResponsiveContainer width="100%" height="100%">
          <AreaChart data={m.drawn} syncId={SYNC_ID} margin={{ top: 8, right: TIME_AXIS_RIGHT, bottom: 0, left: 0 }}>
            {m.backlogMissingFor(index).length > 0 ? (
              <defs>
                <MissingDots />
              </defs>
            ) : null}
            <CartesianGrid vertical={false} />
            {gapBands(m.gaps.gaps, m.gaps.window)}
            {missingBandAreas(m.backlogMissingFor(index), m.gaps.window)}
            {timeAxis(m.span, m.gaps.window)}
            <YAxis tickFormatter={(v: number) => unbroken(formatGas(v))} tickLine={false} axisLine={false} width={GAS_AXIS_WIDTH} />
            <Tooltip isAnimationActive={false} filterNull={false} content={(props) => <ChartTooltip {...props} title={bucketTitle} rows={m.backlogRowsFor(index)} note={m.backlogNoteFor(index)} />} />
            {m.segments
              .filter((s) => s.index === index)
              .map((s) => (
                <Area key={s.backlogKey} type="monotone" dataKey={s.backlogKey} connectNulls={false} stroke={s.color} strokeWidth={2} fill={s.color} fillOpacity={0.1} isAnimationActive={false} activeDot={false} />
              ))}
            {m.unknown ? (
              <Area type="monotone" dataKey={unknownBacklogKey(index)} connectNulls={false} stroke={UNKNOWN_COLOR} strokeWidth={2} strokeDasharray="4 3" fill={UNKNOWN_COLOR} fillOpacity={0.1} isAnimationActive={false} activeDot={false} />
            ) : null}
          </AreaChart>
        </ResponsiveContainer>
      </ChartFrame>
      <GapNote gaps={m.gaps} />
      <MissingNote runs={m.backlogMissingFor(index)} />
    </>
  );
});

/** The card a history chart sits in: its name, its legend and the control that enlarges it. */
function ChartBlock({ title, legend, action, children }: { title: string; legend?: { label: string; color: string; kind?: "rect" | "line" }[]; action?: ReactNode; children: ReactNode }) {
  return (
    <div className="vw-card p-3">
      <div className="mb-2 flex flex-wrap items-center justify-between gap-x-4 gap-y-1">
        <h3 className="text-sm font-semibold text-ink">{title}</h3>
        <div className="flex items-center gap-3">
          {legend && legend.length > 1 ? <Legend items={legend} /> : null}
          {action}
        </div>
      </div>
      {children}
    </div>
  );
}

/**
 * History charts. `model` is the network's pricer (from the api's Network or
 * the live snapshot): the series carries none of its own, and an empty
 * constraint-set list means "no set known", never "legacy". Every card links
 * to the chart's own page at the range on screen.
 */
export const SeriesCharts = memo(function SeriesCharts({ network, range, series, loading, model }: { network: string; range: SeriesRange; series: Series | null; loading: boolean; model: PricerModel }) {
  const m = useMemo(() => (series ? buildSeriesModel(series, model) : null), [series, model]);
  const [tableOpen, setTableOpen] = useState(false);

  if (!series || m === null) {
    return (
      <div className="vw-card p-6 text-sm text-ink-2" aria-busy={loading}>
        {loading ? "Loading history." : "No history yet."}
      </div>
    );
  }
  if (m.points.length === 0) {
    return <div className="vw-card p-6 text-sm text-ink-2">{emptyRangeNote(m.gaps.first)}</div>;
  }

  return (
    <div className="flex flex-col gap-4" style={{ opacity: loading ? 0.7 : 1, transition: "opacity 200ms" }}>
      <ChartBlock
        title="Contribution to x per constraint"
        legend={m.contributionLegend}
        action={<EnlargeLink network={network} view={chartView("contribution")} range={range} />}
      >
        <ContributionChart m={m} />
      </ChartBlock>

      <div className="vw-card p-3">
        <div className="mb-2 flex flex-wrap items-baseline justify-between gap-x-4 gap-y-1">
          <h3 className="text-sm font-semibold text-ink">Backlog per constraint (gas)</h3>
          <span className="text-xs text-ink-3">one panel per slot, each on its own scale; a replaced constraint starts a new series</span>
        </div>
        <div className={`grid gap-3 ${m.count > 2 ? "md:grid-cols-3" : "md:grid-cols-2"}`}>
          {m.indices.map((i) => (
            <div key={i}>
              <div className="mb-1 flex items-center gap-1.5 text-xs text-ink-2">
                <span className="inline-block h-2.5 w-2.5 rounded-[2px]" style={{ background: seriesColor(i) }} aria-hidden="true" />
                <span className="min-w-0">{slotLabel(series, i, model)}</span>
                <EnlargeLink
                  network={network}
                  view={chartView("backlogs")}
                  range={range}
                  constraint={i}
                  size="hero"
                  label={`the ${slotLabel(series, i, model)} backlog`}
                  className="ml-auto"
                />
              </div>
              <BacklogChart m={m} index={i} label={slotLabel(series, i, model)} />
            </div>
          ))}
        </div>
      </div>

      {m.markers.length > 0 ? (
        <ol className="flex flex-wrap gap-x-5 gap-y-1 text-xs text-ink-2">
          {m.markers.map((mk, i) => (
            <li key={`${mk.t}-${mk.action.txHash}`}>
              <span className="num mr-1 rounded bg-surface-2 px-1 text-ink">{i + 1}</span>
              <span className="num text-ink">{formatDateTime(mk.t)}</span> · {describeAction(mk.action)}
            </li>
          ))}
        </ol>
      ) : null}

      <PointInspector
        points={m.points}
        title={bucketRowTitle}
        selectLabel="Select a bucket to read its values"
        groups={[
          { title: "fee", rows: m.feeRows },
          { title: "split", rows: m.contributionRows },
          { title: "gas", rows: m.gasRows },
          { title: "backlog", rows: m.backlogRows },
        ]}
        note={m.inspectorNote}
      />

      <details className="text-xs text-ink-2" onToggle={(e) => setTableOpen((e.currentTarget as HTMLDetailsElement).open)}>
        <summary className="cursor-pointer select-none">Data table ({formatInteger(m.points.length)} buckets, every value the charts draw)</summary>
        {tableOpen ? (
          <div className="mt-2 max-h-[480px] overflow-auto">
            <table className="num w-full min-w-[960px] text-left">
              <caption className="sr-only">History buckets with base fee, floor, exponent split, compute gas per second, backlogs and fee destinations</caption>
              <thead className="sticky top-0 bg-surface text-ink-3">
                <tr>
                  <th scope="col" className="py-1 pr-3 font-medium">bucket</th>
                  <th scope="col" className="py-1 pr-3 font-medium">fee avg (gwei)</th>
                  <th scope="col" className="py-1 pr-3 font-medium">min</th>
                  <th scope="col" className="py-1 pr-3 font-medium">max</th>
                  <th scope="col" className="py-1 pr-3 font-medium">floor</th>
                  <th scope="col" className="py-1 pr-3 font-medium">x</th>
                  <th scope="col" className="py-1 pr-3 font-medium">split (set)</th>
                  <th scope="col" className="py-1 pr-3 font-medium">compute gas/s</th>
                  {m.indices.map((i) => (
                    <th key={i} scope="col" className="py-1 pr-3 font-medium">
                      backlog C{i + 1}
                    </th>
                  ))}
                  <th scope="col" className="py-1 pr-3 font-medium">fees (ETH)</th>
                  <th scope="col" className="py-1 pr-3 font-medium">floor fees</th>
                  <th scope="col" className="py-1 pr-3 font-medium">surplus fees</th>
                  <th scope="col" className="py-1 pr-3 font-medium">poster fees</th>
                  <th scope="col" className="py-1 pr-3 font-medium">blocks</th>
                </tr>
              </thead>
              <tbody>
                {m.points.map((p: ChartPoint) => (
                  <tr key={p.t} className="border-t border-hairline">
                    <th scope="row" className="py-1 pr-3 font-normal">{formatDateTime(p.t)}</th>
                    <td className="py-1 pr-3">{formatSignificant(p.feeAvg, 4)}</td>
                    <td className="py-1 pr-3">{formatSignificant(p.feeMin, 3)}</td>
                    <td className="py-1 pr-3">{formatSignificant(p.feeMax, 3)}</td>
                    <td className="py-1 pr-3">{formatFloor(p.floor)}</td>
                    <td className="py-1 pr-3">{p.x.toFixed(4)}</td>
                    <td className="py-1 pr-3">
                      {describeSplit(p, m.segments)} ({p.setKnown ? `set ${p.constraintSetId}` : "unknown set"})
                    </td>
                    <td className="py-1 pr-3">{p.gps === null ? "n/a" : formatGasPerSecond(p.gps)}</td>
                    {m.indices.map((i) => {
                      const s = p.setKnown ? m.segments.find((seg) => seg.setId === p.constraintSetId && seg.index === i) : undefined;
                      const v = s ? p[s.backlogKey] : p.setKnown ? null : p[unknownBacklogKey(i)];
                      return (
                        <td key={i} className="py-1 pr-3">
                          {typeof v === "number" ? formatGas(v) : "n/a"}
                        </td>
                      );
                    })}
                    <td className="py-1 pr-3">{formatSignificant(p.feesEth, 4)}</td>
                    <td className="py-1 pr-3">{formatFeePart(p.floorFeesEth)}</td>
                    <td className="py-1 pr-3">{formatFeePart(p.surplusFeesEth)}</td>
                    <td className="py-1 pr-3">{formatFeePart(p.posterFeesEth)}</td>
                    <td className="py-1 pr-3">{formatInteger(p.blocks)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        ) : null}
      </details>
    </div>
  );
});
