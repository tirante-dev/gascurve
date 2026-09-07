import { describe, expect, it, vi } from "vitest";
import { getBatches } from "./batches";
import { getL1 } from "./l1";
import { isAscendingByTime, orderPoints } from "./order";
import { getSeries } from "./series";

function jsonFetch(body: unknown) {
  return vi.fn(async () => new Response(JSON.stringify(body), { status: 200 }));
}

describe("point order at the client boundary", () => {
  it("recognises an order that already ascends, and hands that series back unchanged", () => {
    expect(isAscendingByTime([])).toBe(true);
    expect(isAscendingByTime([{ t: 1 }, { t: 1 }, { t: 2 }])).toBe(true);
    expect(isAscendingByTime([{ t: 2 }, { t: 1 }])).toBe(false);
    const series = { points: [{ t: 1 }, { t: 2 }] };
    expect(orderPoints(series)).toBe(series);
  });
  it("sorts a series whose points arrived out of order, keeping ties in the order they came", () => {
    const out = orderPoints({ points: [{ t: 30, id: "c" }, { t: 10, id: "a" }, { t: 20, id: "b1" }, { t: 20, id: "b2" }] });
    expect(out.points.map((p) => p.id)).toEqual(["a", "b1", "b2", "c"]);
  });
  it("leaves a payload with no points alone", () => {
    const empty = { range: "24h" };
    expect(orderPoints(empty as unknown as { points: { t: number }[] })).toBe(empty);
  });
  it("normalises every bucketed endpoint, so no chart has to sort defensively", async () => {
    const series = await getSeries("robinhood", "24h", { fetchImpl: jsonFetch({ points: [{ t: 120 }, { t: 60 }] }) });
    expect(series.points.map((p) => p.t)).toEqual([60, 120]);
    const batches = await getBatches("robinhood", "24h", { fetchImpl: jsonFetch({ points: [{ t: 9 }, { t: 3 }] }) });
    expect(batches.points.map((p) => p.t)).toEqual([3, 9]);
    const l1 = await getL1("robinhood", "24h", { fetchImpl: jsonFetch({ points: [{ t: 9 }, { t: 3 }] }) });
    expect(l1.points.map((p) => p.t)).toEqual([3, 9]);
  });
});
