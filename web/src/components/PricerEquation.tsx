"use client";

import { useMemo } from "react";
import { approxExpBips, baseFeeFromExponent, contributionsBips, legacyExponentBips, toLegacyState, trueExpMultiplier } from "@/lib/pricer";
import type { LiveSnapshot } from "@/types";
import { seriesColor } from "@/utils/chart";
import { formatGwei, formatInteger } from "@/utils/format";

/** What the sampled state prices to: the per-constraint contributions in bips, their sum, the fee they imply with dt = 0, and P4 of it. */
export type LivePricing = { contributions: number[]; exponent: number; predicted: bigint; multiplier: number };

/** The pricing a snapshot implies, through the same integer pricer the chain uses. Null without a snapshot. */
export function livePricing(snapshot: LiveSnapshot | null): LivePricing | null {
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
}

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

/** The live pricer equation with the sampled numbers. P4 against e^x lives on the explainer page. */
export function PricerEquation({ snapshot }: { snapshot: LiveSnapshot | null }) {
  const live = useMemo(() => livePricing(snapshot), [snapshot]);
  const x = live ? live.exponent / 10_000 : 0;
  const legacy = snapshot?.model === "legacy";

  return (
    <div>
      <div className="vw-card overflow-x-auto p-4">
        <div className="num flex flex-wrap items-start gap-x-2 gap-y-3 text-base text-ink-2 sm:text-lg">
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
          <div className="num mt-4 flex flex-wrap items-start gap-x-2 gap-y-3 border-t border-hairline pt-4 text-base text-ink-2 sm:text-lg">
            <Term label="implied, dt = 0">{formatGwei(live.predicted)} gwei</Term>
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
          The chain priced block {formatInteger(snapshot.block.number)} at <span className="num text-ink">{formatGwei(snapshot.baseFee)} gwei</span>. The sampled backlogs already include that block&apos;s gas; the
          value above is the fee they imply with dt = 0, that is if the next block carried the same timestamp. When the next header&apos;s timestamp advances, every backlog is first paid down by
          dt × target, so the fee the next block actually opens with can be lower and is not known until that timestamp is. True e<sup>x</sup> at this x would give{" "}
          <span className="num text-ink">{trueExpMultiplier(x).toFixed(1)}×</span> instead of <span className="num text-ink">{live?.multiplier.toFixed(2)}×</span>.
        </p>
      ) : null}
    </div>
  );
}
