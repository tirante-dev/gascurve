import { render, screen, within } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import type { Network, Series } from "@/types";

vi.mock("recharts", async (importOriginal) => {
  const original = await importOriginal<typeof import("recharts")>();
  return { ...original, ResponsiveContainer: ({ children }: { children: React.ReactNode }) => <div style={{ width: 400, height: 100 }}>{children}</div> };
});

const pushMock = vi.fn();
const replaceMock = vi.fn();
let routeParams: { network?: string } = { network: "4663" };
vi.mock("next/navigation", () => ({
  useParams: () => routeParams,
  useRouter: () => ({ push: pushMock, replace: replaceMock }),
}));

const networks: Network[] = [
  { name: "robinhood", displayName: "Robinhood Chain", chainId: 4663, explorerUrl: "https://explorer.example", model: "constraints", headBlock: 10, headAt: null, lagSeconds: null, enabled: true },
  { name: "arbitrum-one", displayName: "Arbitrum One", chainId: 42161, explorerUrl: "", model: "constraints", headBlock: 1, headAt: "2026-09-06T07:20:00Z", lagSeconds: 1, enabled: true },
];

const emptyApi = { data: null, error: null, loading: false, updatedAt: null, refresh: () => undefined };
const refreshMock = vi.fn();
const apiOptions = new Map<string, { refetchMs?: number } | undefined>();
vi.mock("@/hooks/useApi", () => ({
  useApi: (key: string | null, _fetcher: unknown, options?: { refetchMs?: number }) => {
    if (key !== null) apiOptions.set(key, options);
    return key === "networks" ? { ...emptyApi, data: networks } : { ...emptyApi, refresh: refreshMock };
  },
}));
let seriesData: Series | null = null;
vi.mock("@/hooks/useSeries", () => ({ useSeries: () => ({ ...emptyApi, data: seriesData }) }));

let liveInfo: Network | null = null;
let liveReorgs = 0;
vi.mock("@/hooks/useLive", () => ({
  useLive: () => ({ snapshot: null, recentBlocks: [], status: "connecting", networkInfo: liveInfo, ownerActions: [], reorgs: liveReorgs, resyncing: false, error: null }),
}));

import { NetworkPage, OWNER_ACTION_REFETCH_MS } from "./NetworkPage";
import { NetworkSwitcher } from "./NetworkSwitcher";

describe("NetworkPage with a chain-id route", () => {
  beforeEach(() => {
    replaceMock.mockReset();
    pushMock.mockReset();
    routeParams = { network: "4663" };
    liveInfo = null;
    seriesData = null;
    liveReorgs = 0;
    refreshMock.mockReset();
    apiOptions.clear();
  });

  it("revalidates the owner-action list on an interval and again on every reorg", () => {
    routeParams = { network: "robinhood" };
    const { rerender } = render(<NetworkPage network="robinhood" />);
    // A live action received after the initial request is only kept in memory;
    // the persisted list is refreshed on its own so it cannot drift.
    expect(OWNER_ACTION_REFETCH_MS).toBe(300_000);
    expect(apiOptions.get("robinhood:owner-actions")).toEqual({ refetchMs: OWNER_ACTION_REFETCH_MS });
    expect(refreshMock).not.toHaveBeenCalled();
    // A reorg can orphan a persisted action, so the list is revalidated then too.
    liveReorgs = 1;
    rerender(<NetworkPage network="robinhood" />);
    expect(refreshMock).toHaveBeenCalledTimes(1);
    // And only once per reorg.
    rerender(<NetworkPage network="robinhood" />);
    expect(refreshMock).toHaveBeenCalledTimes(1);
    liveReorgs = 2;
    rerender(<NetworkPage network="robinhood" />);
    expect(refreshMock).toHaveBeenCalledTimes(2);
  });

  it("lets every heading stand alone, with no section description under it", () => {
    routeParams = { network: "robinhood" };
    render(<NetworkPage network="robinhood" />);
    for (const title of ["Live", "The pricer, live", "How the fee works", "History", "Fee flows", "L1", "Owner actions"]) {
      expect(screen.getByRole("heading", { name: title, level: 2 })).toBeInTheDocument();
    }
    expect(screen.queryByText(/The base fee right now, the floor it sits on/)).toBeNull();
    expect(screen.queryByText(/One card per constraint/)).toBeNull();
    expect(screen.queryByText(/Base fee, the split of x across constraints/)).toBeNull();
    expect(screen.queryByText(/The floor in force at each block goes to the infra account/)).toBeNull();
    expect(screen.queryByText(/What the chain pays Ethereum/)).toBeNull();
    expect(screen.queryByText(/Parameter changes decoded from OwnerActs logs/)).toBeNull();
  });

  it("keeps a one-paragraph lead-in on the fee section and links to the network's explainer from it and from the header", () => {
    routeParams = { network: "robinhood" };
    render(<NetworkPage network="robinhood" />);
    // The prose itself moved to its own route; what stays is one paragraph and the link.
    expect(screen.queryByRole("heading", { name: "Two parts, one fee" })).toBeNull();
    expect(screen.getByText(/The explainer walks through all of it with Robinhood Chain's own floor/)).toBeInTheDocument();
    const links = screen.getAllByRole("link", { name: "How the fee works →" });
    expect(links).toHaveLength(2);
    for (const link of links) expect(link).toHaveAttribute("href", "/robinhood/how-it-works");
    // The live equation stayed behind, and the P4 chart went with the prose.
    expect(screen.getByText("minBaseFee")).toBeInTheDocument();
    expect(screen.queryByRole("figure", { name: /Degree-4 Taylor polynomial/ })).toBeNull();
  });

  it("treats /4663 as Robinhood and redirects to the canonical name after hello", () => {
    const { rerender } = render(<NetworkPage network="4663" />);
    expect(screen.queryByText(/does not know a network/)).toBeNull();
    expect(screen.getByText("Robinhood Chain · chain 4663")).toBeInTheDocument();
    expect(screen.getByRole("combobox", { name: "Network" })).toHaveValue("robinhood");
    expect(replaceMock).not.toHaveBeenCalled();
    liveInfo = networks[0];
    rerender(<NetworkPage network="4663" />);
    expect(replaceMock).toHaveBeenCalledWith("/robinhood");
    expect(pushMock).not.toHaveBeenCalled();
  });

  it("passes the network's pricer model to the history, so an empty set list is not read as legacy", () => {
    routeParams = { network: "robinhood" };
    seriesData = {
      range: "24h",
      resolution: "1m",
      from: 1,
      to: 61,
      constraintSets: [],
      ownerActions: [],
      points: [{ t: 1, blocks: 1, gasUsed: 1, gasPerSecond: 1, coverage: 1, feesWei: "0", baseFeeMin: "1", baseFeeAvg: "1", baseFeeMax: "1", exponentBips: 34, constraintBips: [34, 0], backlogs: [3_111_506, 0], backlogsMax: [3_111_506, 0], minBaseFee: "1", floorFeesWei: "0", surplusFeesWei: "0", constraintSetId: 0, replayErrorBips: 0 }],
    };
    // The model comes from the api's network list before any hello.
    const { rerender } = render(<NetworkPage network="robinhood" />);
    expect(screen.queryByText("legacy backlog")).toBeNull();
    expect(screen.getByText("C1 · definition unknown")).toBeInTheDocument();
    // A hello for a legacy network switches the labelling.
    liveInfo = { ...networks[0], model: "legacy" };
    rerender(<NetworkPage network="robinhood" />);
    expect(screen.getAllByText("legacy backlog").length).toBeGreaterThan(0);
    expect(replaceMock).not.toHaveBeenCalled();
  });

  it("still flags a name the api does not know", () => {
    routeParams = { network: "mystery" };
    render(<NetworkPage network="mystery" />);
    expect(screen.getByText(/does not know a network/)).toBeInTheDocument();
    expect(replaceMock).not.toHaveBeenCalled();
  });
});

describe("NetworkSwitcher with a chain-id route", () => {
  it("selects the matching network instead of adding a placeholder", () => {
    render(<NetworkSwitcher networks={networks} current="42161" onChange={vi.fn()} loading={false} />);
    const select = screen.getByRole("combobox", { name: "Network" });
    expect(within(select).getAllByRole("option")).toHaveLength(2);
    expect(select).toHaveValue("arbitrum-one");
  });
});
