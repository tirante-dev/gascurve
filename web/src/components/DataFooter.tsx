"use client";

import { useTicker } from "@/hooks/useTicker";
import { unvouchedKind } from "@/lib/fidelity";
import type { LiveSnapshot, LiveStatus, Network, Series, StatusResponse } from "@/types";
import { formatAgo, formatDateTime, formatInteger } from "@/utils/format";
import { Bips, STATUS_COPY } from "./primitives";

/** The count is one often enough that "1 buckets are estimates" would be on the page most days. */
function estimatedNote(count: number): string {
  return ` (${formatInteger(count)} ${count === 1 ? "bucket" : "buckets"} above 2% ${count === 1 ? "is an estimate" : "are estimates"})`;
}

/** `now` is for a caller that fixes the clock; left out, the footer keeps its own so the page above it does not tick. */
/** What the live head says the replay stands on: the version producing blocks now, and whether the
 * pricer has been measured against it. */
function modelStanding(snapshot: LiveSnapshot | null): string {
  const version = snapshot?.arbosVersion;
  if (typeof version !== "number") return "n/a";
  const fidelity = snapshot?.replayFidelity;
  return fidelity === "unverified" ? `ArbOS ${formatInteger(version)}, not yet measured` : `ArbOS ${formatInteger(version)}`;
}

export function DataFooter({ snapshot, series, networkInfo, status, apiStatus, now }: { snapshot: LiveSnapshot | null; series: Series | null; networkInfo: Network | null; status: LiveStatus; apiStatus: StatusResponse | null; now?: number }) {
  const ticked = useTicker(now === undefined ? 1000 : 0);
  const clock = now ?? ticked;
  const maxReplayError = series ? Math.max(0, ...series.points.map((p) => p.replayErrorBips)) : 0;
  const estimated = series ? series.points.filter((p) => p.replayErrorBips > 200).length : 0;
  // Buckets whose replay the measurement does not cover, which is a different fact from a large error:
  // the numbers may be right, nobody has checked the model that produced them.
  const unvouched = series ? series.points.filter((p) => unvouchedKind(p) !== null).length : 0;
  const sampledAgo = snapshot ? (clock - new Date(snapshot.sampledAt).getTime()) / 1000 : null;
  const networkStatus = apiStatus?.networks.find((n) => n.name === networkInfo?.name);
  const webVersion = process.env.NEXT_PUBLIC_APP_VERSION ?? "dev";
  return (
    <footer className="vw-rule py-8 text-xs leading-relaxed text-ink-2">
      <div className="grid gap-6 md:grid-cols-3">
        <div>
          <h2 className="mb-2 text-sm font-semibold text-ink">What is live</h2>
          <p>
            Base fee, backlogs, prices, and compute gas per second update on new heads when a WebSocket head feed is configured, or every 3 s by default on public RPC networks. Compute gas comes
            from receipt-backed block history. Fee-account balances and L1 pricer values refresh from a separate slow sample every 60 s. Live snapshots are pushed over a WebSocket ({STATUS_COPY[status].label}).
          </p>
        </div>
        <div>
          <h2 className="mb-2 text-sm font-semibold text-ink">What is replayed</h2>
          <p>
            Per-constraint backlogs in history are reconstructed from block headers and receipt poster gas, then re-anchored to sampled backlogs whenever a sample exists. The replay error
            is the gap between the predicted and the actual base fee.
          </p>
          <dl className="num mt-2 grid grid-cols-[auto_1fr] gap-x-3 gap-y-0.5 text-ink">
            <dt className="text-ink-3">latest block</dt>
            {/* `relative` is what the note anchors to, as it is on every line that carries one. */}
            <dd className="relative">{snapshot ? <Bips value={snapshot.replayErrorBips} /> : "n/a"}</dd>
            <dt className="text-ink-3">max in range</dt>
            <dd className="relative">
              {series ? <Bips value={maxReplayError} /> : "n/a"}
              {estimated > 0 ? estimatedNote(estimated) : ""}
            </dd>
            <dt className="text-ink-3">pricing model</dt>
            <dd>{modelStanding(snapshot)}</dd>
            {unvouched > 0 ? (
              <>
                <dt className="text-ink-3">unverified buckets</dt>
                <dd>{`${formatInteger(unvouched)} of ${formatInteger(series?.points.length ?? 0)} in range`}</dd>
              </>
            ) : null}
          </dl>
        </div>
        <div>
          <h2 className="mb-2 text-sm font-semibold text-ink">Collector</h2>
          <dl className="num grid grid-cols-[auto_1fr] gap-x-3 gap-y-0.5 text-ink">
            <dt className="text-ink-3">last sample</dt>
            <dd>{snapshot ? `${formatDateTime(snapshot.sampledAt)} (${formatAgo(sampledAgo ?? 0)})` : "n/a"}</dd>
            <dt className="text-ink-3">head</dt>
            <dd>{networkInfo ? `${formatInteger(networkInfo.headBlock)}, ${networkInfo.lagSeconds === null ? "no head yet" : `lag ${networkInfo.lagSeconds} s`}` : "n/a"}</dd>
            <dt className="text-ink-3">rate limits</dt>
            <dd>{networkStatus ? `${formatInteger(networkStatus.rateLimitEvents)} events` : "n/a"}</dd>
            <dt className="text-ink-3">last error</dt>
            <dd>{networkStatus ? (networkStatus.lastError ?? "none") : "n/a"}</dd>
            <dt className="text-ink-3">versions</dt>
            <dd>
              web {webVersion}
              {apiStatus ? `, api ${apiStatus.version}` : ""}
            </dd>
          </dl>
          <p className="mt-3 text-[11px] font-medium uppercase tracking-[0.1em] text-label">
            <a href="https://tirante.dev" target="_blank" rel="noopener noreferrer" className="hover:text-accent-2-text">
              powered by tirante.dev
            </a>
          </p>
        </div>
      </div>
      <p className="mt-6 text-ink-3">
        Times are shown in your local zone with its abbreviation; owner actions in UTC.
      </p>
    </footer>
  );
}
