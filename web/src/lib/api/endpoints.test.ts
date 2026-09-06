import { afterEach, describe, expect, it, vi } from "vitest";
import { getBatches } from "./batches";
import { getConstraints, getOwnerActions } from "./constraints";
import { getL1 } from "./l1";
import { getBlocks, getLive } from "./live";
import { getNetwork, getStatus, listNetworks } from "./networks";
import { getSeries, isSeriesRange, SERIES_RANGES } from "./series";

function jsonFetch(body: unknown) {
  return vi.fn(async () => new Response(JSON.stringify(body), { status: 200 }));
}

function calledUrl(fetchImpl: ReturnType<typeof jsonFetch>): string {
  return (fetchImpl.mock.calls[0] as unknown as [string])[0];
}

describe("endpoint modules", () => {
  afterEach(() => vi.unstubAllEnvs());

  it("call the documented paths", async () => {
    const fetchImpl = jsonFetch([]);
    await listNetworks({ fetchImpl });
    expect(calledUrl(fetchImpl)).toBe("http://localhost:8080/api/v1/networks");

    const f2 = jsonFetch({});
    await getNetwork("robinhood", { fetchImpl: f2 });
    expect(calledUrl(f2)).toBe("http://localhost:8080/api/v1/networks/robinhood");

    const f3 = jsonFetch({});
    await getLive("4663", { fetchImpl: f3 });
    expect(calledUrl(f3)).toBe("http://localhost:8080/api/v1/networks/4663/live");

    const f4 = jsonFetch([]);
    await getBlocks("robinhood", 50, { fetchImpl: f4 });
    expect(calledUrl(f4)).toBe("http://localhost:8080/api/v1/networks/robinhood/blocks?limit=50");

    const f5 = jsonFetch([]);
    await getBlocks("robinhood", undefined, { fetchImpl: f5 });
    expect(calledUrl(f5)).toBe("http://localhost:8080/api/v1/networks/robinhood/blocks?limit=120");

    const f6 = jsonFetch({});
    await getSeries("robinhood", "24h", { fetchImpl: f6 });
    expect(calledUrl(f6)).toBe("http://localhost:8080/api/v1/networks/robinhood/series?range=24h");

    const f7 = jsonFetch({});
    await getConstraints("robinhood", { fetchImpl: f7 });
    expect(calledUrl(f7)).toBe("http://localhost:8080/api/v1/networks/robinhood/constraints");

    const f8 = jsonFetch([]);
    await getOwnerActions("robinhood", { fetchImpl: f8 });
    expect(calledUrl(f8)).toBe("http://localhost:8080/api/v1/networks/robinhood/owner-actions");

    const f9 = jsonFetch({});
    await getBatches("robinhood", "30d", { fetchImpl: f9 });
    expect(calledUrl(f9)).toBe("http://localhost:8080/api/v1/networks/robinhood/batches?range=30d");

    const f10 = jsonFetch({});
    await getL1("robinhood", "all", { fetchImpl: f10 });
    expect(calledUrl(f10)).toBe("http://localhost:8080/api/v1/networks/robinhood/l1?range=all");

    const f11 = jsonFetch({});
    await getStatus({ fetchImpl: f11 });
    expect(calledUrl(f11)).toBe("http://localhost:8080/api/v1/status");
  });

  it("validates ranges", () => {
    expect(SERIES_RANGES).toEqual(["1h", "24h", "30d", "all"]);
    expect(isSeriesRange("1h")).toBe(true);
    expect(isSeriesRange("2h")).toBe(false);
  });
});
