import type { Metadata } from "next";
import Link from "next/link";
import { NetworkRedirect } from "@/components/NetworkRedirect";
import { PRIMARY_NETWORK, PRIMARY_NETWORK_NAME, SITE_NETWORKS, SITE_TAGLINE } from "@/lib/seo";

export const metadata: Metadata = { alternates: { canonical: "/" } };

/**
 * The root sends viewers to their last network, or to Robinhood Chain. The
 * markup below is what is served before that happens, so it is written for
 * the two readers who see it: a crawler, which follows these links rather
 * than the client-side redirect, and a viewer whose route transition is slow.
 * Robinhood Chain leads, and the rest are named as the comparison they are.
 */
export default function IndexPage() {
  const others = SITE_NETWORKS.filter((n) => !n.primary);
  return (
    <main className="mx-auto max-w-page px-4 py-16">
      <NetworkRedirect />
      <h1 className="vw-wordmark text-2xl sm:text-3xl">gascurve</h1>
      <p className="mt-2 text-[11px] uppercase tracking-[0.18em] text-label">{SITE_TAGLINE}</p>
      <p className="mt-6 max-w-[70ch] text-ink-2">
        Live and historical gas prices for {PRIMARY_NETWORK_NAME}: the multi-constraint base fee pricer, its backlogs, owner parameter changes, fee destinations and ArbOS-attributed batch costs.
      </p>
      <p className="mt-6">
        <Link href={`/${PRIMARY_NETWORK}`} className="text-accent-2-text underline">
          {PRIMARY_NETWORK_NAME} gas tracker →
        </Link>
      </p>
      <nav aria-label="Other chains" className="mt-10">
        <h2 className="text-sm uppercase tracking-[0.14em] text-ink-3">Also covered, for comparison</h2>
        <ul className="mt-3 flex flex-col gap-2">
          {others.map((network) => (
            <li key={network.name}>
              <Link href={`/${network.name}`} className="text-ink-2 underline">
                {network.displayName} gas prices
              </Link>
            </li>
          ))}
        </ul>
      </nav>
      <p className="mt-10 text-sm text-ink-3">
        Opening <Link className="underline" href={`/${PRIMARY_NETWORK}`}>/{PRIMARY_NETWORK}</Link>.
      </p>
    </main>
  );
}
