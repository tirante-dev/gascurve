"use client";

import { createContext, useCallback, useContext, useMemo, useRef, useState, type MouseEvent, type PointerEvent, type ReactNode } from "react";
import { formatDateTime, formatDuration } from "@/utils/format";

export type TimeDomain = readonly [number, number];

type PlotRect = {
  left: number;
  top: number;
  width: number;
  height: number;
  viewportLeft: number;
  viewportTop: number;
};

type TimeZoomValue = {
  domain: TimeDomain;
  span: number;
  zoomed: boolean;
  mode: "timestamp" | "relative";
  select: (domain: TimeDomain) => void;
  reset: () => void;
};

type Drag = {
  start: number;
  end: number;
  plot: PlotRect;
  pointerId: number;
};

const TimeZoomContext = createContext<TimeZoomValue | null>(null);

function within(domain: TimeDomain, full: TimeDomain): TimeDomain {
  const from = Math.max(full[0], domain[0]);
  const to = Math.min(full[1], domain[1]);
  return to > from ? [from, to] : full;
}

function numberAttribute(element: Element, name: string): number | null {
  const attribute = element.getAttribute(name);
  if (attribute === null) return null;
  const value = Number(attribute);
  return Number.isFinite(value) ? value : null;
}

function plotRect(surface: HTMLDivElement): PlotRect | null {
  const surfaceBox = surface.getBoundingClientRect();
  if (surfaceBox.width <= 0 || surfaceBox.height <= 0) return null;
  const fallback = { left: 0, top: 0, width: surfaceBox.width, height: surfaceBox.height, viewportLeft: surfaceBox.left, viewportTop: surfaceBox.top };
  const svg = surface.querySelector<SVGSVGElement>("svg.recharts-surface");
  const clip = svg?.querySelector<SVGRectElement>('clipPath[id$="-clip"] > rect');
  if (!svg || !clip) return fallback;
  const svgBox = svg.getBoundingClientRect();
  const viewBox = svg.getAttribute("viewBox")?.split(/\s+/).map(Number);
  const viewLeft = viewBox?.[0] ?? 0;
  const viewTop = viewBox?.[1] ?? 0;
  const viewWidth = viewBox?.[2] ?? numberAttribute(svg, "width");
  const viewHeight = viewBox?.[3] ?? numberAttribute(svg, "height");
  const x = numberAttribute(clip, "x");
  const y = numberAttribute(clip, "y");
  const width = numberAttribute(clip, "width");
  const height = numberAttribute(clip, "height");
  if (viewWidth === null || viewHeight === null || viewWidth <= 0 || viewHeight <= 0 || x === null || y === null || width === null || height === null || width <= 0 || height <= 0) return fallback;
  const scaleX = svgBox.width / viewWidth;
  const scaleY = svgBox.height / viewHeight;
  const viewportLeft = svgBox.left + (x - viewLeft) * scaleX;
  const viewportTop = svgBox.top + (y - viewTop) * scaleY;
  return {
    left: viewportLeft - surfaceBox.left,
    top: viewportTop - surfaceBox.top,
    width: width * scaleX,
    height: height * scaleY,
    viewportLeft,
    viewportTop,
  };
}

function fractionAt(clientX: number, plot: PlotRect): number {
  return Math.max(0, Math.min(1, (clientX - plot.viewportLeft) / plot.width));
}

function drawSelection(selection: HTMLDivElement | null, drag: Drag | null) {
  if (selection === null) return;
  if (drag === null || drag.start === drag.end) {
    selection.style.display = "none";
    return;
  }
  const from = Math.min(drag.start, drag.end);
  const to = Math.max(drag.start, drag.end);
  selection.style.display = "block";
  selection.style.transform = `translate3d(${drag.plot.left + from * drag.plot.width}px, ${drag.plot.top}px, 0) scale3d(${(to - from) * drag.plot.width}, ${drag.plot.height}, 1)`;
}

export function TimeZoomProvider({ domain, mode = "timestamp", children }: { domain: TimeDomain | null; mode?: "timestamp" | "relative"; children: ReactNode }) {
  const [selected, setSelected] = useState<TimeDomain | null>(null);
  const valid = domain !== null && Number.isFinite(domain[0]) && Number.isFinite(domain[1]) && domain[1] > domain[0];
  const fullFrom = valid ? domain[0] : 0;
  const fullTo = valid ? domain[1] : 1;
  const full = useMemo<TimeDomain>(() => [fullFrom, fullTo], [fullFrom, fullTo]);
  const visible = selected === null ? full : within(selected, full);

  const reset = useCallback(() => {
    setSelected(null);
  }, []);
  const select = useCallback(
    (next: TimeDomain) => {
      const bounded = within(next, visible);
      if (bounded[1] - bounded[0] >= (visible[1] - visible[0]) / 500) setSelected(bounded);
    },
    [visible],
  );
  const value = useMemo<TimeZoomValue>(
    () => ({ domain: visible, span: visible[1] - visible[0], zoomed: visible[0] > full[0] || visible[1] < full[1], mode, select, reset }),
    [full, mode, reset, select, visible],
  );

  if (!valid) return children;
  return <TimeZoomContext value={value}>{children}</TimeZoomContext>;
}

export function useTimeZoomChart(): TimeZoomValue | null {
  return useContext(TimeZoomContext);
}

export function TimeZoomSurface({ zoom, children }: { zoom: TimeZoomValue | null; children: ReactNode }) {
  const selection = useRef<HTMLDivElement>(null);
  const drag = useRef<Drag | null>(null);

  const begin = useCallback(
    (event: PointerEvent<HTMLDivElement>) => {
      if (event.button !== 0 || zoom === null) return;
      const plot = plotRect(event.currentTarget);
      if (plot === null || event.clientX < plot.viewportLeft || event.clientX > plot.viewportLeft + plot.width || event.clientY < plot.viewportTop || event.clientY > plot.viewportTop + plot.height) return;
      event.preventDefault();
      event.stopPropagation();
      event.currentTarget.setPointerCapture?.(event.pointerId);
      const at = fractionAt(event.clientX, plot);
      drag.current = { start: at, end: at, plot, pointerId: event.pointerId };
      drawSelection(selection.current, drag.current);
    },
    [zoom],
  );
  const move = useCallback((event: PointerEvent<HTMLDivElement>) => {
    if (drag.current === null || event.pointerId !== drag.current.pointerId) return;
    event.preventDefault();
    event.stopPropagation();
    drag.current.end = fractionAt(event.clientX, drag.current.plot);
    drawSelection(selection.current, drag.current);
  }, []);
  const finish = useCallback(
    (event: PointerEvent<HTMLDivElement>) => {
      const current = drag.current;
      if (current === null || zoom === null || event.pointerId !== current.pointerId) return;
      event.preventDefault();
      event.stopPropagation();
      current.end = fractionAt(event.clientX, current.plot);
      drag.current = null;
      drawSelection(selection.current, null);
      event.currentTarget.releasePointerCapture?.(event.pointerId);
      const from = Math.min(current.start, current.end);
      const to = Math.max(current.start, current.end);
      zoom.select([zoom.domain[0] + from * zoom.span, zoom.domain[0] + to * zoom.span]);
    },
    [zoom],
  );
  const cancel = useCallback((event: PointerEvent<HTMLDivElement>) => {
    if (drag.current === null || event.pointerId !== drag.current.pointerId) return;
    event.preventDefault();
    event.stopPropagation();
    drag.current = null;
    drawSelection(selection.current, null);
    event.currentTarget.releasePointerCapture?.(event.pointerId);
  }, []);
  const guardMouse = useCallback((event: MouseEvent<HTMLDivElement>) => {
    if (drag.current === null) return;
    event.preventDefault();
    event.stopPropagation();
  }, []);
  const reset = useCallback(
    (event: MouseEvent<HTMLDivElement>) => {
      if (zoom === null) return;
      event.preventDefault();
      event.stopPropagation();
      drag.current = null;
      drawSelection(selection.current, null);
      zoom.reset();
    },
    [zoom],
  );

  return (
    <div
      className={`relative h-full w-full ${zoom ? "cursor-crosshair select-none" : ""}`}
      onPointerDownCapture={begin}
      onPointerMoveCapture={move}
      onPointerUpCapture={finish}
      onPointerCancelCapture={cancel}
      onMouseDownCapture={guardMouse}
      onMouseMoveCapture={guardMouse}
      onDoubleClickCapture={reset}
    >
      {children}
      <div
        ref={selection}
        data-time-zoom-selection=""
        aria-hidden="true"
        className="pointer-events-none absolute left-0 top-0 z-10 hidden h-px w-px origin-top-left"
        style={{ background: "color-mix(in srgb, var(--accent-2) 20%, transparent)", willChange: "transform" }}
      />
    </div>
  );
}

function relativeTime(value: number): string {
  return value >= 0 ? "now" : `${formatDuration(Math.abs(value))} ago`;
}

export function TimeZoomControls({ className = "" }: { className?: string }) {
  const zoom = useContext(TimeZoomContext);
  if (zoom === null) return null;
  const from = zoom.mode === "relative" ? relativeTime(zoom.domain[0]) : formatDateTime(zoom.domain[0]);
  const to = zoom.mode === "relative" ? relativeTime(zoom.domain[1]) : formatDateTime(zoom.domain[1]);
  return (
    <div className={`flex flex-wrap items-center justify-between gap-2 text-xs text-ink-3 ${className}`}>
      {zoom.zoomed ? (
        <span role="status">
          Viewing <span className="num text-ink-2">{from}</span> to <span className="num text-ink-2">{to}</span> ({formatDuration(zoom.span)})
        </span>
      ) : (
        <span>Drag across a time chart to zoom into a timeframe.</span>
      )}
      {zoom.zoomed ? (
        <button type="button" className="vw-control px-2.5 py-1 text-ink-2 hover:text-ink" onClick={zoom.reset}>
          Reset zoom
        </button>
      ) : null}
    </div>
  );
}
