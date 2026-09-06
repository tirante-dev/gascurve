"use client";

import type { Network } from "@/types";

export function NetworkSwitcher({ networks, current, onChange, loading }: { networks: Network[] | null; current: string; onChange: (name: string) => void; loading: boolean }) {
  const known = networks ?? [];
  const options = known.some((n) => n.name === current) ? known : [{ name: current, displayName: current, chainId: 0 } as Network, ...known];
  return (
    <label className="flex items-center gap-2 text-xs text-ink-2">
      <span className="sr-only">Network</span>
      <select
        className="num rounded-md border border-hairline bg-surface px-2 py-1 text-sm text-ink"
        value={current}
        onChange={(e) => onChange(e.target.value)}
        disabled={loading && known.length === 0}
        aria-label="Network"
      >
        {options.map((n) => (
          <option key={n.name} value={n.name} disabled={n.enabled === false}>
            {n.displayName}
            {n.chainId ? ` (${n.chainId})` : ""}
          </option>
        ))}
      </select>
    </label>
  );
}
