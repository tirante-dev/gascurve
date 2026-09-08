"use client";

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useApi } from "@/hooks/useApi";
import { useNetwork } from "@/hooks/useNetwork";
import { useNetworkLive } from "@/hooks/useNetworkLive";
import { useSeries } from "@/hooks/useSeries";
import { getOwnerActions } from "@/lib/api/constraints";
import { getStatus, listNetworks } from "@/lib/api/networks";
import type { OwnerAction, SeriesRange } from "@/types";
import { canonicalNetworkName, findNetwork, isUnknownNetwork } from "@/utils/network";
import { ConstraintCards } from "./ConstraintCards";
import { DataFooter } from "./DataFooter";
import { FeeFlows } from "./FeeFlows";
import { HistoryTabs } from "./HistoryTabs";
import { L1Section } from "./L1Section";
import { LiveHero } from "./LiveHero";
import { NetworkSwitcher } from "./NetworkSwitcher";
import { OwnerActionTimeline } from "./OwnerActionTimeline";
import { HowItWorksLink, PageHeader } from "./PageHeader";
import { PricerEquation } from "./PricerEquation";
import { Prose, Section } from "./primitives";
import { SeriesCharts } from "./SeriesCharts";
import { ThemeToggle } from "./ThemeToggle";

/** How often the REST owner-action list is revalidated; the socket carries new ones in between. */
export const OWNER_ACTION_REFETCH_MS = 300_000;

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
  // One socket and one smoothing loop for the page; a chart's own page takes
  // the same feed from the same hook.
  const { live, smooth, snapshot } = useNetworkLive(name);
  const series = useSeries(name, range);
  const networks = useApi("networks", useCallback((signal: AbortSignal) => listNetworks({ signal }), []), { refetchMs: 300_000 });
  const apiStatus = useApi("status", useCallback((signal: AbortSignal) => getStatus({ signal }), []), { refetchMs: 60_000 });
  // Owner actions are pushed over the socket as they happen; the REST list is
  // the record that survives a reconnect. It is revalidated every five minutes
  // and again on every reorg, so a persisted action on an orphaned block is
  // replaced rather than left on screen.
  const ownerActions = useApi(`${name}:owner-actions`, useCallback((signal: AbortSignal) => getOwnerActions(name, { signal }), [name]), { refetchMs: OWNER_ACTION_REFETCH_MS });
  const refreshOwnerActions = ownerActions.refresh;
  const seenReorgs = useRef(live.reorgs);
  useEffect(() => {
    if (live.reorgs === seenReorgs.current) return;
    seenReorgs.current = live.reorgs;
    refreshOwnerActions();
  }, [live.reorgs, refreshOwnerActions]);
  const actions = useMemo(() => mergeActions(ownerActions.data, live.ownerActions), [ownerActions.data, live.ownerActions]);
  const info = live.networkInfo ?? (networks.data ? findNetwork(networks.data, name) : undefined) ?? null;
  const unknown = isUnknownNetwork(networks.data, name);
  // Which pricer the history belongs to. The series carries no model of its
  // own; an empty constraint-set list must not be read as legacy.
  const model = info?.model ?? snapshot?.model ?? "unknown";

  // A chain-id route (/4663) is valid; once the server confirms the network, move to its name.
  const canonical = canonicalNetworkName(name, live.networkInfo);
  useEffect(() => {
    if (canonical) replaceNetwork(canonical);
  }, [canonical, replaceNetwork]);

  return (
    <div className="mx-auto max-w-[1200px] px-4 pb-12 sm:px-6">
      <PageHeader name={name} info={info}>
        <NetworkSwitcher networks={networks.data} current={name} onChange={setNetwork} loading={networks.loading} />
        <HowItWorksLink network={name} className="vw-control px-3 py-1 text-sm text-ink-2 hover:text-ink" />
        <ThemeToggle />
      </PageHeader>

      {unknown ? (
        <div className="vw-card mb-6 p-4 text-sm text-ink-2">
          The api does not know a network called <span className="num text-ink">{name}</span>. Pick one from the list above.
        </div>
      ) : null}
      {live.error ? <div className="mb-4 text-xs text-critical">Live data: {live.error}</div> : null}

      <main>
        <div className="relative isolate">
          <div className="vw-horizon" aria-hidden="true" />
          <Section id="live" title="Live">
            <LiveHero network={name} live={smooth} status={live.status} model={model} />
          </Section>
        </div>

        <Section id="pricer" title="The pricer, live">
          <ConstraintCards network={name} live={smooth} />
          <div className="mt-6">
            <PricerEquation snapshot={snapshot} />
          </div>
        </Section>

        <Section id="explainer" title="How the fee works">
          <Prose>
            <p>
              The fee is the floor multiplied by <code>P4(x)</code>, and x is the sum over the chain&apos;s constraints of each backlog divided by its target times its window. Long windows
              ratchet on the average demand, short ones spike on bursts and drain within seconds, and there are no tips to jump the queue. The explainer walks through all of it with{" "}
              {info ? info.displayName : name}&apos;s own floor and constraint set.{" "}
              <HowItWorksLink network={name} className="text-accent underline-offset-2 hover:underline" />
            </p>
          </Prose>
        </Section>

        <Section
          id="history"
          title="History"
          aside={<HistoryTabs range={range} onChange={setRange} loading={series.loading} />}
        >
          {series.error ? <p className="mb-3 text-sm text-critical">Could not load history: {series.error}</p> : null}
          <SeriesCharts network={name} range={range} series={series.data} loading={series.loading} model={model} />
        </Section>

        <Section id="fees" title="Fee flows">
          <FeeFlows network={name} range={range} snapshot={snapshot} series={series.data} explorerUrl={info?.explorerUrl} model={model} />
        </Section>

        <Section id="l1" title="L1">
          <L1Section network={name} range={range} snapshot={snapshot} series={series.data} />
        </Section>

        <Section id="owner" title="Owner actions">
          <OwnerActionTimeline actions={actions} explorerUrl={info?.explorerUrl} loading={ownerActions.loading} error={ownerActions.error} />
        </Section>
      </main>

      <DataFooter snapshot={snapshot} series={series.data} networkInfo={info} status={live.status} apiStatus={apiStatus.data} />
    </div>
  );
}
