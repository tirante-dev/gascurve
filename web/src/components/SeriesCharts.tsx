"use client";

import { useMemo } from "react";
import { Area, AreaChart, CartesianGrid, ComposedChart, Line, ReferenceLine, ResponsiveContainer, Tooltip, XAxis, YAxis } from "recharts";
import type { OwnerAction, Series } from "@/types";
import { buildChartPoints, latestSet, logDomain, seriesColor, seriesCount, seriesLabel, shortConstraintLabel, spanSeconds, type ChartPoint } from "@/utils/chart";
import { formatDateTime, formatGas, formatGwei, formatInteger, formatSignificant, formatTick, weiToGweiNumber } from "@/utils/format";
import { ChartTooltip, type TooltipRow } from "./ChartTooltip";
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

function ChartBlock({ title, legend, children, height, label }: { title: string; legend?: { label: string; color: string; kind?: "rect" | "line" }[]; children: React.ReactNode; height: number; label: string }) {
  return (
    <div className="rounded-md border border-hairline bg-surface p-3">
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

export function SeriesCharts({ series, floorWei, loading }: { series: Series | null; floorWei: string | null; loading: boolean }) {
  const points = useMemo(() => (series ? buildChartPoints(series) : []), [series]);
  const markers = useMemo(() => (series ? markersFor(series) : []), [series]);
  const count = series ? seriesCount(series) : 0;
  const set = series ? latestSet(series) : undefined;
  const span = spanSeconds(points);
  const bucketSeconds = points.length > 1 ? points[1].t - points[0].t : 60;
  const floor = floorWei ? weiToGweiNumber(floorWei) : undefined;
  const feeDomain = useMemo(() => logDomain(points.flatMap((p) => [p.feeMin, p.feeMax]), floor), [points, floor]);
  const indices = Array.from({ length: count }, (_, i) => i);
  const constraintLegend = series ? indices.map((i) => ({ label: seriesLabel(series, i), color: seriesColor(i) })) : [];
  const tickFormatter = (t: number) => formatTick(t, span);
  const title = (t: number) => formatDateTime(t);
  const note = (row: Record<string, unknown>) => {
    const hits = actionsInBucket(markers, Number(row.t), bucketSeconds);
    return hits.length > 0 ? hits.map((h) => `Owner action at block ${formatInteger(h.action.block)}: ${describeAction(h.action)}`).join(" · ") : null;
  };

  if (!series) {
    return (
      <div className="rounded-md border border-hairline bg-surface p-6 text-sm text-ink-2" aria-busy={loading}>
        {loading ? "Loading history." : "No history yet."}
      </div>
    );
  }
  if (points.length === 0) {
    return <div className="rounded-md border border-hairline bg-surface p-6 text-sm text-ink-2">No buckets in this range yet.</div>;
  }

  const feeRows: TooltipRow[] = [
    { label: "base fee, average", color: "var(--series-1)", value: (r) => `${formatSignificant(Number(r.feeAvg), 4)} gwei` },
    { label: "min to max in bucket", value: (r) => `${formatSignificant(Number(r.feeMin), 3)} to ${formatSignificant(Number(r.feeMax), 3)} gwei` },
    { label: "x", value: (r) => Number(r.x).toFixed(4) },
    { label: "blocks", value: (r) => formatInteger(Number(r.blocks)) },
  ];
  const contributionRows: TooltipRow[] = indices.map((i) => ({
    label: seriesLabel(series, i),
    color: seriesColor(i),
    kind: "rect",
    value: (r) => Number(r[`c${i}`] ?? 0).toFixed(4),
  }));
  contributionRows.push({ label: "x total", value: (r) => Number(r.x).toFixed(4) });
  const gasRows: TooltipRow[] = [{ label: "gas per second", color: "var(--series-1)", value: (r) => `${formatGas(Number(r.gps))} gas/s` }];
  if (set) {
    set.constraints.forEach((c, i) => gasRows.push({ label: `target C${i + 1}`, color: seriesColor(i), value: () => `${formatGas(c.target)} gas/s` }));
  }
  // Markers carry their chronological number; the list under the charts decodes them.
  const markerLines = markers.map((m, i) => (
    <ReferenceLine key={`${m.t}-${m.action.txHash}`} x={m.t} stroke="var(--ink-2)" strokeWidth={1} label={{ value: String(i + 1), position: "insideTopLeft", fill: "var(--ink-2)", fontSize: 10 }} />
  ));

  const xAxis = <XAxis dataKey="t" type="number" domain={["dataMin", "dataMax"]} tickFormatter={tickFormatter} tickLine={false} axisLine={false} minTickGap={48} />;

  return (
    <div className="flex flex-col gap-4" style={{ opacity: loading ? 0.7 : 1, transition: "opacity 200ms" }}>
      <ChartBlock title="Base fee (gwei, log scale)" height={260} label="Base fee over time on a log scale with the floor marked">
        <ResponsiveContainer width="100%" height="100%">
          <ComposedChart data={points} syncId={SYNC_ID} margin={{ top: 12, right: 12, bottom: 0, left: 0 }}>
            <CartesianGrid vertical={false} />
            {xAxis}
            <YAxis scale="log" domain={feeDomain} tickFormatter={(v: number) => formatSignificant(v, 2)} tickLine={false} axisLine={false} width={48} />
            <Tooltip isAnimationActive={false} content={(props) => <ChartTooltip {...props} title={title} rows={feeRows} note={note} />} />
            <Area type="monotone" dataKey="feeMax" stroke="none" fill="var(--series-1)" fillOpacity={0.12} isAnimationActive={false} activeDot={false} />
            <Area type="monotone" dataKey="feeMin" stroke="none" fill="var(--surface)" fillOpacity={1} isAnimationActive={false} activeDot={false} />
            <Line type="monotone" dataKey="feeAvg" stroke="var(--series-1)" strokeWidth={2} dot={false} isAnimationActive={false} />
            {floor ? <ReferenceLine y={floor} stroke="var(--ink-3)" label={{ value: `floor ${formatSignificant(floor, 2)} gwei`, position: "insideBottomRight", fill: "var(--ink-3)", fontSize: 10 }} /> : null}
            {markerLines}
          </ComposedChart>
        </ResponsiveContainer>
      </ChartBlock>

      <ChartBlock title="Contribution to x per constraint" legend={constraintLegend} height={220} label="Stacked per-constraint contribution to the exponent">
        <ResponsiveContainer width="100%" height="100%">
          <AreaChart data={points} syncId={SYNC_ID} margin={{ top: 12, right: 12, bottom: 0, left: 0 }}>
            <CartesianGrid vertical={false} />
            {xAxis}
            <YAxis tickFormatter={(v: number) => formatSignificant(v, 2)} tickLine={false} axisLine={false} width={48} />
            <Tooltip isAnimationActive={false} content={(props) => <ChartTooltip {...props} title={title} rows={contributionRows} note={note} />} />
            {indices.map((i) => (
              <Area key={i} type="monotone" dataKey={`c${i}`} stackId="x" stroke="var(--surface)" strokeWidth={1} fill={seriesColor(i)} fillOpacity={0.85} isAnimationActive={false} activeDot={false} />
            ))}
            {markerLines}
          </AreaChart>
        </ResponsiveContainer>
      </ChartBlock>

      <ChartBlock
        title="Gas per second against each target"
        legend={[{ label: "gas/s", color: "var(--series-1)", kind: "line" }, ...(set ? set.constraints.map((c, i) => ({ label: `target C${i + 1} ${formatGas(c.target)}`, color: seriesColor(i), kind: "line" as const })) : [])]}
        height={220}
        label="Gas used per second with each constraint target as a horizontal line"
      >
        <ResponsiveContainer width="100%" height="100%">
          <ComposedChart data={points} syncId={SYNC_ID} margin={{ top: 12, right: 12, bottom: 0, left: 0 }}>
            <CartesianGrid vertical={false} />
            {xAxis}
            <YAxis tickFormatter={(v: number) => formatGas(v)} tickLine={false} axisLine={false} width={48} />
            <Tooltip isAnimationActive={false} content={(props) => <ChartTooltip {...props} title={title} rows={gasRows} note={note} />} />
            <Area type="monotone" dataKey="gps" stroke="var(--series-1)" strokeWidth={2} fill="var(--series-1)" fillOpacity={0.1} isAnimationActive={false} activeDot={false} />
            {set ? set.constraints.map((c, i) => <ReferenceLine key={i} y={c.target} stroke={seriesColor(i)} strokeDasharray="4 3" />) : null}
          </ComposedChart>
        </ResponsiveContainer>
      </ChartBlock>

      <div className="rounded-md border border-hairline bg-surface p-3">
        <div className="mb-2 flex flex-wrap items-baseline justify-between gap-x-4 gap-y-1">
          <h3 className="text-sm font-semibold text-ink">Backlog per constraint (gas)</h3>
          <span className="text-xs text-ink-3">one panel per constraint, each on its own scale</span>
        </div>
        <div className={`grid gap-3 ${count > 2 ? "md:grid-cols-3" : "md:grid-cols-2"}`}>
          {indices.map((i) => (
            <div key={i}>
              <div className="mb-1 flex items-center gap-1.5 text-xs text-ink-2">
                <span className="inline-block h-2.5 w-2.5 rounded-[2px]" style={{ background: seriesColor(i) }} aria-hidden="true" />
                {seriesLabel(series, i)}
              </div>
              <ChartFrame height={140} minWidth={260} label={`Backlog of ${seriesLabel(series, i)} over time`}>
                <ResponsiveContainer width="100%" height="100%">
                  <AreaChart data={points} syncId={SYNC_ID} margin={{ top: 8, right: 12, bottom: 0, left: 0 }}>
                    <CartesianGrid vertical={false} />
                    {xAxis}
                    <YAxis tickFormatter={(v: number) => formatGas(v)} tickLine={false} axisLine={false} width={48} />
                    <Tooltip
                      isAnimationActive={false}
                      content={(props) => (
                        <ChartTooltip {...props} title={title} rows={[{ label: "backlog", color: seriesColor(i), value: (r) => `${formatGas(Number(r[`b${i}`] ?? 0))} gas` }]} note={note} />
                      )}
                    />
                    <Area type="monotone" dataKey={`b${i}`} stroke={seriesColor(i)} strokeWidth={2} fill={seriesColor(i)} fillOpacity={0.1} isAnimationActive={false} activeDot={false} />
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

      <details className="text-xs text-ink-2">
        <summary className="cursor-pointer select-none">Table view (latest 24 buckets)</summary>
        <div className="mt-2 overflow-x-auto">
          <table className="num w-full min-w-[640px] text-left">
            <thead className="text-ink-3">
              <tr>
                <th className="py-1 pr-3 font-medium">bucket</th>
                <th className="py-1 pr-3 font-medium">fee avg (gwei)</th>
                <th className="py-1 pr-3 font-medium">min</th>
                <th className="py-1 pr-3 font-medium">max</th>
                <th className="py-1 pr-3 font-medium">x</th>
                <th className="py-1 pr-3 font-medium">gas/s</th>
                {indices.map((i) => (
                  <th key={i} className="py-1 pr-3 font-medium">
                    backlog C{i + 1}
                  </th>
                ))}
              </tr>
            </thead>
            <tbody>
              {points.slice(-24).map((p: ChartPoint) => (
                <tr key={p.t} className="border-t border-hairline">
                  <td className="py-1 pr-3">{formatDateTime(p.t)}</td>
                  <td className="py-1 pr-3">{formatSignificant(p.feeAvg, 4)}</td>
                  <td className="py-1 pr-3">{formatSignificant(p.feeMin, 3)}</td>
                  <td className="py-1 pr-3">{formatSignificant(p.feeMax, 3)}</td>
                  <td className="py-1 pr-3">{p.x.toFixed(4)}</td>
                  <td className="py-1 pr-3">{formatGas(p.gps)}</td>
                  {indices.map((i) => (
                    <td key={i} className="py-1 pr-3">
                      {formatGas(Number(p[`b${i}`] ?? 0))}
                    </td>
                  ))}
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      </details>
    </div>
  );
}
