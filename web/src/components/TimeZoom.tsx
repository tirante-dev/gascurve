"use client";

import { createContext, useCallback, useContext, useMemo, useRef, useState, useSyncExternalStore, type MouseEvent, type ReactNode } from "react";
import { ReferenceArea, type MouseHandlerDataParam } from "recharts";
import { formatDateTime, formatDuration } from "@/utils/format";

export type TimeDomain = readonly [number, number];

type ChartHandler = (state: MouseHandlerDataParam, event: MouseEvent<SVGGraphicsElement>) => void;

type TimeZoomValue = {
  domain: TimeDomain;
  span: number;
  zoomed: boolean;
  mode: "timestamp" | "relative";
  select: (domain: TimeDomain) => void;
  reset: () => void;
};

type DraftStore = {
  getSnapshot: () => TimeDomain | null;
  subscribe: (listener: () => void) => () => void;
  set: (domain: TimeDomain | null) => void;
};

type TimeZoomChartValue = TimeZoomValue & {
  draft: DraftStore;
  handlers: {
    onMouseDown: ChartHandler;
    onMouseMove: ChartHandler;
    onMouseUp: ChartHandler;
    onMouseLeave: ChartHandler;
    onDoubleClick: ChartHandler;
  };
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

function createDraftStore(): DraftStore {
  let domain: TimeDomain | null = null;
  const listeners = new Set<() => void>();
  return {
    getSnapshot: () => domain,
    subscribe: (listener) => {
      listeners.add(listener);
      return () => listeners.delete(listener);
    },
    set: (next) => {
      domain = next;
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
  const drag = useRef<TimeDomain | null>(null);

  const begin = useCallback(
    (state: MouseHandlerDataParam, event: MouseEvent<SVGGraphicsElement>) => {
      if (event.button !== 0 || zoom === null) return;
      const at = valueAt(state);
      if (at === null) return;
      event.preventDefault();
      drag.current = [at, at];
      draft.set([at, at]);
    },
    [draft, zoom],
  );
  const move = useCallback(
    (state: MouseHandlerDataParam, event: MouseEvent<SVGGraphicsElement>) => {
      if (drag.current === null) return;
      const at = valueAt(state);
      if (at === null) return;
      event.preventDefault();
      drag.current = [drag.current[0], at];
      draft.set(ordered(drag.current[0], at));
    },
    [draft],
  );
  const finish = useCallback(
    (state: MouseHandlerDataParam) => {
      const current = drag.current;
      if (current === null || zoom === null) return;
      const at = valueAt(state) ?? current[1];
      drag.current = null;
      draft.set(null);
      zoom.select(ordered(current[0], at));
    },
    [draft, zoom],
  );
  const reset = useCallback(() => {
    drag.current = null;
    draft.set(null);
    zoom?.reset();
  }, [draft, zoom]);
  const handlers = useMemo(
    () => ({ onMouseDown: begin, onMouseMove: move, onMouseUp: finish, onMouseLeave: finish, onDoubleClick: reset }),
    [begin, finish, move, reset],
  );
  return useMemo(() => (zoom === null ? null : { ...zoom, draft, handlers }), [draft, handlers, zoom]);
}

export function TimeZoomSelection({ zoom }: { zoom: TimeZoomChartValue | null }) {
  const draft = useSyncExternalStore((zoom?.draft ?? EMPTY_DRAFT).subscribe, (zoom?.draft ?? EMPTY_DRAFT).getSnapshot, EMPTY_DRAFT.getSnapshot);
  if (!draft || draft[0] === draft[1]) return null;
  return <ReferenceArea x1={draft[0]} x2={draft[1]} fill="var(--accent-2)" fillOpacity={0.16} stroke="var(--accent-2)" strokeOpacity={0.75} ifOverflow="hidden" />;
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
