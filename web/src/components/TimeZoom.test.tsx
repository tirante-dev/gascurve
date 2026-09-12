import { fireEvent, render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { TimeZoomControls, TimeZoomProvider, TimeZoomSurface, useTimeZoomChart } from "./TimeZoom";

function Driver({ renders, onChartMove }: { renders?: () => void; onChartMove?: () => void }) {
  const zoom = useTimeZoomChart();
  renders?.();
  if (zoom === null) return <span>no zoom</span>;
  return (
    <>
      <output>{zoom.domain.join(":")}</output>
      <TimeZoomSurface zoom={zoom}>
        <div data-testid="chart" onMouseMove={onChartMove} />
      </TimeZoomSurface>
    </>
  );
}

function chartSurface(): HTMLDivElement {
  const surface = screen.getByTestId("chart").parentElement;
  if (!(surface instanceof HTMLDivElement)) throw new Error("chart surface missing");
  vi.spyOn(surface, "getBoundingClientRect").mockReturnValue({
    x: 0,
    y: 0,
    left: 0,
    top: 0,
    right: 100,
    bottom: 100,
    width: 100,
    height: 100,
    toJSON: () => ({}),
  });
  return surface;
}

function drag(surface: HTMLDivElement, from: number, to: number, button = 0) {
  fireEvent.pointerDown(surface, { button, pointerId: 1, clientX: from, clientY: 50 });
  fireEvent.pointerMove(surface, { pointerId: 1, clientX: to, clientY: 50 });
  fireEvent.pointerUp(surface, { pointerId: 1, clientX: to, clientY: 50 });
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

    drag(chartSurface(), 20, 80);
    expect(screen.getByText("-80:-20")).toBeInTheDocument();
    expect(screen.getByText(/Viewing/)).toHaveTextContent("1.3 min ago to 20 s ago (1 min)");

    fireEvent.click(screen.getByRole("button", { name: "Reset zoom" }));
    expect(screen.getByText("-100:0")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Reset zoom" })).toBeNull();
  });

  it("ignores a non-primary drag and supports the chart's double-click reset", () => {
    render(
      <TimeZoomProvider domain={[0, 100]}>
        <Driver />
      </TimeZoomProvider>,
    );
    const surface = chartSurface();
    drag(surface, 10, 90, 1);
    expect(screen.getByText("0:100")).toBeInTheDocument();
    drag(surface, 10, 90);
    expect(screen.getByText("10:90")).toBeInTheDocument();
    fireEvent.doubleClick(surface);
    expect(screen.getByText("0:100")).toBeInTheDocument();
  });

  it("keeps pointer-move previews out of chart state", () => {
    const renders = vi.fn();
    const chartMoves = vi.fn();
    render(
      <TimeZoomProvider domain={[0, 100]}>
        <Driver renders={renders} onChartMove={chartMoves} />
      </TimeZoomProvider>,
    );
    const surface = chartSurface();
    expect(renders).toHaveBeenCalledTimes(1);
    fireEvent.pointerDown(surface, { button: 0, pointerId: 1, clientX: 20, clientY: 50 });
    fireEvent.pointerMove(surface, { pointerId: 1, clientX: 40, clientY: 50 });
    fireEvent.pointerMove(surface, { pointerId: 1, clientX: 60, clientY: 50 });
    fireEvent.pointerMove(surface, { pointerId: 1, clientX: 80, clientY: 50 });
    fireEvent.mouseMove(screen.getByTestId("chart"), { clientX: 80, clientY: 50 });
    expect(renders).toHaveBeenCalledTimes(1);
    expect(chartMoves).not.toHaveBeenCalled();
    expect(screen.getByText("0:100")).toBeInTheDocument();
    expect(surface.querySelector("[aria-hidden=true]")).toHaveStyle({ left: "20px", width: "60px" });
    fireEvent.pointerUp(surface, { pointerId: 1, clientX: 80, clientY: 50 });
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
