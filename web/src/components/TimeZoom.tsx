"use client";

import {
  createContext,
  useCallback,
  useContext,
  useMemo,
  useRef,
  useState,
  useSyncExternalStore,
  type MouseEvent,
  type PointerEvent,
  type ReactNode,
} from "react";
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

type ZoomDraft = Pick<PlotRect, "top" | "height"> & {
  left: number;
  width: number;
};

type TimeZoomValue = {
  domain: TimeDomain;
  span: number;
  zoomed: boolean;
  mode: "timestamp" | "relative";
  select: (domain: TimeDomain) => void;
  reset: () => void;
};

type DraftStore = {
  getSnapshot: () => ZoomDraft | null;
  subscribe: (listener: () => void) => () => void;
  set: (draft: ZoomDraft | null) => void;
};

type Drag = {
  start: number;
  end: number;
  plot: PlotRect;
  pointerId: number;
};

type TimeZoomChartValue = TimeZoomValue & {
  draft: DraftStore;
  handlers: {
    onPointerDownCapture: (event: PointerEvent<HTMLDivElement>) => void;
    onPointerMoveCapture: (event: PointerEvent<HTMLDivElement>) => void;
    onPointerUpCapture: (event: PointerEvent<HTMLDivElement>) => void;
    onPointerCancelCapture: (event: PointerEvent<HTMLDivElement>) => void;
    onMouseDownCapture: (event: MouseEvent<HTMLDivElement>) => void;
    onMouseMoveCapture: (event: MouseEvent<HTMLDivElement>) => void;
    onDoubleClickCapture: (event: MouseEvent<HTMLDivElement>) => void;
  };
};

const TimeZoomContext = createContext<TimeZoomValue | null>(null);

function within(domain: TimeDomain, full: TimeDomain): TimeDomain {
  const from = Math.max(full[0], domain[0]);
  const to = Math.min(full[1], domain[1]);
  return to > from ? [from, to] : full;
}

function numberAttribute(element: Element, name: string): number | null {
  const value = Number(element.getAttribute(name));
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

function preview(drag: Drag): ZoomDraft {
  const from = Math.min(drag.start, drag.end);
  const to = Math.max(drag.start, drag.end);
  return { left: drag.plot.left + from * drag.plot.width, top: drag.plot.top, width: (to - from) * drag.plot.width, height: drag.plot.height };
}

function createDraftStore(): DraftStore {
  let current: ZoomDraft | null = null;
  const listeners = new Set<() => void>();
  return {
    getSnapshot: () => current,
    subscribe: (listener) => {
      listeners.add(listener);
      return () => listeners.delete(listener);
    },
    set: (next) => {
      current = next;
      listeners.forEach((listener) => listener());
    },
  };
}

const EMPTY_DRAFT = createDraftStore();

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

export function useTimeZoomChart(): TimeZoomChartValue | null {
  const zoom = useContext(TimeZoomContext);
  const [draft] = useState(createDraftStore);
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
      draft.set(preview(drag.current));
    },
    [draft, zoom],
  );
  const move = useCallback(
    (event: PointerEvent<HTMLDivElement>) => {
      if (drag.current === null || event.pointerId !== drag.current.pointerId) return;
      event.preventDefault();
      event.stopPropagation();
      drag.current.end = fractionAt(event.clientX, drag.current.plot);
      draft.set(preview(drag.current));
    },
    [draft],
  );
  const finish = useCallback(
    (event: PointerEvent<HTMLDivElement>) => {
      const current = drag.current;
      if (current === null || zoom === null || event.pointerId !== current.pointerId) return;
      event.preventDefault();
      event.stopPropagation();
      current.end = fractionAt(event.clientX, current.plot);
      drag.current = null;
      draft.set(null);
      event.currentTarget.releasePointerCapture?.(event.pointerId);
      const from = Math.min(current.start, current.end);
      const to = Math.max(current.start, current.end);
      zoom.select([zoom.domain[0] + from * zoom.span, zoom.domain[0] + to * zoom.span]);
    },
    [draft, zoom],
  );
  const cancel = useCallback(
    (event: PointerEvent<HTMLDivElement>) => {
      if (drag.current === null || event.pointerId !== drag.current.pointerId) return;
      event.preventDefault();
      event.stopPropagation();
      drag.current = null;
      draft.set(null);
      event.currentTarget.releasePointerCapture?.(event.pointerId);
    },
    [draft],
  );
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
      draft.set(null);
      zoom.reset();
    },
    [draft, zoom],
  );
  const handlers = useMemo(
    () => ({
      onPointerDownCapture: begin,
      onPointerMoveCapture: move,
      onPointerUpCapture: finish,
      onPointerCancelCapture: cancel,
      onMouseDownCapture: guardMouse,
      onMouseMoveCapture: guardMouse,
      onDoubleClickCapture: reset,
    }),
    [begin, cancel, finish, guardMouse, move, reset],
  );
  return useMemo(() => (zoom === null ? null : { ...zoom, draft, handlers }), [draft, handlers, zoom]);
}

function TimeZoomSelection({ zoom }: { zoom: TimeZoomChartValue | null }) {
  const store = zoom?.draft ?? EMPTY_DRAFT;
  const draft = useSyncExternalStore(store.subscribe, store.getSnapshot, EMPTY_DRAFT.getSnapshot);
  if (!draft || draft.width === 0) return null;
  return (
    <div
      aria-hidden="true"
      className="pointer-events-none absolute z-10 border-x"
      style={{ left: draft.left, top: draft.top, width: draft.width, height: draft.height, borderColor: "var(--accent-2)", background: "color-mix(in srgb, var(--accent-2) 16%, transparent)" }}
    />
  );
}

export function TimeZoomSurface({ zoom, children }: { zoom: TimeZoomChartValue | null; children: ReactNode }) {
  return (
    <div className={`relative h-full w-full ${zoom ? "cursor-crosshair select-none" : ""}`} {...zoom?.handlers}>
      {children}
      <TimeZoomSelection zoom={zoom} />
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
