"use client";

import { useMemo } from "react";
import { CartesianGrid, Line, LineChart, ReferenceDot, ResponsiveContainer, Tooltip, XAxis, YAxis } from "recharts";
import { p4Multiplier, trueExpMultiplier } from "@/lib/pricer";
import type { LiveSnapshot } from "@/types";
import { taylorCurve } from "@/utils/chart";
import { ChartTooltip } from "./ChartTooltip";
import { livePricing } from "./PricerEquation";
import { ChartFrame, Legend } from "./primitives";

/** P4 against e^x, with a dot on the live x when the chain has one to mark. */
export function TaylorChart({ snapshot }: { snapshot: LiveSnapshot | null }) {
  const curve = useMemo(() => taylorCurve(p4Multiplier, trueExpMultiplier, 5, 0.1), []);
  const live = useMemo(() => livePricing(snapshot), [snapshot]);
  const x = live ? live.exponent / 10_000 : 0;
  return (
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
            {live ? <ReferenceDot x={Math.min(5, Math.round(x * 10) / 10)} y={Math.max(1, live.multiplier)} r={5} fill="var(--series-1)" stroke="var(--chart)" strokeWidth={2} /> : null}
          </LineChart>
        </ResponsiveContainer>
      </ChartFrame>
      <p className="mt-2 text-xs text-ink-3">
        Log scale. {live ? "The dot marks the live x. " : ""}Above x ≈ 2 the fee grows like x⁴/24, a polynomial, not an exponential.
      </p>
    </div>
  );
}
