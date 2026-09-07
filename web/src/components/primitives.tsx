"use client";

import type { ReactNode } from "react";
import type { LiveStatus } from "@/types";

/** A titled section. The heading stands alone: sections carry no description line. */
export function Section({ id, title, children, aside }: { id: string; title: string; children: ReactNode; aside?: ReactNode }) {
  return (
    <section id={id} className="vw-rule py-8 first:border-t-0" aria-labelledby={`${id}-title`}>
      <header className="mb-5 flex flex-wrap items-baseline justify-between gap-x-6 gap-y-2">
        <h2 id={`${id}-title`} className="vw-title max-w-[65ch] text-lg font-bold tracking-tight text-ink" style={{ textWrap: "balance" }}>
          {title}
        </h2>
        {aside}
      </header>
      {children}
    </section>
  );
}

export function Label({ children }: { children: ReactNode }) {
  return <div className="text-[11px] font-medium uppercase tracking-[0.1em] text-label">{children}</div>;
}

/** A label/value pair. Values are set in the mono face with tabular figures. */
export function Stat({ label, value, unit, hint, size = "md" }: { label: ReactNode; value: ReactNode; unit?: ReactNode; hint?: ReactNode; size?: "sm" | "md" | "lg" }) {
  const valueClass = size === "lg" ? "text-3xl sm:text-4xl" : size === "sm" ? "text-base" : "text-xl";
  return (
    <div className="min-w-0">
      <Label>{label}</Label>
      <div className={`num mt-1 leading-none text-ink ${valueClass}`}>
        {value}
        {unit ? <span className="ml-1 text-[0.6em] font-normal text-ink-2">{unit}</span> : null}
      </div>
      {hint ? <div className="mt-1 text-xs text-ink-3">{hint}</div> : null}
    </div>
  );
}

/**
 * A live figure in a box of reserved width, so a changing digit or decimal
 * count never moves its neighbours: tabular figures in the mono face, and
 * `ch` (the width of a digit) reserved. The unit belongs outside, after it.
 */
export function Figure({ children, ch, className = "" }: { children: ReactNode; ch: number; className?: string }) {
  return (
    <span className={`num inline-block text-left tabular-nums ${className}`} style={{ minWidth: `${ch}ch` }}>
      {children}
    </span>
  );
}

export const STATUS_COPY: Record<LiveStatus, { label: string; tone: "good" | "warning" | "critical" | "neutral"; detail: string }> = {
  open: { label: "live", tone: "good", detail: "WebSocket connected, one update per collector tick" },
  connecting: { label: "connecting", tone: "neutral", detail: "Opening the WebSocket" },
  reconnecting: { label: "reconnecting", tone: "warning", detail: "Socket dropped, retrying with backoff and polling /live every 2 s" },
  polling: { label: "polling", tone: "warning", detail: "Socket unavailable, polling /live every 2 s while retrying" },
};

export function StatusPill({ status }: { status: LiveStatus }) {
  const copy = STATUS_COPY[status];
  const tone =
    copy.tone === "good" ? "bg-good vw-dot-glow" : copy.tone === "warning" ? "bg-warning" : copy.tone === "critical" ? "bg-critical" : "bg-ink-3";
  return (
    <span
      className="vw-control inline-flex items-center gap-2 px-2.5 py-1 text-xs font-medium"
      title={copy.detail}
      role="status"
      aria-live="polite"
    >
      <span className={`inline-block h-2 w-2 rounded-full ${tone}`} aria-hidden="true" />
      {copy.label}
    </span>
  );
}

/** The hatch geometry, shared by the chart pattern and its legend swatch so the two are the same mark. */
export const HATCH_SPACING = 6;
export const HATCH_STROKE = 2;

export type SwatchKind = "rect" | "line" | "hatch";

/**
 * The hatch a chart fills an unknown series with. Drawn at full strength: a
 * translucent hatch composites to about 2.2:1 on the light chart surface,
 * under the 3:1 a non-text mark needs. Its legend swatch repeats the same
 * geometry, so the association does not rest on colour alone.
 */
export function HatchPattern({ id, color }: { id: string; color: string }) {
  return (
    <pattern id={id} width={HATCH_SPACING} height={HATCH_SPACING} patternUnits="userSpaceOnUse" patternTransform="rotate(45)">
      <line x1={0} y1={0} x2={0} y2={HATCH_SPACING} stroke={color} strokeWidth={HATCH_STROKE} />
    </pattern>
  );
}

export function Swatch({ color, kind = "rect" }: { color: string; kind?: SwatchKind }) {
  if (kind === "line") {
    return <span className="inline-block h-0.5 w-4 rounded-full align-middle" style={{ background: color }} aria-hidden="true" />;
  }
  if (kind === "hatch") {
    // The same 45 degree hatch the chart fills with, at the same spacing, so
    // the legend carries the pattern and not only the colour.
    return (
      <span
        className="inline-block h-2.5 w-2.5 rounded-[2px] align-middle"
        style={{
          backgroundImage: `repeating-linear-gradient(45deg, ${color} 0, ${color} ${HATCH_STROKE}px, transparent ${HATCH_STROKE}px, transparent ${HATCH_SPACING}px)`,
          boxShadow: `inset 0 0 0 1px ${color}`,
        }}
        aria-hidden="true"
      />
    );
  }
  return <span className="inline-block h-2.5 w-2.5 rounded-[2px] align-middle" style={{ background: color }} aria-hidden="true" />;
}

export function Legend({ items }: { items: { label: string; color: string; kind?: SwatchKind }[] }) {
  return (
    <ul className="flex flex-wrap gap-x-4 gap-y-1 text-xs text-ink-2">
      {items.map((item) => (
        <li key={item.label} className="flex items-center gap-1.5">
          <Swatch color={item.color} kind={item.kind} />
          <span>{item.label}</span>
        </li>
      ))}
    </ul>
  );
}

/**
 * Room at the right of a time axis for half of its last tick label. The axis
 * spans the window a range asked for, so its last tick sits exactly at the
 * right edge and would otherwise be cut in half by the frame.
 */
export const TIME_AXIS_RIGHT = 22;

/**
 * How tall a chart frame stands: a number of pixels, or the utility classes
 * that size it. An enlarged chart is sized against the viewport, which is a
 * class and not a number.
 */
export type ChartHeight = number | string;

/**
 * Charts scroll inside this frame on narrow screens; the page never scrolls
 * sideways. The frame paints the chart surface, the colour every series
 * palette was validated against, so marks never sit on the card colour.
 */
export function ChartFrame({ height, minWidth = 560, children, label }: { height: ChartHeight; minWidth?: number; children: ReactNode; label: string }) {
  const sized = typeof height === "string";
  return (
    <div className="-mx-1 overflow-x-auto px-1" role="figure" aria-label={label}>
      <div className={`rounded-sm bg-chart ${sized ? height : ""}`} style={{ height: sized ? undefined : height, minWidth }}>
        {children}
      </div>
    </div>
  );
}

export function Card({ children, className = "" }: { children: ReactNode; className?: string }) {
  return <div className={`vw-card p-4 ${className}`}>{children}</div>;
}

export function Prose({ children }: { children: ReactNode }) {
  return <div className="max-w-[65ch] text-[15px] leading-relaxed text-ink-2 [&_code]:rounded [&_code]:bg-surface-2 [&_code]:px-1 [&_code]:py-0.5 [&_code]:font-mono [&_code]:text-[0.9em] [&_code]:text-ink [&_h3]:mt-6 [&_h3]:mb-2 [&_h3]:text-base [&_h3]:font-semibold [&_h3]:text-ink [&_p+p]:mt-3 [&_strong]:text-ink">{children}</div>;
}
