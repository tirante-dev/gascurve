"use client";

import type { Network } from "@/types";
import { findNetwork } from "@/utils/network";

export function NetworkSwitcher({ networks, current, onChange, loading }: { networks: Network[] | null; current: string; onChange: (name: string) => void; loading: boolean }) {
  // A network switched off in the collector's configuration is not offered. The api already leaves it
  // out of the list; an older one still reports it, and it is not somewhere a viewer can be sent.
  const known = (networks ?? []).filter((n) => n.enabled !== false);
  // A chain-id route selects its network; only a name the api does not know gets a placeholder option.
  const match = findNetwork(known, current);
  const options = match ? known : [{ name: current, displayName: current, chainId: 0 } as Network, ...known];
  const value = match ? match.name : current;
  return (
    <label className="flex min-w-0 max-w-full items-center gap-2 text-xs text-ink-2">
      <span className="sr-only">Network</span>
      <select
        className="vw-control num min-w-0 max-w-full px-3 py-1 text-sm text-ink"
        value={value}
        onChange={(e) => onChange(e.target.value)}
        disabled={loading && known.length === 0}
        aria-label="Network"
      >
        {options.map((n) => (
          <option key={n.name} value={n.name}>
            {n.displayName}
            {n.chainId ? ` (${n.chainId})` : ""}
          </option>
        ))}
      </select>
    </label>
  );
}
