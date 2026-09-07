// Mock api: routes the same paths the Go api serves to the in-memory worlds.
// Only reachable when NEXT_PUBLIC_USE_MOCK_DATA=true (see src/lib/api/core.ts).

import { ApiError, type QueryValue } from "@/lib/api/core";
import { isSeriesRange } from "@/lib/api/series";
import type { SeriesRange, StatusResponse } from "@/types";
import { MOCK_NETWORKS } from "./defs";
import { findMockWorld, mockNow } from "./registry";

export { MockWebSocket } from "./socket";
export { findMockWorld, findMockDef, mockNow, resetMockWorlds } from "./registry";
export { MockWorld } from "./world";
export { MOCK_NETWORKS } from "./defs";

export const MOCK_VERSION = "0.0.0-mock";

function rangeFrom(query: Record<string, QueryValue> | undefined): SeriesRange {
  const raw = query?.range;
  if (raw === undefined) return "1h";
  const value = String(raw);
  if (!isSeriesRange(value)) throw new ApiError(400, "bad_request", `unknown range ${value}`);
  return value;
}

function notFound(what: string): never {
  throw new ApiError(404, "not_found", `${what} not found`);
}

/** Resolves a request against the mock world. Paths are relative to /api/v1. */
export async function mockRequest<T>(path: string, query?: Record<string, QueryValue>): Promise<T> {
  const clean = path.replace(/^\/+/, "").replace(/\/+$/, "");
  const now = mockNow();
  const parts = clean.split("/");

  if (clean === "health" || clean === "ready") return { ok: true } as T;
  if (clean === "networks") {
    return MOCK_NETWORKS.map((def) => {
      const world = findMockWorld(def.name);
      if (!world) return notFound(def.name);
      world.advanceTo(now);
      return world.network(now);
    }) as T;
  }
  if (clean === "status") {
    const response: StatusResponse = {
      version: MOCK_VERSION,
      status: "healthy",
      networks: MOCK_NETWORKS.map((def) => {
        const world = findMockWorld(def.name);
        if (!world) return notFound(def.name);
        world.advanceTo(now);
        return world.status(now);
      }),
    };
    return response as T;
  }
  if (parts[0] !== "networks" || parts.length < 2 || parts.length > 3) return notFound(path);
  const world = findMockWorld(decodeURIComponent(parts[1]));
  if (!world) return notFound(`network ${parts[1]}`);
  world.advanceTo(now);
  const resource = parts[2];
  switch (resource) {
    case undefined:
      return world.network(now) as T;
    case "live":
      return world.snapshot(now) as T;
    case "blocks": {
      const raw = Number(query?.limit ?? 120);
      const limit = Number.isFinite(raw) && raw > 0 ? Math.min(1000, Math.floor(raw)) : 120;
      return world.recentBlocks(limit).slice().reverse() as T;
    }
    case "series":
      return world.series(rangeFrom(query), now) as T;
    case "constraints":
      return world.constraintsResponse() as T;
    case "owner-actions":
      return world.ownerActions() as T;
    case "batches":
      return world.batches(rangeFrom(query), now) as T;
    case "l1":
      return world.l1Series(rangeFrom(query), now) as T;
    default:
      return notFound(path);
  }
}
