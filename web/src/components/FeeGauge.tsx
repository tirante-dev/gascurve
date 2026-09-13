"use client";

import { useId } from "react";
import {
  DIAL_CX,
  DIAL_CY,
  DIAL_MAJOR_TICKS,
  DIAL_MAX,
  DIAL_MINOR_TICKS,
  DIAL_RADIUS,
  DIAL_SCALE_LABEL,
  DIAL_VIEW_H,
  DIAL_VIEW_W,
  dialArc,
  dialPoint,
  dialPosition,
  GRID_H,
  GRID_VANISH_X,
  GRID_W,
  gridLines,
  needlePoints,
  PRESSURE_WIDTH,
  R_BEZEL,
  R_HUB,
  R_NUMERAL,
  R_TICK_MAJOR,
  R_TICK_MINOR,
  R_TICK_OUT,
} from "@/lib/dial";
import { FIXED_WIDTH_CH, formatGweiFixed, formatMultiplierFixed } from "@/utils/format";
import { Figure, HoverNote, Term } from "./primitives";

/** How far the unlit ring is turned down. It has to read as the same tube as the lit stretch, unpowered. */
const DIM = 0.22;

const NUMERAL_TEXT: Record<number, string> = { 1: "1×", 10: "10×", [DIAL_MAX]: `${DIAL_MAX}×` };

function Ticks() {
  return (
    <>
      {DIAL_MINOR_TICKS.map((m) => {
        const a = dialPoint(dialPosition(m), R_TICK_MINOR);
        const b = dialPoint(dialPosition(m), R_TICK_OUT);
        return <line key={`min-${m}`} x1={a.x.toFixed(2)} y1={a.y.toFixed(2)} x2={b.x.toFixed(2)} y2={b.y.toFixed(2)} stroke="var(--ink-3)" strokeWidth={1} strokeLinecap="round" />;
      })}
      {DIAL_MAJOR_TICKS.map((m) => {
        const a = dialPoint(dialPosition(m), R_TICK_MAJOR);
        const b = dialPoint(dialPosition(m), R_TICK_OUT);
        const n = dialPoint(dialPosition(m), R_NUMERAL);
        const anchor = m === 1 ? "start" : m === DIAL_MAX ? "end" : "middle";
        return (
          <g key={`maj-${m}`}>
            <line x1={a.x.toFixed(2)} y1={a.y.toFixed(2)} x2={b.x.toFixed(2)} y2={b.y.toFixed(2)} stroke="var(--ink-2)" strokeWidth={1.8} strokeLinecap="round" />
            <text x={n.x.toFixed(1)} y={n.y.toFixed(1)} textAnchor={anchor} dominantBaseline="middle" fontFamily="var(--font-display)" fontWeight={500} fontSize={10} fill="var(--ink-2)">
              {NUMERAL_TEXT[m]}
            </text>
          </g>
        );
      })}
    </>
  );
}

/**
 * The perspective grid under the horizon, stretched to whatever height the readout needs and masked so
 * it dissolves before the bottom edge rather than ending on a line.
 */
function Grid({ maskId }: { maskId: string }) {
  const { verticals, horizontals } = gridLines();
  return (
    <svg viewBox={`0 0 ${GRID_W} ${GRID_H}`} preserveAspectRatio="none" className="absolute inset-0 h-full w-full" aria-hidden="true" focusable="false">
      <defs>
        <linearGradient id={`${maskId}-fade`} x1="0" y1="0" x2="0" y2="1">
          <stop offset="0%" stopColor="#fff" stopOpacity={0.95} />
          <stop offset="100%" stopColor="#fff" stopOpacity={0} />
        </linearGradient>
        <mask id={maskId}>
          <rect x={0} y={0} width={GRID_W} height={GRID_H} fill={`url(#${maskId}-fade)`} />
        </mask>
      </defs>
      <g mask={`url(#${maskId})`} opacity={0.5}>
        {verticals.map((x) => (
          <line key={`v${x}`} x1={GRID_VANISH_X} y1={0} x2={x} y2={GRID_H} stroke="var(--accent)" strokeWidth={0.7} />
        ))}
        {horizontals.map((y) => (
          <line key={`h${y}`} x1={0} y1={y.toFixed(1)} x2={GRID_W} y2={y.toFixed(1)} stroke="var(--accent)" strokeWidth={0.7} />
        ))}
      </g>
    </svg>
  );
}

/**
 * The base fee as an instrument: a ring lit from the floor to the multiplier, a needle at the reading,
 * and the figure on the grid below the horizon. Nothing but the needle is drawn inside the arc, because
 * a six digit figure does not fit a semicircle at every rail width.
 *
 * The face and the ground are tokens, so the gauge sits on the card in light and on an inset dark panel
 * in dark without a second markup tree.
 */
export function FeeGauge({ baseFeeGwei, floorGwei, multiplier, exponent }: { baseFeeGwei: number; floorGwei: string; multiplier: number; exponent: number }) {
  const uid = useId();
  const figure = formatMultiplierFixed(multiplier);
  const position = dialPosition(multiplier);
  // Each side is rounded on its own, so the product is only ever about equal.
  const lines = [`${figure}× the ${floorGwei} gwei floor`, `${formatGweiFixed(baseFeeGwei)} gwei ≈ ${figure} × ${floorGwei} gwei`];
  const scale = multiplier > DIAL_MAX ? `above the displayed logarithmic scale, which runs from 1 to ${DIAL_MAX} times the floor` : `on a logarithmic scale from 1 to ${DIAL_MAX} times the floor`;
  const description = `${figure} times the ${floorGwei} gwei floor, ${scale}.`;
  return (
    <div className="vw-gauge mx-auto w-full max-w-[220px] rounded-md sm:max-w-[280px] lg:max-w-none" data-testid="fee-gauge">
      <svg viewBox={`0 0 ${DIAL_VIEW_W} ${DIAL_VIEW_H}`} className="block h-auto w-full" aria-hidden="true" focusable="false">
        <defs>
          <filter id={`${uid}-neon`} x="-60%" y="-60%" width="220%" height="220%">
            <feGaussianBlur stdDeviation={2.8} result="b" />
            <feMerge>
              <feMergeNode in="b" />
              <feMergeNode in="b" />
              <feMergeNode in="SourceGraphic" />
            </feMerge>
          </filter>
          <filter id={`${uid}-soft`} x="-60%" y="-60%" width="220%" height="220%">
            <feGaussianBlur stdDeviation={1.4} result="b" />
            <feMerge>
              <feMergeNode in="b" />
              <feMergeNode in="SourceGraphic" />
            </feMerge>
          </filter>
          <linearGradient id={`${uid}-chrome`} x1="0" y1="0" x2="1" y2="0">
            <stop offset="0%" stopColor="var(--accent)" />
            <stop offset="50%" stopColor="var(--gauge-chrome-mid)" />
            <stop offset="100%" stopColor="var(--accent-2)" />
          </linearGradient>
          <linearGradient id={`${uid}-pressure`} gradientUnits="userSpaceOnUse" x1={DIAL_CX - DIAL_RADIUS} y1={0} x2={DIAL_CX + DIAL_RADIUS} y2={0} data-testid="fee-gauge-pressure-gradient">
            <stop offset="0%" stopColor="var(--seq-5)" />
            <stop offset="50%" stopColor="var(--seq-6)" />
            <stop offset="100%" stopColor="var(--seq-7)" />
          </linearGradient>
        </defs>
        <path d={dialArc(0, 1, R_BEZEL)} fill="none" stroke={`url(#${uid}-chrome)`} strokeWidth={1.6} opacity={0.9} />
        <Ticks />
        <path d={dialArc(0, 1)} fill="none" stroke={`url(#${uid}-pressure)`} strokeWidth={PRESSURE_WIDTH} strokeLinecap="butt" opacity={DIM} data-pressure="track" />
        {position > 0 ? <path d={dialArc(0, position)} fill="none" stroke={`url(#${uid}-pressure)`} strokeWidth={PRESSURE_WIDTH} strokeLinecap="butt" filter={`url(#${uid}-neon)`} data-pressure="reading" data-position={position.toFixed(4)} /> : null}
        <line x1={6} y1={DIAL_CY} x2={DIAL_VIEW_W - 6} y2={DIAL_CY} stroke="var(--gauge-horizon)" strokeWidth={1.1} filter={`url(#${uid}-soft)`} />
        <polygon
          points={needlePoints(multiplier)
            .map((p) => `${p.x.toFixed(2)},${p.y.toFixed(2)}`)
            .join(" ")}
          fill="var(--ink)"
          filter={`url(#${uid}-soft)`}
          data-testid="fee-gauge-needle"
          data-position={dialPosition(multiplier).toFixed(4)}
        />
        <circle cx={DIAL_CX} cy={DIAL_CY} r={R_HUB} fill={`url(#${uid}-chrome)`} />
        <circle cx={DIAL_CX} cy={DIAL_CY} r={3} fill="var(--ink)" />
      </svg>
      <div className="relative pb-2.5">
        <Grid maskId={`${uid}-grid`} />
        <div className="vw-readout relative px-2 pt-2.5 pb-1.5 text-center">
          <div className="text-[11px] font-medium uppercase tracking-[0.1em] text-label">
            <Term lines={BASE_FEE_NOTE}>Base fee now</Term>
          </div>
          <div className="num mt-1 text-4xl leading-none tracking-tight text-ink sm:text-5xl">
            <Figure ch={FIXED_WIDTH_CH.gwei} className="vw-hero">
              {formatGweiFixed(baseFeeGwei)}
            </Figure>
            <span className="ml-1.5 text-lg font-normal text-ink-2">gwei</span>
          </div>
          <div className="num mt-2 text-sm text-ink" data-testid="fee-gauge-multiplier">
            <HoverNote lines={lines} description={description} flow="inline">
              <Figure ch={FIXED_WIDTH_CH.multiplier}>{figure}</Figure>&#215;
            </HoverNote>
            <span className="ml-1.5 text-[11px] font-medium uppercase tracking-[0.08em] text-ink-3">over floor</span>
          </div>
          <div className="num mt-1.5 text-xs text-ink-3">
            floor {floorGwei} gwei &middot; x <Figure ch={FIXED_WIDTH_CH.x}>{exponent.toFixed(4)}</Figure>
          </div>
          <div className="mt-1 text-[10px] font-medium uppercase tracking-[0.08em] text-ink-3">Pressure scale &middot; {DIAL_SCALE_LABEL}</div>
        </div>
      </div>
    </div>
  );
}

const BASE_FEE_NOTE = [
  "the price of one unit of gas right now, in gwei",
  "a gwei is a billionth of an ETH; every transaction pays this much per unit of gas it uses, and there are no tips",
] as const;
