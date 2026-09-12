import { fireEvent, render, screen } from "@testing-library/react";
import type { MouseEvent } from "react";
import type { MouseHandlerDataParam } from "recharts";
import { describe, expect, it, vi } from "vitest";
import { TimeZoomControls, TimeZoomProvider, useTimeZoomChart } from "./TimeZoom";

function chartState(activeLabel: number): MouseHandlerDataParam {
  return { activeTooltipIndex: 0, activeIndex: 0, activeLabel, activeDataKey: "t", activeCoordinate: { x: 0, y: 0 }, isTooltipActive: true };
}

function chartEvent(button = 0): MouseEvent<SVGGraphicsElement> {
  return { button, preventDefault: vi.fn() } as unknown as MouseEvent<SVGGraphicsElement>;
}

function Driver() {
  const zoom = useTimeZoomChart();
  if (zoom === null) return <span>no zoom</span>;
  return (
    <>
      <output>{zoom.domain.join(":")}</output>
      <button
        type="button"
        onClick={() => {
          zoom.handlers.onMouseDown(chartState(-80), chartEvent());
          zoom.handlers.onMouseMove(chartState(-20), chartEvent());
          zoom.handlers.onMouseUp(chartState(-20), chartEvent());
        }}
      >
        Select
      </button>
      <button type="button" onClick={() => zoom.handlers.onDoubleClick(chartState(0), chartEvent())}>
        Double click
      </button>
    </>
  );
}

describe("TimeZoom", () => {
  it("selects, describes, and resets a timeframe", () => {
    render(
      <TimeZoomProvider domain={[-100, 0]} mode="relative">
        <TimeZoomControls />
        <Driver />
      </TimeZoomProvider>,
    );
    expect(screen.getByText("Drag across a time chart to zoom into a timeframe.")).toBeInTheDocument();
    expect(screen.getByText("-100:0")).toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: "Select" }));
    expect(screen.getByText("-80:-20")).toBeInTheDocument();
    expect(screen.getByText(/Viewing/)).toHaveTextContent("1.3 min ago to 20 s ago (1 min)");

    fireEvent.click(screen.getByRole("button", { name: "Reset zoom" }));
    expect(screen.getByText("-100:0")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Reset zoom" })).toBeNull();
  });

  it("ignores a non-primary drag and supports the chart's double-click reset", () => {
    function Buttons() {
      const zoom = useTimeZoomChart();
      if (zoom === null) return null;
      return (
        <>
          <output>{zoom.domain.join(":")}</output>
          <button
            type="button"
            onClick={() => {
              zoom.handlers.onMouseDown(chartState(10), chartEvent(1));
              zoom.handlers.onMouseMove(chartState(90), chartEvent());
              zoom.handlers.onMouseUp(chartState(90), chartEvent());
            }}
          >
            Wrong button
          </button>
          <button
            type="button"
            onClick={() => {
              zoom.handlers.onMouseDown(chartState(10), chartEvent());
              zoom.handlers.onMouseUp(chartState(90), chartEvent());
            }}
          >
            Zoom
          </button>
          <button type="button" onClick={() => zoom.handlers.onDoubleClick(chartState(0), chartEvent())}>
            Reset gesture
          </button>
        </>
      );
    }

    render(
      <TimeZoomProvider domain={[0, 100]}>
        <Buttons />
      </TimeZoomProvider>,
    );
    fireEvent.click(screen.getByRole("button", { name: "Wrong button" }));
    expect(screen.getByText("0:100")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Zoom" }));
    expect(screen.getByText("10:90")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Reset gesture" }));
    expect(screen.getByText("0:100")).toBeInTheDocument();
  });

  it("keeps pointer-move previews out of shared chart state", () => {
    const renders = vi.fn();
    function PerformanceDriver() {
      const zoom = useTimeZoomChart();
      renders();
      if (zoom === null) return null;
      return (
        <>
          <output>{zoom.domain.join(":")}</output>
          <button
            type="button"
            onClick={() => {
              zoom.handlers.onMouseDown(chartState(20), chartEvent());
              zoom.handlers.onMouseMove(chartState(40), chartEvent());
              zoom.handlers.onMouseMove(chartState(60), chartEvent());
              zoom.handlers.onMouseMove(chartState(80), chartEvent());
            }}
          >
            Preview
          </button>
          <button type="button" onClick={() => zoom.handlers.onMouseUp(chartState(80), chartEvent())}>
            Finish
          </button>
        </>
      );
    }

    render(
      <TimeZoomProvider domain={[0, 100]}>
        <PerformanceDriver />
      </TimeZoomProvider>,
    );
    expect(renders).toHaveBeenCalledTimes(1);
    fireEvent.click(screen.getByRole("button", { name: "Preview" }));
    expect(renders).toHaveBeenCalledTimes(1);
    expect(screen.getByText("0:100")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Finish" }));
    expect(renders).toHaveBeenCalledTimes(2);
    expect(screen.getByText("20:80")).toBeInTheDocument();
  });

  it("does not offer zoom for an empty domain", () => {
    render(
      <TimeZoomProvider domain={null}>
        <TimeZoomControls />
        <Driver />
      </TimeZoomProvider>,
    );
    expect(screen.getByText("no zoom")).toBeInTheDocument();
    expect(screen.queryByText(/Drag across/)).toBeNull();
  });
});
