import { MOCK_NETWORKS, type MockNetworkDef } from "./defs";
import { MockWorld } from "./world";

const worlds = new Map<string, MockWorld>();

/** Current unix time in seconds. Tests control it with vi.setSystemTime. */
export function mockNow(): number {
  return Math.floor(Date.now() / 1000);
}

export function findMockDef(network: string): MockNetworkDef | undefined {
  return MOCK_NETWORKS.find((n) => n.name === network || String(n.chainId) === network);
}

/** The world for a network name or chain id, built on first use. */
export function findMockWorld(network: string): MockWorld | undefined {
  const def = findMockDef(network);
  if (!def) return undefined;
  let world = worlds.get(def.name);
  if (!world) {
    world = new MockWorld(def, mockNow());
    worlds.set(def.name, world);
  }
  return world;
}

export function resetMockWorlds(): void {
  worlds.clear();
}
