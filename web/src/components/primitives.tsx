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

/** Which edge of the tile a HoverNote panel lines up with, so it opens into the card rather than over its edge. */
export type NoteAlign = "start" | "end";

/**
 * Whether the note anchors to a block of its own or sits inside a line of
 * running text. A tile is a fixed-width grid cell, so `block` places the panel
 * against the whole tile. A word mid-sentence cannot have a block wrapper
 * without breaking the line around it, so `inline` leaves the wrapper
 * unpositioned and the panel anchors to the nearest positioned ancestor
 * instead: **the line element must be `relative`**. Anchoring to the word
 * itself is what does not work. A panel is far wider than the word it explains,
 * so at a phone's width it runs off whichever edge the word sits nearer, and
 * neither `align` saves it.
 */
export type NoteFlow = "block" | "inline";

/**
 * A figure whose working is a hover away. The browser's own `title` tooltip
 * was the obvious way to carry it and the wrong one: it gives the reader
 * nothing to notice, waits about a second, and draws in the platform's chrome
 * rather than the panel the charts already read out in. So the trigger says it
 * is inspectable (a dotted rule and a help cursor) and the panel is the one
 * from ChartTooltip.
 *
 * Focus opens it as hover does, so the working is not behind a pointer, and
 * `description` states the same facts in the accessible name for a reader that
 * gets neither. The panel is `aria-hidden` because that description already
 * carries it: announcing both would say everything twice.
 *
 * WCAG 1.4.13 asks that content shown on hover or focus be hoverable and
 * dismissable, so the panel takes the pointer (with the gap above the figure
 * bridged, or crossing it would close the panel on the way in) and Escape
 * closes it.
 *
 * Escape has to be caught twice over, because the two ways in leave the key
 * somewhere different. A reader who focused the figure sends it to the figure;
 * a reader who only hovered has never moved focus, so it goes to whatever holds
 * it, usually the body. Hence a handler on the trigger and, while the pointer
 * is over the note, one on the document. Dismissing has to work without moving
 * the pointer, which is the whole point of the requirement.
 *
 * Escape is undone on the way in, by the pointer or the focus arriving, rather
 * than on the way out. Both edges would do in the ordinary case; arriving is
 * the one to hang it on because of how the two fail. A missed leave leaves the
 * note permanently unopenable, which is worse than what it was fixing; a
 * missed arrival costs nothing, because the next one clears it.
 */
export function HoverNote({ children, lines, description, align = "start", flow = "block" }: { children: ReactNode; lines: readonly string[]; description: string; align?: NoteAlign; flow?: NoteFlow }) {
  const [dismissed, setDismissed] = useState(false);
  const [under, setUnder] = useState(false);
  // Only while the pointer is on the note: a page of these should not each hold
  // a document listener for a key that is not being pressed at them.
  useEffect(() => {
    if (!under) return;
    const close = (e: KeyboardEvent) => e.key === "Escape" && setDismissed(true);
    document.addEventListener("keydown", close);
    return () => document.removeEventListener("keydown", close);
  }, [under]);
  return (
    /* The panel is placed against the tile, not against the figure: a note wider
       than the digits it explains has the whole tile to open into, which is what
       keeps the right-hand one of a pair on screen at a phone's width. An inline
       note anchors to the line it sits in for the same reason, which is the
       call site's `relative` and not this wrapper's. */
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
      <span aria-hidden="true" className={`absolute bottom-full z-20 hidden pb-2 ${dismissed ? "" : "group-focus-within:block group-hover:block"} ${align === "end" ? "right-0" : "left-0"}`}>
        {/* Never wider than the viewport leaves room for: at 320 px, or at 400%
            zoom, the equation wraps rather than running off the card. */}
        <span className="block w-max max-w-[min(42ch,calc(100vw_-_5rem))] rounded-md border border-hairline bg-surface px-3 py-2 text-left text-xs font-normal leading-snug shadow-lg">
          {lines.map((line, i) => (
            <span key={line} className={i === 0 ? "num block text-ink" : "block text-ink-2"}>
              {line}
            </span>
          ))}
        </span>
      </span>
    </span>
  );
}

/** The definition of the unit, the half that does not depend on the figure in front of it. */
const BIPS_NOTE = "basis points: 1 bip is 1/10,000. The pricer holds these as integers, never as floats.";

/**
 * A figure quoted in basis points, with the unit's definition and its own
 * value in ordinary decimal a hover away. The pricer works in integer bips and
 * the api hands them over unchanged, so the raw unit reaches the page; rather
 * than translate it away (the integer is the thing the pricer actually holds)
 * the word carries what it means.
 */
export function Bips({ value, align = "end" }: { value: number; align?: NoteAlign }) {
  const bips = Math.round(value);
  // Four places is what the constraint cards already print x to, so the note
  // reads back as the figure above it rather than as a second rounding.
  const decimal = (bips / 10_000).toFixed(4);
  const figure = bips.toLocaleString("en-US");
  return (
    <HoverNote flow="inline" align={align} lines={[`${figure} bips = ${decimal}`, BIPS_NOTE]} description={`${figure} bips is ${decimal}. ${BIPS_NOTE}`}>
      {figure} <abbr>bips</abbr>
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
