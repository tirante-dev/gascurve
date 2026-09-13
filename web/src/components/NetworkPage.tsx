"use client";

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useApi } from "@/hooks/useApi";
import { useNetwork } from "@/hooks/useNetwork";
import { useNetworkLive } from "@/hooks/useNetworkLive";
import { useRefreshOnOwnerAction, useSeries } from "@/hooks/useSeries";
import { getOwnerActions } from "@/lib/api/constraints";
import { getStatus, listNetworks } from "@/lib/api/networks";
import { latestConstraintAction } from "@/lib/ownerActions";
import type { LiveSnapshot, Network, NetworkStatus, OwnerAction, PricerModel, SeriesRange, StatusResponse } from "@/types";
import { formatGas, formatGasPerSecond, formatInteger } from "@/utils/format";
import { canonicalNetworkName, findNetwork, isUnknownNetwork, networkPricerModel } from "@/utils/network";
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
import { WhyNow } from "./WhyNow";

/** How often the REST owner-action list is revalidated; the socket carries new ones in between. */
export const OWNER_ACTION_REFETCH_MS = 300_000;

function mergeActions(fetched: OwnerAction[] | null, live: OwnerAction[]): OwnerAction[] | null {
  if (!fetched) return live.length > 0 ? live : null;
  const seen = new Set(fetched.map((a) => `${a.txHash}-${a.block}`));
  const fresh = live.filter((a) => !seen.has(`${a.txHash}-${a.block}`));
  return [...fresh, ...fetched].sort((a, b) => b.block - a.block);
}

export function matchingNetworkStatus(status: StatusResponse | null, snapshot: LiveSnapshot | null, info: Network | null): NetworkStatus | null {
  if (!status) return null;
  const chainId = snapshot?.chainId ?? info?.chainId;
  return status.networks.find((candidate) => chainId !== undefined && candidate.chainId === chainId) ?? status.networks.find((candidate) => candidate.name === info?.name) ?? null;
}

export function ModelSummary({ model, snapshot, network, displayName }: { model: PricerModel; snapshot: Pick<LiveSnapshot, "model" | "legacy"> | null; network: string; displayName: string }) {
  if (model === "legacy") {
    const legacy = snapshot?.model === "legacy" ? snapshot.legacy : undefined;
    return (
      <p>
        The legacy pricer keeps one compute-gas backlog. Each elapsed second drains it at the speed limit. Tolerance multiplied by the speed limit is the floor region; only backlog above that threshold
        adds pressure. Inertia multiplied by the speed limit is the excess backlog that adds 1 to x, then <code>P4(x)</code> multiplies the floor.
        {legacy ? (
          <>
            {" "}For this sample, the speed limit is {formatGasPerSecond(legacy.speedLimit)}, tolerance is {formatInteger(legacy.tolerance)}, inertia is {formatInteger(legacy.inertia)}, and the legacy backlog is {formatGas(legacy.backlog)}.{" "}
          </>
        ) : (
          " The current speed limit, tolerance, inertia, and legacy backlog are unavailable for this sample. "
        )}
        <HowItWorksLink network={network} className="text-accent underline-offset-2 hover:underline" />
      </p>
    );
  }
  if (model === "constraints") {
    return (
      <p>
        The fee is the floor multiplied by <code>P4(x)</code>, and x is the sum over the chain&apos;s constraints of each backlog divided by the product of its target and window. Long windows ratchet on average
        demand, short ones spike on bursts and drain within seconds, and there are no tips to jump the queue. The explainer uses {displayName}&apos;s current floor and constraint set.{" "}
        <HowItWorksLink network={network} className="text-accent underline-offset-2 hover:underline" />
      </p>
    );
  }
  return (
    <p>
      The api has not identified whether this network uses constraints or the legacy speed-limit pricer. gascurve will not apply either model&apos;s explanation until that is known.{" "}
      <HowItWorksLink network={network} className="text-accent underline-offset-2 hover:underline" />
    </p>
  );
}

const SECTION_LINKS = [
  ["why-now", "Why now"],
  ["live", "Live"],
  ["pricer", "Pricer"],
  ["explainer", "Explainer"],
  ["history", "History"],
  ["fees", "Fee flows"],
  ["l1", "L1"],
  ["owner", "Owner actions"],
  ["data-method", "Data & method"],
] as const;

function TimeBasis({ children }: { children: string }) {
  return <span className="text-xs text-ink-3">Times: {children}</span>;
}

/**
 * What the site owes a reader whose pricer is redefined under them: the cards below this describe a
 * set that did not exist a moment ago, and the averages that reached back before it were dropped.
 * Only calls that replace what prices a block raise it, and only from the socket: a change already on
 * screen when the page loaded is history, not news.
 */
export function ParameterChangeNotice({ action }: { action: OwnerAction | null }) {
  if (!action) return null;
  return (
    <div className="vw-card mb-4 p-3 text-sm text-ink-2" role="status">
      Parameters changed at block <span className="num text-ink">{formatInteger(action.block)}</span>. <code>{action.method}</code> replaced what prices a block, so the figures below are the
      new definition and nothing before it is averaged into them.{" "}
      <a className="text-accent underline-offset-2 hover:underline" href="#owner">
        See the owner actions.
      </a>
    </div>
  );
}

export function NetworkPage({ network: routeNetwork }: { network: string }) {
  const { network, setNetwork, replaceNetwork } = useNetwork();
  const name = network || routeNetwork;
  const [range, setRange] = useState<SeriesRange>("24h");
  // One socket and one smoothing loop for the page; a chart's own page takes
  // the same feed from the same hook.
  const { live, smooth, snapshot } = useNetworkLive(name);
  const series = useSeries(name, range);
  const diagnosisSeries = useSeries(name, "24h");
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
  useRefreshOnOwnerAction(live.ownerActions, series.refresh);
  const actions = useMemo(() => mergeActions(ownerActions.data, live.ownerActions), [ownerActions.data, live.ownerActions]);
  // Only the socket's actions: the REST list carries every historical change, and the newest of those
  // is not news about this session.
  const changed = useMemo(() => latestConstraintAction(live.ownerActions), [live.ownerActions]);
  const info = live.networkInfo ?? (networks.data ? findNetwork(networks.data, name) : undefined) ?? null;
  const unknown = isUnknownNetwork(networks.data, name);
  // Which pricer the history belongs to. The series carries no model of its
  // own; an empty constraint-set list must not be read as legacy.
  const model = networkPricerModel(snapshot, info);
  const networkStatus = useMemo(() => matchingNetworkStatus(apiStatus.data, snapshot, info), [apiStatus.data, snapshot, info]);
  const displayName = info?.displayName ?? name;

  // A chain-id route (/4663) is valid; once the server confirms the network, move to its name.
  const canonical = canonicalNetworkName(name, live.networkInfo);
  useEffect(() => {
    if (canonical) replaceNetwork(canonical);
  }, [canonical, replaceNetwork]);

  return (
    <div className="mx-auto max-w-page px-4 pb-12 sm:px-6">
      <a
        href="#main-content"
        onClick={(event) => {
          event.preventDefault();
          const main = document.getElementById("main-content");
          main?.focus();
        }}
        className="fixed left-4 top-2 z-50 -translate-y-16 rounded-md bg-surface px-3 py-2 text-sm font-semibold text-ink shadow-lg transition-transform focus:translate-y-0 motion-reduce:transition-none"
      >
        Skip to content
      </a>
      <PageHeader name={name} info={info}>
        <NetworkSwitcher networks={networks.data} current={name} onChange={setNetwork} loading={networks.loading} />
        <HowItWorksLink network={name} className="vw-control px-3 py-1 text-sm text-ink-2 hover:text-ink" />
        <ThemeToggle />
      </PageHeader>

      <main id="main-content" tabIndex={-1}>
        <div className="mb-4">
          <h1 className="text-2xl font-bold tracking-tight text-ink sm:text-3xl">{displayName} gas pricing</h1>
          <p className="mt-1 text-sm text-ink-2">Current Nitro base-fee pressure and indexed history.</p>
        </div>
        <nav aria-label="Dashboard sections" className="-mx-4 mb-4 overflow-x-auto px-4 pb-1 sm:-mx-6 sm:px-6">
          <ul className="flex w-max gap-2">
            {SECTION_LINKS.map(([id, label]) => (
              <li key={id}>
                <a href={`#${id}`} className="vw-control flex min-h-11 items-center px-3 py-2 text-xs font-medium text-ink-2 hover:text-ink">
                  {label}
                </a>
              </li>
            ))}
          </ul>
        </nav>

        {unknown ? (
          <div className="vw-card mb-6 p-4 text-sm text-ink-2">
            The api does not know a network called <span className="num text-ink">{name}</span>. Pick one from the list above.
          </div>
        ) : null}
        {live.error ? <div className="mb-4 text-xs text-critical">Live data: {live.error}</div> : null}

        <WhyNow
          snapshot={snapshot}
          series={diagnosisSeries.data}
          seriesLoading={diagnosisSeries.loading}
          liveStatus={live.status}
          networkStatus={networkStatus}
          listener={apiStatus.data?.listener}
        />

        <div className="relative isolate">
          <div className="vw-horizon" aria-hidden="true" />
          <Section id="live" title="Live">
            <LiveHero network={name} live={smooth} status={live.status} model={model} ownerActions={live.ownerActions} />
          </Section>
        </div>

        <Section id="pricer" title="The pricer, live">
          <ParameterChangeNotice action={changed} />
          <ConstraintCards network={name} live={smooth} />
          <div className="mt-6">
            <PricerEquation snapshot={snapshot} model={model} />
          </div>
        </Section>

        <Section id="explainer" title="How the fee works">
          <Prose>
            <ModelSummary model={model} snapshot={snapshot} network={name} displayName={displayName} />
          </Prose>
        </Section>

        <Section
          id="history"
          title="History"
          aside={
            <div className="flex flex-wrap items-center justify-end gap-3">
              <TimeBasis>Local</TimeBasis>
              <HistoryTabs range={range} onChange={setRange} loading={series.loading} />
            </div>
          }
        >
          {series.error ? <p className="mb-3 text-sm text-critical">Could not load history: {series.error}</p> : null}
          <SeriesCharts network={name} range={range} series={series.data} loading={series.loading} model={model} />
        </Section>

        <Section id="fees" title="Fee flows" aside={<TimeBasis>Local</TimeBasis>}>
          <FeeFlows network={name} range={range} snapshot={snapshot} series={series.data} explorerUrl={info?.explorerUrl} model={model} />
        </Section>

        <Section id="l1" title="L1" aside={<TimeBasis>Local</TimeBasis>}>
          <L1Section network={name} range={range} snapshot={snapshot} series={series.data} />
        </Section>

        <Section id="owner" title="Owner actions" aside={<TimeBasis>UTC</TimeBasis>}>
          <OwnerActionTimeline actions={actions} explorerUrl={info?.explorerUrl} loading={ownerActions.loading} error={ownerActions.error} />
        </Section>
      </main>

      <div id="data-method" className="scroll-mt-4">
        <DataFooter snapshot={snapshot} series={series.data} networkInfo={info} status={live.status} apiStatus={apiStatus.data} />
      </div>
    </div>
  );
}
