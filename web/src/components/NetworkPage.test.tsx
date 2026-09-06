import { render, screen, within } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import type { Network } from "@/types";

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
vi.mock("@/hooks/useApi", () => ({
  useApi: (key: string | null) => (key === "networks" ? { ...emptyApi, data: networks } : emptyApi),
}));
vi.mock("@/hooks/useSeries", () => ({ useSeries: () => emptyApi }));

let liveInfo: Network | null = null;
vi.mock("@/hooks/useLive", () => ({
  useLive: () => ({ snapshot: null, recentBlocks: [], status: "connecting", networkInfo: liveInfo, ownerActions: [], error: null }),
}));

import { NetworkPage } from "./NetworkPage";
import { NetworkSwitcher } from "./NetworkSwitcher";

describe("NetworkPage with a chain-id route", () => {
  beforeEach(() => {
    replaceMock.mockReset();
    pushMock.mockReset();
    routeParams = { network: "4663" };
    liveInfo = null;
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
