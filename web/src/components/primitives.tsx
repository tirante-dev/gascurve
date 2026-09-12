"use client";

import { useEffect, useState, type ReactNode } from "react";
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

/** Whether a stat is set plainly on the card or as an instrument readout. Both resolve through tokens,
 * and in light they resolve to the same thing, so the variant only shows itself in dark. */
export type StatTone = "ink" | "readout";

export function Stat({ label, value, unit, hint, size = "md", tone = "ink" }: { label: ReactNode; value: ReactNode; unit?: ReactNode; hint?: ReactNode; size?: "sm" | "md" | "lg"; tone?: StatTone }) {
  const valueClass = size === "lg" ? "text-3xl sm:text-4xl" : size === "sm" ? "text-base" : "text-xl";
  const readout = tone === "readout";
  return (
    <div className={`min-w-0 ${readout ? "vw-stat-panel" : ""}`}>
      <Label>{label}</Label>
      <div className={`num mt-1 leading-none ${readout ? "vw-readout-ink" : "text-ink"} ${valueClass}`}>
        {value}
        {unit ? <span className={`ml-1 text-[0.6em] font-normal ${readout ? "vw-readout-unit" : "text-ink-2"}`}>{unit}</span> : null}
      </div>
      {hint ? <div className="mt-1 text-xs text-ink-3">{hint}</div> : null}
    </div>
  );
}

/** A live figure in a box of reserved width, so a changing digit never moves its neighbours. `ch` is the
 * width of a digit; the unit belongs outside, after it. */
export function Figure({ children, ch, className = "" }: { children: ReactNode; ch: number; className?: string }) {
  return (
    <span className={`num inline-block text-left tabular-nums ${className}`} style={{ minWidth: `${ch}ch` }}>
      {children}
    </span>
  );
}

/** Which edge of the tile a HoverNote panel lines up with, so it opens into the card rather than over its edge. */
export type NoteAlign = "start" | "end";

/**
 * The edge classes for each alignment, and the `sm` overrides that let a note change edge at the
 * breakpoint: a reflowing grid moves a tile between columns, so one static edge cannot keep a panel
 * inside the card at both widths. Tailwind scans for whole class names, hence the table.
 */
const NOTE_ALIGN: Record<NoteAlign, string> = {
  start: "left-0",
  end: "right-0",
};
const NOTE_ALIGN_SM: Record<NoteAlign, string> = {
  start: "sm:left-0 sm:right-auto",
  end: "sm:right-0 sm:left-auto",
};

/**
 * Whether the note anchors to a block of its own or sits inside running text. A tile is a fixed-width
 * grid cell, so `block` places the panel against the whole tile. `inline` leaves the wrapper unpositioned
 * and the panel anchors to the nearest positioned ancestor: **the line element must be `relative`**.
 * Anchoring to the word itself does not work, since a panel is far wider than the word it explains.
 */
export type NoteFlow = "block" | "inline";

/**
 * Whether the note anchors to a block of its own or sits inside running text. A tile is a fixed-width grid
 * cell, so `block` places the panel against the whole tile. `inline` leaves the wrapper unpositioned and
 * the panel anchors to the nearest positioned ancestor: **the line element must be `relative`**.
 */
export function HoverNote({
  children,
  lines,
  description,
  align = "start",
  alignSm = align,
  flow = "block",
  lead = "figure",
}: {
  children: ReactNode;
  lines: readonly string[];
  description: string;
  align?: NoteAlign;
  /** The edge to line up with from the `sm` breakpoint up; defaults to `align`, which is one edge at every width. */
  alignSm?: NoteAlign;
  flow?: NoteFlow;
  /** What the first line is: a figure's working, set in the mono face, or a term's definition, set as text. */
  lead?: "figure" | "text";
}) {
  const [dismissed, setDismissed] = useState(false);
  const [under, setUnder] = useState(false);
  // Escape is caught here as well as on the trigger, because a reader who only hovered never moved focus.
  // Only while the pointer is on the note: a page of these should not each hold a document listener.
  useEffect(() => {
    if (!under) return;
    const close = (e: KeyboardEvent) => e.key === "Escape" && setDismissed(true);
    document.addEventListener("keydown", close);
    return () => document.removeEventListener("keydown", close);
  }, [under]);
  return (
    /* The panel is placed against the tile, not the figure: a note wider than the digits it explains has
       the whole tile to open into. An inline note anchors to the line it sits in, which is the call
       site's `relative` and not this wrapper's. */
    <span
      className={flow === "inline" ? "group" : "group relative block"}
      onMouseEnter={() => {
        setUnder(true);
        setDismissed(false);
      }}
      onMouseLeave={() => setUnder(false)}
      onFocus={() => setDismissed(false)}
    >
      {/* A border, not `underline`: the figure inside is an inline-block, which
          text-decoration does not reach, so an underline would rule the dollar
          sign and stop there. */}
      <span tabIndex={0} className="inline-block cursor-help border-b border-dotted border-ink-3 pb-0.5" onKeyDown={(e) => e.key === "Escape" && setDismissed(true)}>
        <span aria-hidden="true">{children}</span>
        <span className="sr-only">{description}</span>
      </span>
      {/* The outer box carries the gap as padding rather than margin, so the
          pointer crosses live ground on its way from the figure to the panel. */}
      <span aria-hidden="true" className={`absolute bottom-full z-20 hidden pb-2 ${dismissed ? "" : "group-focus-within:block group-hover:block"} ${NOTE_ALIGN[align]} ${NOTE_ALIGN_SM[alignSm]}`}>
        {/* Never wider than the viewport leaves room for: at 320 px, or at 400%
            zoom, the equation wraps rather than running off the card. */}
        <span className="block w-max max-w-[min(42ch,calc(100vw_-_5rem))] rounded-md border border-hairline bg-surface px-3 py-2 text-left text-xs font-normal leading-snug shadow-lg">
          {lines.map((line, i) => (
            <span key={i} className={i === 0 ? `${lead === "figure" ? "num " : ""}block text-ink` : "block text-ink-2"}>
              {line}
            </span>
          ))}
        </span>
      </span>
    </span>
  );
}

/**
 * Plain words on the page with the precise term a hover away. The label says
 * what a reader who has never met the pricer would call the thing ("Network
 * load"); the note says what the figure actually is ("compute gas per second,
 * averaged over 10 s") and, where one line is not enough, why. Nothing is
 * taken off the page: the term moves from the label into the note, and the
 * accessible name carries both.
 */
export function Term({ children, lines, align, alignSm, flow }: { children: string; lines: readonly string[]; align?: NoteAlign; alignSm?: NoteAlign; flow?: NoteFlow }) {
  return (
    <HoverNote lines={lines} description={`${children}: ${lines.join(". ")}.`} align={align} alignSm={alignSm} flow={flow} lead="text">
      {children}
    </HoverNote>
  );
}

/** The definition of the unit, the half that does not depend on the figure in front of it. */
const BIPS_NOTE = "basis points: 1 bip is 1/10,000. The pricer holds these as integers, never as floats.";

/**
 * What to print past 2^53, where a double stops carrying an exact integer and the digits that reach the
 * browser are no longer the ones the api sent, so there is no figure to quote. The api's own int64
 * ceiling lands here (9223372036854775807 parses back as 9223372036854776000), but the range is wider
 * than saturation, and the note must not claim more than the value proves.
 */
const OFF_SCALE = "off scale";
const OFF_SCALE_NOTE = "past 2^53, where a browser's numbers stop being exact, so these are not the digits the api sent. The pricer's int64 ceiling saturates into this range.";

/**
 * A figure quoted in basis points, with the unit's definition and its decimal value a hover away. The
 * pricer works in integer bips and the api hands them over unchanged, so rather than translate the unit
 * away the word carries what it means.
 */
export function Bips({ value, align = "end" }: { value: number; align?: NoteAlign }) {
  if (Math.abs(value) > Number.MAX_SAFE_INTEGER) {
    return (
      <HoverNote flow="inline" align={align} lead="text" lines={[OFF_SCALE_NOTE, BIPS_NOTE]} description={`${OFF_SCALE}: ${OFF_SCALE_NOTE} ${BIPS_NOTE}`}>
        {OFF_SCALE}
      </HoverNote>
    );
  }
  const bips = Math.round(value);
  // Four places is what the constraint cards already print x to, so the note
  // reads back as the figure above it rather than as a second rounding.
  const decimal = (bips / 10_000).toFixed(4);
  const figure = bips.toLocaleString("en-US");
  return (
    <HoverNote flow="inline" align={align} lines={[`${figure} bips = ${decimal}`, BIPS_NOTE]} description={`${figure} bips is ${decimal}. ${BIPS_NOTE}`}>
      {figure} bips
    </HoverNote>
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
 * The hatch a chart fills an unknown series with, drawn at full strength: a translucent hatch composites
 * to about 2.2:1 on the light chart surface, under the 3:1 a non-text mark needs. Its legend swatch
 * repeats the geometry, so the association does not rest on colour alone.
 */
export function HatchPattern({ id, color, angle = 45 }: { id: string; color: string; angle?: number }) {
  return (
    <pattern id={id} width={HATCH_SPACING} height={HATCH_SPACING} patternUnits="userSpaceOnUse" patternTransform={`rotate(${angle})`}>
      <line x1={0} y1={0} x2={0} y2={HATCH_SPACING} stroke={color} strokeWidth={HATCH_STROKE} />
    </pattern>
  );
}

export function Swatch({ color, kind = "rect" }: { color: string; kind?: SwatchKind }) {
  if (kind === "line") {
    return <span className="inline-block h-0.5 w-4 rounded-full align-middle" style={{ background: color }} aria-hidden="true" />;
  }
  if (kind === "hatch") {
    // The same 45 degree hatch the chart fills with, so the legend carries the pattern and not only the
    // colour.
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

/** Room at the right of a time axis for half of its last tick label: the axis spans the window a range
 * asked for, so its last tick sits exactly at the edge. */
export const TIME_AXIS_RIGHT = 22;

/** How tall a chart frame stands: pixels, or the utility classes that size it. An enlarged chart is sized
 * against the viewport, which is a class and not a number. */
export type ChartHeight = number | string;

/** The frame paints the chart surface, the colour every series palette was validated against. `minWidth`
 * is the width a chart is drawn for, capped at the frame: a card narrower than that shrinks the chart
 * rather than scrolling sideways, which is what a phone got before the cap. */
export function ChartFrame({ height, minWidth = 560, children, label }: { height: ChartHeight; minWidth?: number; children: ReactNode; label: string }) {
  const sized = typeof height === "string";
  return (
    <div className="-mx-1 overflow-x-auto px-1" role="figure" aria-label={label}>
      <div className={`rounded-sm bg-chart ${sized ? height : ""}`} style={{ height: sized ? undefined : height, minWidth: `min(${minWidth}px, 100%)` }}>
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
