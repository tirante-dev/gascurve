"use client";

import { useMemo, useState } from "react";
import { Area, AreaChart, CartesianGrid, ComposedChart, Line, ReferenceLine, ResponsiveContainer, Tooltip, XAxis, YAxis } from "recharts";
import type { OwnerAction, PricerModel, Series } from "@/types";
import {
  buildChartPoints,
  FLOOR_COLOR,
  hasUnknownSets,
  hasUnrecordedSplit,
  logDomain,
  MARKER_COLOR,
  NULL_SPLIT_LABEL,
  segmentsFor,
  seriesColor,
  seriesCount,
  shortConstraintLabel,
  slotLabel,
  spanSeconds,
  targetKey,
  UNKNOWN_COLOR,
  UNKNOWN_KEY,
  UNKNOWN_LABEL,
  unknownBacklogKey,
  withSetBoundaries,
  type ChartPoint,
  type Segment,
} from "@/utils/chart";
import { formatDateTime, formatGas, formatGwei, formatInteger, formatSignificant, formatTick } from "@/utils/format";
import { ChartTooltip, applicableRows, type TooltipRow } from "./ChartTooltip";
import { ChartFrame, Legend } from "./primitives";

const SYNC_ID = "history";

type Marker = { t: number; action: OwnerAction; label: string };

function markersFor(series: Series): Marker[] {
  return series.ownerActions.map((a) => ({ t: Math.floor(new Date(a.at).getTime() / 1000), action: a, label: a.method }));
}

/** Owner actions that fall inside the bucket that starts at `t`. */
function actionsInBucket(markers: Marker[], t: number, bucketSeconds: number): Marker[] {
  return markers.filter((m) => m.t >= t && m.t < t + bucketSeconds);
}

function describeAction(a: OwnerAction): string {
  if (a.method === "setGasPricingConstraints") {
    const raw = a.args.constraints;
    if (Array.isArray(raw)) {
      const parts = raw.map((c) => (Array.isArray(c) && c.length >= 2 ? shortConstraintLabel({ target: Number(c[0]), window: Number(c[1]) }) : String(c)));
      return `setGasPricingConstraints: ${parts.join(", ")}`;
    }
  }
  if (a.method === "setMinimumL2BaseFee" && typeof a.args.priceInWei === "string") {
    return `setMinimumL2BaseFee: ${formatGwei(a.args.priceInWei)} gwei`;
  }
  return a.method;
}

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

/** "0.1234" for a known fee part, "n/a" for one that predates the fee split. */
function formatFeePart(eth: number | null): string {
  return eth === null ? "n/a" : formatSignificant(eth, 4);
}

function ChartBlock({ title, legend, children, height, label }: { title: string; legend?: { label: string; color: string; kind?: "rect" | "line" }[]; children: React.ReactNode; height: number; label: string }) {
  return (
    <div className="vw-card p-3">
      <div className="mb-2 flex flex-wrap items-baseline justify-between gap-x-4 gap-y-1">
        <h3 className="text-sm font-semibold text-ink">{title}</h3>
        {legend && legend.length > 1 ? <Legend items={legend} /> : null}
      </div>
      <ChartFrame height={height} label={label}>
        {children}
      </ChartFrame>
    </div>
  );
}

/**
 * Keyboard access to every point: a slider picks a bucket and the same rows
 * the tooltips show are read out in a live region.
 */
function PointInspector({ points, groups, note }: { points: ChartPoint[]; groups: { title: string; rows: TooltipRow[] }[]; note: (row: Record<string, unknown>) => string | null }) {
  const [index, setIndex] = useState(points.length - 1);
  const clamped = Math.max(0, Math.min(points.length - 1, index));
  const row = points[clamped] as unknown as Record<string, unknown>;
  const extra = note(row);
  return (
    <div className="vw-card p-3 text-xs text-ink-2">
      <label className="flex flex-wrap items-center gap-3">
        <span className="font-semibold text-ink">Point inspector</span>
        <input type="range" min={0} max={points.length - 1} value={clamped} onChange={(e) => setIndex(Number(e.target.value))} aria-label="Select a bucket to read its values" className="min-w-[160px] flex-1" />
        <span className="num text-ink">{formatDateTime(points[clamped].t)}</span>
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
 * History charts. `model` is the network's pricer (from the api's Network or
 * the live snapshot): the series carries none of its own, and an empty
 * constraint-set list means "no set known", never "legacy".
 */
export function SeriesCharts({ series, loading, model }: { series: Series | null; loading: boolean; model: PricerModel }) {
  // One row per bucket for the table and the inspector; the charts draw
  // `drawn`, which duplicates each bucket where the set in force changes so a
  // replacement is a vertical edge rather than a slope across the bucket.
  const points = useMemo(() => (series ? buildChartPoints(series, model) : []), [series, model]);
  const drawn = useMemo(() => withSetBoundaries(points), [points]);
  const markers = useMemo(() => (series ? markersFor(series) : []), [series]);
  const segments = useMemo(() => (series ? segmentsFor(series, model) : []), [series, model]);
  // Two ways a point ends up in the unknown-split series: its set is not
  // known (backlogs then go under the unlabelled slots too), or its set is
  // known but its per-constraint split was never recorded.
  const unknown = series ? hasUnknownSets(series, model) : false;
  const unrecorded = series ? hasUnrecordedSplit(series) : false;
  const unknownSplit = unknown || unrecorded;
  const count = series ? seriesCount(series) : 0;
  const span = spanSeconds(points);
  const bucketSeconds = points.length > 1 ? points[1].t - points[0].t : 60;
  const feeDomain = useMemo(() => logDomain(points.flatMap((p) => [p.feeMin, p.feeMax, p.floor])), [points]);
  const indices = Array.from({ length: count }, (_, i) => i);
  const [tableOpen, setTableOpen] = useState(false);
  const tickFormatter = (t: number) => formatTick(t, span);
  const title = (t: number) => formatDateTime(t);
  const note = (row: Record<string, unknown>) => {
    const hits = actionsInBucket(markers, Number(row.t), bucketSeconds);
    return hits.length > 0 ? hits.map((h) => `Owner action at block ${formatInteger(h.action.block)}: ${describeAction(h.action)}`).join(" · ") : null;
  };

  if (!series) {
    return (
      <div className="vw-card p-6 text-sm text-ink-2" aria-busy={loading}>
        {loading ? "Loading history." : "No history yet."}
      </div>
    );
  }
  if (points.length === 0) {
    return <div className="vw-card p-6 text-sm text-ink-2">No buckets in this range yet.</div>;
  }

  const feeRows: TooltipRow[] = [
    { label: "base fee, average", color: "var(--series-1)", value: (r) => `${formatSignificant(Number(r.feeAvg), 4)} gwei` },
    { label: "min to max in bucket", value: (r) => `${formatSignificant(Number(r.feeMin), 3)} to ${formatSignificant(Number(r.feeMax), 3)} gwei` },
    { label: "floor in force", color: FLOOR_COLOR, value: (r) => `${formatSignificant(Number(r.floor), 3)} gwei` },
    { label: "x", value: (r) => Number(r.x).toFixed(4) },
    { label: "blocks", value: (r) => formatInteger(Number(r.blocks)) },
  ];
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
  const gasRows: TooltipRow[] = [{ label: "gas per second", color: "var(--series-1)", value: (r) => `${formatGas(Number(r.gps))} gas/s` }];
  indices.forEach((i) => gasRows.push({ label: `target C${i + 1} in force`, color: seriesColor(i), value: (r) => `${formatGas(Number(r[targetKey(i)]))} gas/s`, when: (r) => typeof r[targetKey(i)] === "number" }));
  const backlogRowsFor = (i: number): TooltipRow[] => [
    ...segments
      .filter((s) => s.index === i)
      .map((s): TooltipRow => ({
        label: `backlog ${s.label}`,
        color: s.color,
        value: (r) => known(r[s.backlogKey], (v) => `${formatGas(v)} gas`),
        when: (r) => inForce(s)(r) && typeof r[s.backlogKey] === "number",
      })),
    ...(unknown
      ? [
          {
            label: `backlog C${i + 1} (set unknown)`,
            color: UNKNOWN_COLOR,
            value: (r: Record<string, unknown>) => known(r[unknownBacklogKey(i)], (v) => `${formatGas(v)} gas`),
            when: (r: Record<string, unknown>) => r.setKnown === false && typeof r[unknownBacklogKey(i)] === "number",
          },
        ]
      : []),
  ];
  const backlogRows: TooltipRow[] = indices.flatMap(backlogRowsFor);
  const contributionLegend = [
    ...segments.map((s) => ({ label: s.label, color: s.color })),
    ...(unknown ? [{ label: UNKNOWN_LABEL, color: UNKNOWN_COLOR }] : []),
    ...(unrecorded ? [{ label: NULL_SPLIT_LABEL, color: UNKNOWN_COLOR }] : []),
  ];
  const hasTargets = segments.some((s) => s.constraint !== null);
  const gasLegend = [{ label: "gas/s", color: "var(--series-1)", kind: "line" as const }, ...(hasTargets ? indices.map((i) => ({ label: `target C${i + 1} (stepped, per set)`, color: seriesColor(i), kind: "line" as const })) : [])];
  // Markers carry their chronological number; the list under the charts decodes them.
  const markerLines = markers.map((m, i) => (
    <ReferenceLine key={`${m.t}-${m.action.txHash}`} x={m.t} stroke={MARKER_COLOR} strokeWidth={1} strokeDasharray="2 3" label={{ value: String(i + 1), position: "insideTopLeft", fill: "var(--ink-2)", fontSize: 10 }} />
  ));

  const xAxis = <XAxis dataKey="t" type="number" domain={["dataMin", "dataMax"]} tickFormatter={tickFormatter} tickLine={false} axisLine={false} minTickGap={48} />;

  return (
    <div className="flex flex-col gap-4" style={{ opacity: loading ? 0.7 : 1, transition: "opacity 200ms" }}>
      <ChartBlock title="Base fee (gwei, log scale)" legend={[{ label: "base fee", color: "var(--series-1)", kind: "line" }, { label: "floor in force (stepped)", color: FLOOR_COLOR, kind: "line" }]} height={260} label="Base fee over time on a log scale with the floor in force drawn as a stepped line">
        <ResponsiveContainer width="100%" height="100%">
          <ComposedChart data={drawn} syncId={SYNC_ID} margin={{ top: 12, right: 12, bottom: 0, left: 0 }}>
            <CartesianGrid vertical={false} />
            {xAxis}
            <YAxis scale="log" domain={feeDomain} tickFormatter={(v: number) => formatSignificant(v, 2)} tickLine={false} axisLine={false} width={48} />
            <Tooltip isAnimationActive={false} content={(props) => <ChartTooltip {...props} title={title} rows={feeRows} note={note} />} />
            <Area type="monotone" dataKey="feeMax" stroke="none" fill="var(--series-1)" fillOpacity={0.12} isAnimationActive={false} activeDot={false} />
            <Area type="monotone" dataKey="feeMin" stroke="none" fill="var(--chart)" fillOpacity={1} isAnimationActive={false} activeDot={false} />
            <Line type="monotone" dataKey="feeAvg" stroke="var(--series-1)" strokeWidth={2} dot={false} isAnimationActive={false} />
            <Line type="stepAfter" dataKey="floor" stroke={FLOOR_COLOR} strokeWidth={1.5} strokeDasharray="4 3" dot={false} isAnimationActive={false} />
            {markerLines}
          </ComposedChart>
        </ResponsiveContainer>
      </ChartBlock>

      <ChartBlock title="Contribution to x per constraint" legend={contributionLegend} height={220} label="Stacked per-constraint contribution to the exponent, one series per constraint set">
        <ResponsiveContainer width="100%" height="100%">
          <AreaChart data={drawn} syncId={SYNC_ID} margin={{ top: 12, right: 12, bottom: 0, left: 0 }}>
            <CartesianGrid vertical={false} />
            {xAxis}
            <YAxis tickFormatter={(v: number) => formatSignificant(v, 2)} tickLine={false} axisLine={false} width={48} />
            <Tooltip isAnimationActive={false} content={(props) => <ChartTooltip {...props} title={title} rows={contributionRows} note={note} />} />
            {segments.map((s) => (
              <Area key={s.key} type="monotone" dataKey={s.key} stackId="x" connectNulls={false} stroke="var(--chart)" strokeWidth={1} fill={s.color} fillOpacity={0.85} isAnimationActive={false} activeDot={false} />
            ))}
            {unknownSplit ? <Area type="monotone" dataKey={UNKNOWN_KEY} stackId="x" connectNulls={false} stroke="var(--chart)" strokeWidth={1} fill={UNKNOWN_COLOR} fillOpacity={0.5} isAnimationActive={false} activeDot={false} /> : null}
            {markerLines}
          </AreaChart>
        </ResponsiveContainer>
      </ChartBlock>

      <ChartBlock title="Gas per second against each target" legend={gasLegend} height={220} label="Gas used per second with each constraint target in force drawn as a stepped line">
        <ResponsiveContainer width="100%" height="100%">
          <ComposedChart data={drawn} syncId={SYNC_ID} margin={{ top: 12, right: 12, bottom: 0, left: 0 }}>
            <CartesianGrid vertical={false} />
            {xAxis}
            <YAxis tickFormatter={(v: number) => formatGas(v)} tickLine={false} axisLine={false} width={48} />
            <Tooltip isAnimationActive={false} content={(props) => <ChartTooltip {...props} title={title} rows={gasRows} note={note} />} />
            <Area type="monotone" dataKey="gps" stroke="var(--series-1)" strokeWidth={2} fill="var(--series-1)" fillOpacity={0.1} isAnimationActive={false} activeDot={false} />
            {hasTargets ? indices.map((i) => <Line key={i} type="stepAfter" dataKey={targetKey(i)} stroke={seriesColor(i)} strokeDasharray="4 3" dot={false} isAnimationActive={false} />) : null}
          </ComposedChart>
        </ResponsiveContainer>
      </ChartBlock>

      <div className="vw-card p-3">
        <div className="mb-2 flex flex-wrap items-baseline justify-between gap-x-4 gap-y-1">
          <h3 className="text-sm font-semibold text-ink">Backlog per constraint (gas)</h3>
          <span className="text-xs text-ink-3">one panel per slot, each on its own scale; a replaced constraint starts a new series</span>
        </div>
        <div className={`grid gap-3 ${count > 2 ? "md:grid-cols-3" : "md:grid-cols-2"}`}>
          {indices.map((i) => (
            <div key={i}>
              <div className="mb-1 flex items-center gap-1.5 text-xs text-ink-2">
                <span className="inline-block h-2.5 w-2.5 rounded-[2px]" style={{ background: seriesColor(i) }} aria-hidden="true" />
                {slotLabel(series, i, model)}
              </div>
              <ChartFrame height={140} minWidth={260} label={`Backlog of ${slotLabel(series, i, model)} over time`}>
                <ResponsiveContainer width="100%" height="100%">
                  <AreaChart data={drawn} syncId={SYNC_ID} margin={{ top: 8, right: 12, bottom: 0, left: 0 }}>
                    <CartesianGrid vertical={false} />
                    {xAxis}
                    <YAxis tickFormatter={(v: number) => formatGas(v)} tickLine={false} axisLine={false} width={48} />
                    <Tooltip isAnimationActive={false} content={(props) => <ChartTooltip {...props} title={title} rows={backlogRowsFor(i)} note={note} />} />
                    {segments
                      .filter((s) => s.index === i)
                      .map((s) => (
                        <Area key={s.backlogKey} type="monotone" dataKey={s.backlogKey} connectNulls={false} stroke={s.color} strokeWidth={2} fill={s.color} fillOpacity={0.1} isAnimationActive={false} activeDot={false} />
                      ))}
                    {unknown ? <Area type="monotone" dataKey={unknownBacklogKey(i)} connectNulls={false} stroke={UNKNOWN_COLOR} strokeWidth={2} strokeDasharray="4 3" fill={UNKNOWN_COLOR} fillOpacity={0.1} isAnimationActive={false} activeDot={false} /> : null}
                  </AreaChart>
                </ResponsiveContainer>
              </ChartFrame>
            </div>
          ))}
        </div>
      </div>

      {markers.length > 0 ? (
        <ol className="flex flex-wrap gap-x-5 gap-y-1 text-xs text-ink-2">
          {markers.map((m, i) => (
            <li key={`${m.t}-${m.action.txHash}`}>
              <span className="num mr-1 rounded bg-surface-2 px-1 text-ink">{i + 1}</span>
              <span className="num text-ink">{formatDateTime(m.t)}</span> · {describeAction(m.action)}
            </li>
          ))}
        </ol>
      ) : null}

      <PointInspector
        points={points}
        groups={[
          { title: "fee", rows: feeRows },
          { title: "split", rows: contributionRows },
          { title: "gas", rows: gasRows },
          { title: "backlog", rows: backlogRows },
        ]}
        note={note}
      />

      <details className="text-xs text-ink-2" onToggle={(e) => setTableOpen((e.currentTarget as HTMLDetailsElement).open)}>
        <summary className="cursor-pointer select-none">Data table ({formatInteger(points.length)} buckets, every value the charts draw)</summary>
        {tableOpen ? (
          <div className="mt-2 max-h-[480px] overflow-auto">
            <table className="num w-full min-w-[960px] text-left">
              <caption className="sr-only">History buckets with base fee, floor, exponent split, gas per second, backlogs and fee destinations</caption>
              <thead className="sticky top-0 bg-surface text-ink-3">
                <tr>
                  <th scope="col" className="py-1 pr-3 font-medium">bucket</th>
                  <th scope="col" className="py-1 pr-3 font-medium">fee avg (gwei)</th>
                  <th scope="col" className="py-1 pr-3 font-medium">min</th>
                  <th scope="col" className="py-1 pr-3 font-medium">max</th>
                  <th scope="col" className="py-1 pr-3 font-medium">floor</th>
                  <th scope="col" className="py-1 pr-3 font-medium">x</th>
                  <th scope="col" className="py-1 pr-3 font-medium">split (set)</th>
                  <th scope="col" className="py-1 pr-3 font-medium">gas/s</th>
                  {indices.map((i) => (
                    <th key={i} scope="col" className="py-1 pr-3 font-medium">
                      backlog C{i + 1}
                    </th>
                  ))}
                  <th scope="col" className="py-1 pr-3 font-medium">fees (ETH)</th>
                  <th scope="col" className="py-1 pr-3 font-medium">floor fees</th>
                  <th scope="col" className="py-1 pr-3 font-medium">surplus fees</th>
                  <th scope="col" className="py-1 pr-3 font-medium">blocks</th>
                </tr>
              </thead>
              <tbody>
                {points.map((p: ChartPoint) => (
                  <tr key={p.t} className="border-t border-hairline">
                    <th scope="row" className="py-1 pr-3 font-normal">{formatDateTime(p.t)}</th>
                    <td className="py-1 pr-3">{formatSignificant(p.feeAvg, 4)}</td>
                    <td className="py-1 pr-3">{formatSignificant(p.feeMin, 3)}</td>
                    <td className="py-1 pr-3">{formatSignificant(p.feeMax, 3)}</td>
                    <td className="py-1 pr-3">{formatSignificant(p.floor, 3)}</td>
                    <td className="py-1 pr-3">{p.x.toFixed(4)}</td>
                    <td className="py-1 pr-3">
                      {describeSplit(p, segments)} ({p.setKnown ? `set ${p.constraintSetId}` : "unknown set"})
                    </td>
                    <td className="py-1 pr-3">{formatGas(p.gps)}</td>
                    {indices.map((i) => {
                      const s = p.setKnown ? segments.find((seg) => seg.setId === p.constraintSetId && seg.index === i) : undefined;
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
}
