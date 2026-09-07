import { describe, expect, it } from "vitest";
import {
  CHART_SECTIONS,
  CHART_VIEWS,
  chartBackHref,
  chartHref,
  chartView,
  DEFAULT_HERO_RANGE,
  DEFAULT_SERIES_RANGE,
  DETAIL_FRAME_CLASS,
  getChartView,
  linkRange,
  resolveChartRange,
  resolveConstraint,
  resolveHeroRange,
  resolveSeriesRange,
  type ChartViewId,
} from "./chartViews";

/** Every chart the site draws. A new one has to be added here as well as to the registry. */
const EXPECTED: ChartViewId[] = ["base-fee", "backlog-sawtooth", "contribution", "gas-per-second", "backlogs", "fee-flows", "l1", "taylor"];

describe("the chart registry", () => {
  it("lists every chart exactly once", () => {
    expect(CHART_VIEWS.map((v) => v.id)).toEqual(EXPECTED);
    expect(new Set(CHART_VIEWS.map((v) => v.id)).size).toBe(CHART_VIEWS.length);
  });

  it("gives every chart a back anchor that is a section of the network page", () => {
    for (const view of CHART_VIEWS) {
      expect(CHART_SECTIONS).toContain(view.section);
      expect(chartBackHref("robinhood", view)).toBe(`/robinhood#${view.section}`);
    }
  });

  it("gives every chart a name, a short name and a description, and no em dash anywhere", () => {
    for (const view of CHART_VIEWS) {
      expect(view.title.length).toBeGreaterThan(0);
      expect(view.shortTitle.length).toBeGreaterThan(0);
      expect(view.shortTitle.length).toBeLessThanOrEqual(16);
      expect(view.description.length).toBeGreaterThan(20);
      expect(`${view.title} ${view.shortTitle} ${view.description}`).not.toContain("—");
    }
  });

  it("only asks for a constraint where one chart is drawn per constraint", () => {
    expect(CHART_VIEWS.filter((v) => v.constraint).map((v) => v.id)).toEqual(["backlog-sawtooth", "backlogs"]);
    expect(CHART_VIEWS.filter((v) => v.live).map((v) => v.id)).toEqual(["base-fee", "backlog-sawtooth", "taylor"]);
    expect(CHART_VIEWS.filter((v) => v.range === "series").map((v) => v.id)).toEqual(["contribution", "gas-per-second", "backlogs", "fee-flows", "l1"]);
    expect(CHART_VIEWS.filter((v) => v.range === "hero").map((v) => v.id)).toEqual(["base-fee"]);
  });

  it("looks a chart up by id and knows when there is none", () => {
    expect(getChartView("contribution")?.title).toBe("Contribution to x per constraint");
    expect(getChartView("nonsense")).toBeNull();
    expect(getChartView(undefined)).toBeNull();
    expect(chartView("l1").section).toBe("l1");
    expect(() => chartView("nonsense" as ChartViewId)).toThrow("no chart view nonsense");
  });

  it("sizes an enlarged chart against the viewport, with a floor and a ceiling", () => {
    expect(DETAIL_FRAME_CLASS).toBe("h-[62vh] min-h-[360px] max-h-[720px]");
  });
});

describe("chart hrefs", () => {
  it("carries the range the card is showing", () => {
    expect(chartHref("robinhood", chartView("base-fee"), { range: "live" })).toBe("/robinhood/charts/base-fee?range=live");
    expect(chartHref("robinhood", chartView("base-fee"), { range: "30d" })).toBe("/robinhood/charts/base-fee?range=30d");
    expect(chartHref("robinhood", chartView("contribution"), { range: "1h" })).toBe("/robinhood/charts/contribution?range=1h");
  });

  it("drops a range the target chart cannot draw, so it opens on its own default", () => {
    // Live is the hero's range alone: a history chart opens on 24h instead.
    expect(chartHref("robinhood", chartView("contribution"), { range: "live" })).toBe("/robinhood/charts/contribution");
    expect(chartHref("robinhood", chartView("base-fee"), { range: "nonsense" })).toBe("/robinhood/charts/base-fee");
    expect(chartHref("robinhood", chartView("taylor"), { range: "24h" })).toBe("/robinhood/charts/taylor");
    expect(chartHref("robinhood", chartView("contribution"))).toBe("/robinhood/charts/contribution");
    expect(linkRange(chartView("contribution"), null)).toBeNull();
    expect(linkRange(chartView("base-fee"), "all")).toBe("all");
  });

  it("carries the constraint only where one is drawn at a time", () => {
    expect(chartHref("robinhood", chartView("backlog-sawtooth"), { constraint: 0 })).toBe("/robinhood/charts/backlog-sawtooth?constraint=0");
    expect(chartHref("robinhood", chartView("backlogs"), { range: "24h", constraint: 2 })).toBe("/robinhood/charts/backlogs?range=24h&constraint=2");
    // A chart that draws every constraint at once takes no index, and a bad
    // index is left off rather than passed on.
    expect(chartHref("robinhood", chartView("contribution"), { constraint: 1 })).toBe("/robinhood/charts/contribution");
    expect(chartHref("robinhood", chartView("backlogs"), { constraint: -1 })).toBe("/robinhood/charts/backlogs");
    expect(chartHref("robinhood", chartView("backlogs"), { constraint: 1.5 })).toBe("/robinhood/charts/backlogs");
    expect(chartHref("robinhood", chartView("backlogs"), { constraint: null })).toBe("/robinhood/charts/backlogs");
  });

  it("encodes the network the way every other in-app link does", () => {
    expect(chartHref("robin hood", chartView("taylor"))).toBe("/robin%20hood/charts/taylor");
    expect(chartBackHref("robin hood", chartView("taylor"))).toBe("/robin%20hood#explainer");
  });
});

describe("what a chart's page reads out of its URL", () => {
  it("falls back to Live for the base fee and to 24h for a history chart", () => {
    expect(resolveHeroRange("24h")).toBe("24h");
    expect(resolveHeroRange("live")).toBe("live");
    expect(resolveHeroRange(null)).toBe(DEFAULT_HERO_RANGE);
    expect(resolveHeroRange("nonsense")).toBe("live");
    expect(resolveSeriesRange("1h")).toBe("1h");
    expect(resolveSeriesRange(undefined)).toBe(DEFAULT_SERIES_RANGE);
    // Live is not a range the api serves buckets for.
    expect(resolveSeriesRange("live")).toBe("24h");
    expect(resolveChartRange(chartView("base-fee"), "all")).toBe("all");
    expect(resolveChartRange(chartView("fee-flows"), "live")).toBe("24h");
    expect(resolveChartRange(chartView("taylor"), "24h")).toBeNull();
  });

  it("picks a constraint out of the ones the chart can draw", () => {
    expect(resolveConstraint("1", [0, 1, 2])).toBe(1);
    // A constraint that is not on offer, or none at all, falls back to the first.
    expect(resolveConstraint("9", [0, 1])).toBe(0);
    expect(resolveConstraint(null, [3, 4])).toBe(3);
    expect(resolveConstraint("nonsense", [0])).toBe(0);
    expect(resolveConstraint("", [0])).toBe(0);
    // Nothing to choose from at all: a legacy chain has no short window.
    expect(resolveConstraint("0", [])).toBeNull();
  });
});
