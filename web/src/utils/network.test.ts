import { describe, expect, it } from "vitest";
import { canonicalNetworkName, findNetwork, isChainIdParam, isUnknownNetwork, networkLabel } from "./network";

const networks = [
  { name: "robinhood", chainId: 4663 },
  { name: "arbitrum-one", chainId: 42161 },
];

describe("network route helpers", () => {
  it("finds a network by name or by chain id", () => {
    expect(findNetwork(networks, "robinhood")?.chainId).toBe(4663);
    expect(findNetwork(networks, "4663")?.name).toBe("robinhood");
    expect(findNetwork(networks, "42161")?.name).toBe("arbitrum-one");
    expect(findNetwork(networks, "nope")).toBeUndefined();
  });
  it("treats a chain-id route as known, and an unlisted name as unknown only once the list is loaded", () => {
    expect(isUnknownNetwork(networks, "4663")).toBe(false);
    expect(isUnknownNetwork(networks, "robinhood")).toBe(false);
    expect(isUnknownNetwork(networks, "nope")).toBe(true);
    expect(isUnknownNetwork(null, "nope")).toBe(false);
  });
  it("redirects a chain-id route to the canonical name once the server confirms it", () => {
    expect(canonicalNetworkName("4663", { name: "robinhood", chainId: 4663 })).toBe("robinhood");
    expect(canonicalNetworkName("robinhood", { name: "robinhood", chainId: 4663 })).toBeNull();
    expect(canonicalNetworkName("4663", null)).toBeNull();
    // A hello for another network never redirects.
    expect(canonicalNetworkName("4663", { name: "arbitrum-one", chainId: 42161 })).toBeNull();
  });
});

describe("networkLabel", () => {
  it("title cases a slug, so a server rendered title reads as a name", () => {
    expect(networkLabel("robinhood")).toBe("Robinhood");
    expect(networkLabel("arbitrum-one")).toBe("Arbitrum One");
    expect(networkLabel("robinhood-testnet")).toBe("Robinhood Testnet");
    expect(networkLabel("arbitrum-sepolia")).toBe("Arbitrum Sepolia");
  });
  it("names a chain-id route after the id rather than guessing at a name", () => {
    expect(networkLabel("4663")).toBe("Chain 4663");
    expect(networkLabel("42161")).toBe("Chain 42161");
  });
  it("survives the shapes a route parameter can arrive in", () => {
    expect(networkLabel("")).toBe("");
    expect(networkLabel("-a--b-")).toBe("A B");
  });
  it("tells the two route forms apart", () => {
    expect(isChainIdParam("4663")).toBe(true);
    expect(isChainIdParam("robinhood")).toBe(false);
    expect(isChainIdParam("arbitrum-one")).toBe(false);
    expect(isChainIdParam("")).toBe(false);
  });
});
