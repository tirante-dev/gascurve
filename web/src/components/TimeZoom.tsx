"use client";

import { createContext, useCallback, useContext, useMemo, useRef, useState, type MouseEvent, type ReactNode } from "react";
import { ReferenceArea, type MouseHandlerDataParam } from "recharts";
import { formatDateTime, formatDuration } from "@/utils/format";

export type TimeDomain = readonly [number, number];

type ChartHandler = (state: MouseHandlerDataParam, event: MouseEvent<SVGGraphicsElement>) => void;

type TimeZoomValue = {
  domain: TimeDomain;
  span: number;
  zoomed: boolean;
  draft: TimeDomain | null;
  mode: "timestamp" | "relative";
  handlers: {
    onMouseDown: ChartHandler;
    onMouseMove: ChartHandler;
    onMouseUp: ChartHandler;
    onMouseLeave: ChartHandler;
    onDoubleClick: ChartHandler;
  };
  reset: () => void;
};

const TimeZoomContext = createContext<TimeZoomValue | null>(null);

function ordered(a: number, b: number): TimeDomain {
  return a <= b ? [a, b] : [b, a];
}

function valueAt(state: MouseHandlerDataParam): number | null {
  const value = typeof state.activeLabel === "number" ? state.activeLabel : Number(state.activeLabel);
  return Number.isFinite(value) ? value : null;
}

function within(domain: TimeDomain, full: TimeDomain): TimeDomain {
  const from = Math.max(full[0], domain[0]);
  const to = Math.min(full[1], domain[1]);
  return to > from ? [from, to] : full;
}

export function TimeZoomProvider({ domain, mode = "timestamp", children }: { domain: TimeDomain | null; mode?: "timestamp" | "relative"; children: ReactNode }) {
  const [selected, setSelected] = useState<TimeDomain | null>(null);
  const [draft, setDraft] = useState<TimeDomain | null>(null);
  const drag = useRef<TimeDomain | null>(null);
  const valid = domain !== null && Number.isFinite(domain[0]) && Number.isFinite(domain[1]) && domain[1] > domain[0];
  const fullFrom = valid ? domain[0] : 0;
  const fullTo = valid ? domain[1] : 1;
  const full = useMemo<TimeDomain>(() => [fullFrom, fullTo], [fullFrom, fullTo]);
  const visible = selected === null ? full : within(selected, full);

  const reset = useCallback(() => {
    drag.current = null;
    setDraft(null);
    setSelected(null);
  }, []);

  const begin = useCallback((state: MouseHandlerDataParam, event: MouseEvent<SVGGraphicsElement>) => {
    if (event.button !== 0) return;
    const at = valueAt(state);
    if (at === null) return;
    event.preventDefault();
    drag.current = [at, at];
    setDraft([at, at]);
  }, []);

  const move = useCallback((state: MouseHandlerDataParam, event: MouseEvent<SVGGraphicsElement>) => {
    if (drag.current === null) return;
    const at = valueAt(state);
    if (at === null) return;
    event.preventDefault();
    drag.current = [drag.current[0], at];
    setDraft(ordered(drag.current[0], at));
  }, []);

  const finish = useCallback(
    (state: MouseHandlerDataParam) => {
      const current = drag.current;
      if (current === null) return;
      const at = valueAt(state) ?? current[1];
      const next = within(ordered(current[0], at), visible);
      drag.current = null;
      setDraft(null);
      if (next[1] - next[0] >= (visible[1] - visible[0]) / 500) setSelected(next);
    },
    [visible],
  );

  const handlers = useMemo(
    () => ({
      onMouseDown: begin,
      onMouseMove: move,
      onMouseUp: finish,
      onMouseLeave: finish,
      onDoubleClick: reset,
    }),
    [begin, finish, move, reset],
  );
  const value = useMemo<TimeZoomValue>(
    () => ({ domain: visible, span: visible[1] - visible[0], zoomed: visible[0] > full[0] || visible[1] < full[1], draft, mode, handlers, reset }),
    [draft, full, handlers, mode, reset, visible],
  );

  if (!valid) return children;
  return <TimeZoomContext value={value}>{children}</TimeZoomContext>;
}

export function useTimeZoomChart(): TimeZoomValue | null {
  return useContext(TimeZoomContext);
}

export function TimeZoomSelection() {
  const draft = useTimeZoomChart()?.draft;
  if (!draft || draft[0] === draft[1]) return null;
  return <ReferenceArea x1={draft[0]} x2={draft[1]} fill="var(--accent-2)" fillOpacity={0.16} stroke="var(--accent-2)" strokeOpacity={0.75} ifOverflow="hidden" />;
}

function relativeTime(value: number): string {
  return value >= 0 ? "now" : `${formatDuration(Math.abs(value))} ago`;
}

export function TimeZoomControls({ className = "" }: { className?: string }) {
  const zoom = useTimeZoomChart();
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
