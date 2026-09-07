"use client";

import Link from "next/link";
import type { ReactNode } from "react";
import type { Network } from "@/types";

/** The masthead both pages wear: the wordmark, which chain is on screen, and whatever controls the page puts on the right. */
export function PageHeader({ name, info, children }: { name: string; info: Network | null; children: ReactNode }) {
  return (
    <header className="flex flex-wrap items-center justify-between gap-3 py-4">
      <div className="flex min-w-0 flex-col gap-1">
        <Link href="/" className="vw-wordmark text-lg sm:text-xl">
          gascurve
        </Link>
        <div className="flex flex-wrap items-baseline gap-x-3 gap-y-0.5">
          <span className="text-[11px] uppercase tracking-[0.18em] text-label">live gas prices</span>
          <span className="text-sm text-ink-3">{info ? `${info.displayName} · chain ${info.chainId}` : name}</span>
        </div>
      </div>
      <div className="flex max-w-full flex-wrap items-center gap-2">{children}</div>
    </header>
  );
}

/** One label for the explainer, wherever it is linked from, so the header and the lead-in name the same page. */
export const HOW_IT_WORKS_LABEL = "How the fee works →";

/** The explainer route for a network. Every link to it is network scoped: the page quotes that chain's floor and constraint set. */
export function howItWorksHref(network: string): string {
  return `/${encodeURIComponent(network)}/how-it-works`;
}

export function HowItWorksLink({ network, className = "" }: { network: string; className?: string }) {
  return (
    <Link href={howItWorksHref(network)} className={className}>
      {HOW_IT_WORKS_LABEL}
    </Link>
  );
}
