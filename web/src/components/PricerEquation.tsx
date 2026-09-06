"use client";

import { useMemo } from "react";
import { CartesianGrid, Line, LineChart, ReferenceDot, ResponsiveContainer, Tooltip, XAxis, YAxis } from "recharts";
import { approxExpBips, baseFeeFromExponent, contributionsBips, legacyExponentBips, p4Multiplier, toLegacyState, trueExpMultiplier } from "@/lib/pricer";
import type { LiveSnapshot } from "@/types";
import { seriesColor, taylorCurve } from "@/utils/chart";
import { formatGwei, formatInteger } from "@/utils/format";
import { ChartTooltip } from "./ChartTooltip";
import { ChartFrame, Legend } from "./primitives";

function Term({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <span className="inline-flex flex-col items-center">
      <span className="num text-ink">{children}</span>
      <span className="mt-0.5 text-[10px] uppercase tracking-[0.08em] text-ink-3">{label}</span>
    </span>
  );
}

function Op({ children }: { children: React.ReactNode }) {
  return <span className="self-start pt-[2px] text-ink-3">{children}</span>;
}

/** The live pricer equation with the sampled numbers, and P4 against e^x. */
export function PricerEquation({ snapshot }: { snapshot: LiveSnapshot | null }) {
  const curve = useMemo(() => taylorCurve(p4Multiplier, trueExpMultiplier, 5, 0.1), []);
  const live = useMemo(() => {
    if (!snapshot) return null;
    const minFee = BigInt(snapshot.minBaseFee);
    if (snapshot.model === "legacy" && snapshot.legacy) {
      const exponent = legacyExponentBips(toLegacyState(snapshot.legacy));
      return { contributions: [Number(exponent)], exponent: Number(exponent), predicted: baseFeeFromExponent(minFee, exponent), multiplier: Number(approxExpBips(exponent)) / 10_000 };
    }
    const contributions = contributionsBips(snapshot.constraints);
    const exponent = contributions.reduce((a, b) => a + b, 0);
    const bips = BigInt(exponent);
    return { contributions, exponent, predicted: baseFeeFromExponent(minFee, bips), multiplier: Number(approxExpBips(bips)) / 10_000 };
  }, [snapshot]);
  const x = live ? live.exponent / 10_000 : 0;
  const legacy = snapshot?.model === "legacy";

  return (
    <div className="grid gap-6 lg:grid-cols-[minmax(0,1.2fr)_minmax(0,1fr)]">
      <div>
        <div className="overflow-x-auto rounded-md border border-hairline bg-surface p-4">
          <div className="num flex min-w-max flex-wrap items-start gap-x-2 gap-y-3 text-base text-ink-2 sm:text-lg">
            <Term label="base fee">baseFee</Term>
            <Op>=</Op>
            <Term label="floor">minBaseFee</Term>
            <Op>×</Op>
            <Term label="degree-4 Taylor">P4(</Term>
            {legacy ? (
              <Term label="legacy exponent">(backlog − tolerance·speedLimit) / (inertia·speedLimit)</Term>
            ) : (
              <>
                <Term label="sum over constraints">Σ</Term>
                <Term label="backlog over target × window">
                  backlog<sub>i</sub> / (T<sub>i</sub> × W<sub>i</sub>)
                </Term>
              </>
            )}
            <Term label="">)</Term>
          </div>
          {snapshot && live ? (
            <div className="num mt-4 flex min-w-max flex-wrap items-start gap-x-2 gap-y-3 border-t border-hairline pt-4 text-base text-ink-2 sm:text-lg">
              <Term label="predicted">{formatGwei(live.predicted)} gwei</Term>
              <Op>=</Op>
              <Term label="floor">{formatGwei(snapshot.minBaseFee)} gwei</Term>
              <Op>×</Op>
              <Term label={`P4(${x.toFixed(4)})`}>{live.multiplier.toFixed(2)}</Term>
              {!legacy ? (
                <>
                  <Op>with x =</Op>
                  {live.contributions.map((c, i) => (
                    <span key={i} className="inline-flex items-start gap-x-2">
                      {i > 0 ? <Op>+</Op> : null}
                      <Term label={`x${i + 1}`}>
                        <span className="mr-1 inline-block h-2 w-2 rounded-[2px] align-middle" style={{ background: seriesColor(i) }} aria-hidden="true" />
                        {(c / 10_000).toFixed(4)}
                      </Term>
                    </span>
                  ))}
                </>
              ) : (
                <Term label="x">{x.toFixed(4)}</Term>
              )}
            </div>
          ) : null}
        </div>
        {snapshot ? (
          <p className="mt-3 max-w-[65ch] text-sm text-ink-2">
            The chain priced block {formatInteger(snapshot.block.number)} at <span className="num text-ink">{formatGwei(snapshot.baseFee)} gwei</span>. The sampled backlogs already include that block&apos;s gas, so the
            prediction from them is the fee the next block will open with; the two differ by the gas of one block. True e<sup>x</sup> at this x would give{" "}
            <span className="num text-ink">{trueExpMultiplier(x).toFixed(1)}×</span> instead of <span className="num text-ink">{live?.multiplier.toFixed(2)}×</span>.
          </p>
        ) : null}
      </div>

      <div>
        <div className="mb-2 flex flex-wrap items-baseline justify-between gap-2">
          <h3 className="text-sm font-semibold text-ink">P4(x) against e^x</h3>
          <Legend
            items={[
              { label: "P4, what the chain uses", color: "var(--series-1)", kind: "line" },
              { label: "e^x", color: "var(--series-2)", kind: "line" },
            ]}
          />
        </div>
        <ChartFrame height={220} minWidth={320} label="Degree-4 Taylor polynomial compared with the exponential for x from 0 to 5">
          <ResponsiveContainer width="100%" height="100%">
            <LineChart data={curve} margin={{ top: 8, right: 12, bottom: 4, left: 0 }}>
              <CartesianGrid vertical={false} />
              <XAxis dataKey="x" type="number" domain={[0, 5]} ticks={[0, 1, 2, 3, 4, 5]} tickLine={false} axisLine={false} />
              <YAxis scale="log" domain={[1, 200]} ticks={[1, 10, 100]} tickLine={false} axisLine={false} width={36} />
              <Tooltip
                isAnimationActive={false}
                content={(props) => (
                  <ChartTooltip
                    {...props}
                    title={(l) => `x = ${l.toFixed(1)}`}
                    rows={[
                      { label: "P4(x)", color: "var(--series-1)", value: (r) => `${Number(r.p4).toFixed(2)}×` },
                      { label: "e^x", color: "var(--series-2)", value: (r) => `${Number(r.exp).toFixed(2)}×` },
                    ]}
                  />
                )}
              />
              <Line type="monotone" dataKey="exp" stroke="var(--series-2)" strokeWidth={2} dot={false} isAnimationActive={false} />
              <Line type="monotone" dataKey="p4" stroke="var(--series-1)" strokeWidth={2} dot={false} isAnimationActive={false} />
              {live ? <ReferenceDot x={Math.min(5, Math.round(x * 10) / 10)} y={Math.max(1, live.multiplier)} r={5} fill="var(--series-1)" stroke="var(--surface)" strokeWidth={2} /> : null}
            </LineChart>
          </ResponsiveContainer>
        </ChartFrame>
        <p className="mt-2 text-xs text-ink-3">Log scale. The dot marks the live x. Above x ≈ 2 the fee grows like x⁴/24, a polynomial, not an exponential.</p>
      </div>
    </div>
  );
}
