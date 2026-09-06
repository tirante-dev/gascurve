"use client";

import Link from "next/link";
import { useCallback, useEffect, useMemo, useState } from "react";
import { useApi } from "@/hooks/useApi";
import { useLive } from "@/hooks/useLive";
import { useNetwork } from "@/hooks/useNetwork";
import { useSeries } from "@/hooks/useSeries";
import { useTicker } from "@/hooks/useTicker";
import { getOwnerActions } from "@/lib/api/constraints";
import { getStatus, listNetworks } from "@/lib/api/networks";
import type { OwnerAction, SeriesRange } from "@/types";
import { canonicalNetworkName, findNetwork, isUnknownNetwork } from "@/utils/network";
import { ConstraintCards } from "./ConstraintCards";
import { DataFooter } from "./DataFooter";
import { Explainer } from "./Explainer";
import { FeeFlows } from "./FeeFlows";
import { HistoryTabs } from "./HistoryTabs";
import { L1Section } from "./L1Section";
import { LiveStrip } from "./LiveStrip";
import { NetworkSwitcher } from "./NetworkSwitcher";
import { OwnerActionTimeline } from "./OwnerActionTimeline";
import { PricerEquation } from "./PricerEquation";
import { Section } from "./primitives";
import { SeriesCharts } from "./SeriesCharts";
import { ThemeToggle } from "./ThemeToggle";

function mergeActions(fetched: OwnerAction[] | null, live: OwnerAction[]): OwnerAction[] | null {
  if (!fetched) return live.length > 0 ? live : null;
  const seen = new Set(fetched.map((a) => `${a.txHash}-${a.block}`));
  const fresh = live.filter((a) => !seen.has(`${a.txHash}-${a.block}`));
  return [...fresh, ...fetched].sort((a, b) => b.block - a.block);
}

export function NetworkPage({ network: routeNetwork }: { network: string }) {
  const { network, setNetwork, replaceNetwork } = useNetwork();
  const name = network || routeNetwork;
  const [range, setRange] = useState<SeriesRange>("24h");
  const live = useLive(name);
  const series = useSeries(name, range);
  const networks = useApi("networks", useCallback((signal: AbortSignal) => listNetworks({ signal }), []), { refetchMs: 300_000 });
  const apiStatus = useApi("status", useCallback((signal: AbortSignal) => getStatus({ signal }), []), { refetchMs: 60_000 });
  const ownerActions = useApi(`${name}:owner-actions`, useCallback((signal: AbortSignal) => getOwnerActions(name, { signal }), [name]));
  const now = useTicker(1000);
  const actions = useMemo(() => mergeActions(ownerActions.data, live.ownerActions), [ownerActions.data, live.ownerActions]);
  const info = live.networkInfo ?? (networks.data ? findNetwork(networks.data, name) : undefined) ?? null;
  const unknown = isUnknownNetwork(networks.data, name);

  // A chain-id route (/4663) is valid; once the server confirms the network, move to its name.
  const canonical = canonicalNetworkName(name, live.networkInfo);
  useEffect(() => {
    if (canonical) replaceNetwork(canonical);
  }, [canonical, replaceNetwork]);

  return (
    <div className="mx-auto max-w-[1200px] px-4 pb-12 sm:px-6">
      <header className="flex flex-wrap items-center justify-between gap-3 py-4">
        <div className="flex items-baseline gap-3">
          <Link href="/" className="text-base font-semibold tracking-tight text-ink">
            gascurve
          </Link>
          <span className="text-sm text-ink-3">{info ? `${info.displayName} · chain ${info.chainId}` : name}</span>
        </div>
        <div className="flex items-center gap-2">
          <NetworkSwitcher networks={networks.data} current={name} onChange={setNetwork} loading={networks.loading} />
          <ThemeToggle />
        </div>
      </header>

      {unknown ? (
        <div className="mb-6 rounded-md border border-hairline bg-surface p-4 text-sm text-ink-2">
          The api does not know a network called <span className="num text-ink">{name}</span>. Pick one from the list above.
        </div>
      ) : null}
      {live.error ? <div className="mb-4 text-xs text-critical">Live data: {live.error}</div> : null}

      <main>
        <Section id="live" title="Live" lede="The base fee right now, the floor it sits on, and the last two minutes of blocks.">
          <LiveStrip snapshot={live.snapshot} recentBlocks={live.recentBlocks} status={live.status} />
        </Section>

        <Section id="pricer" title="The pricer, live" lede="One card per constraint. Backlogs drain at the target rate between samples and snap on every tick.">
          <ConstraintCards snapshot={live.snapshot} />
          <div className="mt-6">
            <PricerEquation snapshot={live.snapshot} />
          </div>
        </Section>

        <Section id="explainer" title="How the fee works">
          <Explainer snapshot={live.snapshot} />
        </Section>

        <Section
          id="history"
          title="History"
          lede="Base fee, the split of x across constraints, gas per second against targets, and the backlogs. Hover or use the point inspector for exact values; owner actions are marked and each constraint set is drawn as its own series."
          aside={<HistoryTabs range={range} onChange={setRange} loading={series.loading} />}
        >
          {series.error ? <p className="mb-3 text-sm text-critical">Could not load history: {series.error}</p> : null}
          <SeriesCharts series={series.data} loading={series.loading} />
        </Section>

        <Section id="fees" title="Fee flows" lede="The floor in force at each block goes to the infra account, everything above it to the network account.">
          <FeeFlows snapshot={live.snapshot} series={series.data} explorerUrl={info?.explorerUrl} />
        </Section>

        <Section id="l1" title="L1" lede="What the chain pays Ethereum, and the pricer that recovers it.">
          <L1Section network={name} range={range} snapshot={live.snapshot} series={series.data} />
        </Section>

        <Section id="owner" title="Owner actions" lede="Parameter changes decoded from OwnerActs logs. A constraint change replaces the backlogs with the starting values it carries.">
          <OwnerActionTimeline actions={actions} explorerUrl={info?.explorerUrl} loading={ownerActions.loading} error={ownerActions.error} />
        </Section>
      </main>

      <DataFooter snapshot={live.snapshot} series={series.data} networkInfo={info} status={live.status} apiStatus={apiStatus.data} now={now} />
    </div>
  );
}
