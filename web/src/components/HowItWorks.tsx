"use client";

import Link from "next/link";
import { useCallback, useMemo } from "react";
import { useApi } from "@/hooks/useApi";
import { getConstraints } from "@/lib/api/constraints";
import { getLive } from "@/lib/api/live";
import { listNetworks } from "@/lib/api/networks";
import type { ConstraintSetEntry, LiveSnapshot } from "@/types";
import { shortConstraintLabel } from "@/utils/chart";
import { formatGas, formatGwei, formatUtc } from "@/utils/format";
import { findNetwork } from "@/utils/network";
import { Explainer } from "./Explainer";
import { PageHeader } from "./PageHeader";
import { Card, Label, Section } from "./primitives";
import { TaylorChart } from "./TaylorChart";
import { ThemeToggle } from "./ThemeToggle";

/** The explainer's live values are a quote, not a feed: they are refetched slowly and the page reads without them. */
export const LIVE_REFETCH_MS = 30_000;
export const CONSTRAINTS_REFETCH_MS = 300_000;

/** What the api last said the chain charges nothing below. Null while it has said nothing. */
function floorText(snapshot: LiveSnapshot | null): string | null {
  return snapshot ? `${formatGwei(snapshot.minBaseFee)} gwei` : null;
}

/**
 * The constraint set to quote: the api's current set when it has one,
 * otherwise the definition the live snapshot was priced under, otherwise
 * nothing at all. A legacy chain has no set and says so.
 */
export function quotedConstraints(current: ConstraintSetEntry[] | null, snapshot: LiveSnapshot | null): ConstraintSetEntry[] | null {
  if (current && current.length > 0) return current;
  if (snapshot && snapshot.model === "constraints" && snapshot.constraints.length > 0) {
    return snapshot.constraints.map((c) => ({ target: c.target, window: c.window, startingBacklog: 0 }));
  }
  return null;
}

/** The explainer, network scoped: the prose, this chain's floor and constraint set, and P4 against the exponential. */
export function HowItWorks({ network }: { network: string }) {
  const live = useApi(`${network}:live-quote`, useCallback((signal: AbortSignal) => getLive(network, { signal }), [network]), { refetchMs: LIVE_REFETCH_MS });
  const constraints = useApi(`${network}:constraints`, useCallback((signal: AbortSignal) => getConstraints(network, { signal }), [network]), { refetchMs: CONSTRAINTS_REFETCH_MS });
  const networks = useApi("networks", useCallback((signal: AbortSignal) => listNetworks({ signal }), []), { refetchMs: 300_000 });
  const snapshot = live.data;
  const info = (networks.data ? findNetwork(networks.data, network) : undefined) ?? null;
  const set = constraints.data?.current ?? null;
  const quoted = useMemo(() => quotedConstraints(set?.constraints ?? null, snapshot), [set, snapshot]);
  const legacy = snapshot?.model === "legacy" || info?.model === "legacy";
  const floor = floorText(snapshot);

  return (
    <div className="mx-auto max-w-page px-4 pb-12 sm:px-6">
      <PageHeader name={network} info={info}>
        <Link href={`/${encodeURIComponent(network)}`} className="vw-control px-3 py-1 text-sm text-ink-2 hover:text-ink">
          ← Live view
        </Link>
        <ThemeToggle />
      </PageHeader>

      <main>
        <Section id="explainer" title="How the fee works">
          <div className="grid grid-cols-[minmax(0,1fr)] gap-8 lg:grid-cols-[minmax(0,1.2fr)_minmax(0,1fr)]">
            <Explainer snapshot={snapshot} />
            <div className="flex flex-col gap-6">
              <Card>
                <Label>On {info?.displayName ?? network}</Label>
                <dl className="mt-3 grid gap-3 text-sm">
                  <div>
                    <dt className="text-ink-2">Floor (minimum base fee)</dt>
                    <dd className="num mt-0.5 text-ink">{floor ?? "unavailable right now"}</dd>
                  </div>
                  <div>
                    <dt className="text-ink-2">{legacy ? "Pricer" : "Constraint set in force"}</dt>
                    <dd className="mt-1">
                      {legacy ? (
                        <span className="text-ink">the legacy speed-limit pricer, no constraints configured</span>
                      ) : quoted ? (
                        <ul className="flex flex-wrap gap-1.5">
                          {quoted.map((c, i) => (
                            <li key={`${c.target}-${c.window}-${i}`} className="num rounded bg-surface-2 px-2 py-0.5 text-xs text-ink">
                              {shortConstraintLabel(c)}
                            </li>
                          ))}
                        </ul>
                      ) : (
                        <span className="text-ink-2">unavailable right now</span>
                      )}
                    </dd>
                    {set && !legacy ? (
                      <dd className="num mt-1 text-xs text-ink-3">
                        in force since block {set.effectiveBlock.toLocaleString("en-US")}, {formatUtc(set.effectiveAt)} ({set.source.replace("_", " ")})
                      </dd>
                    ) : null}
                  </div>
                  {!legacy && quoted ? (
                    <div>
                      <dt className="text-ink-2">Gas per unit of x</dt>
                      <dd className="num mt-0.5 text-ink">{quoted.map((c) => formatGas(c.target * c.window)).join(" · ")}</dd>
                    </div>
                  ) : null}
                </dl>
                {live.error || constraints.error ? <p className="mt-3 text-xs text-ink-3">The api did not answer for the live values, so the figures above and in the prose fall back to generic ones.</p> : null}
              </Card>
              <TaylorChart snapshot={snapshot} network={network} />
            </div>
          </div>
        </Section>
      </main>
    </div>
  );
}
