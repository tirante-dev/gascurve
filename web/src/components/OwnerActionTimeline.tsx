"use client";

import { parseConstraintArg, rawConstraintArg } from "@/lib/ownerActions";
import type { OwnerAction } from "@/types";
import { shortConstraintLabel } from "@/utils/chart";
import { formatGas, formatGwei, formatInteger, formatUtc, shortHash } from "@/utils/format";

function ArgsView({ action }: { action: OwnerAction }) {
  if (action.method === "setGasPricingConstraints" && Array.isArray(action.args.constraints)) {
    return (
      <ul className="flex flex-wrap gap-1.5">
        {action.args.constraints.map((c, i) => {
          const parsed = parseConstraintArg(c);
          if (!parsed) return <li key={i} className="num rounded bg-surface-2 px-2 py-0.5 text-xs text-ink">{rawConstraintArg(c)}</li>;
          return (
            <li key={i} className="num rounded bg-surface-2 px-2 py-0.5 text-xs text-ink">
              {shortConstraintLabel({ target: parsed.target, window: parsed.window })} · start {formatGas(parsed.backlog)}
            </li>
          );
        })}
      </ul>
    );
  }
  if (action.method === "setMinimumL2BaseFee" && typeof action.args.priceInWei === "string") {
    return <span className="num rounded bg-surface-2 px-2 py-0.5 text-xs text-ink">floor {formatGwei(action.args.priceInWei)} gwei</span>;
  }
  const entries = Object.entries(action.args);
  if (entries.length === 0) return null;
  return (
    <ul className="flex flex-wrap gap-1.5">
      {entries.map(([k, v]) => (
        <li key={k} className="num rounded bg-surface-2 px-2 py-0.5 text-xs text-ink">
          {k}: {typeof v === "string" ? (v.length > 24 ? shortHash(v) : v) : JSON.stringify(v)}
        </li>
      ))}
    </ul>
  );
}

/** Decoded owner actions, newest first. */
export function OwnerActionTimeline({ actions, explorerUrl, loading, error }: { actions: OwnerAction[] | null; explorerUrl?: string; loading: boolean; error: string | null }) {
  if (error) return <p className="text-sm text-critical">Could not load owner actions: {error}</p>;
  if (!actions) return <p className="text-sm text-ink-2">{loading ? "Loading owner actions." : "No owner actions."}</p>;
  if (actions.length === 0) return <p className="text-sm text-ink-2">No owner actions decoded for this network yet.</p>;
  const base = explorerUrl?.replace(/\/+$/, "");
  return (
    <ol className="vw-card divide-y divide-hairline">
      {actions.map((a) => (
        <li key={`${a.txHash}-${a.block}`} className="grid gap-2 px-4 py-3 sm:grid-cols-[180px_minmax(0,1fr)]">
          <div className="num text-xs text-ink-3">
            <div className="text-ink">{formatUtc(a.at)}</div>
            <div>
              block{" "}
              {base ? (
                <a className="text-accent underline-offset-2 hover:underline" href={`${base}/block/${a.block}`} target="_blank" rel="noreferrer">
                  {formatInteger(a.block)}
                </a>
              ) : (
                formatInteger(a.block)
              )}
            </div>
            <div>
              {base ? (
                <a className="text-accent underline-offset-2 hover:underline" href={`${base}/tx/${a.txHash}`} target="_blank" rel="noreferrer">
                  {shortHash(a.txHash)}
                </a>
              ) : (
                shortHash(a.txHash)
              )}
            </div>
          </div>
          <div className="min-w-0">
            <div className="text-sm text-ink">
              <span className="num">{a.method}</span> <span className="num text-xs text-ink-3">{a.selector}</span>
            </div>
            <div className="mt-1.5">
              <ArgsView action={a} />
            </div>
          </div>
        </li>
      ))}
    </ol>
  );
}
