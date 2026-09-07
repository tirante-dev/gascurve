"use client";

import type { LiveSnapshot, LiveStatus, Network, Series, StatusResponse } from "@/types";
import { formatAgo, formatDateTime, formatInteger } from "@/utils/format";
import { STATUS_COPY } from "./primitives";

export function DataFooter({ snapshot, series, networkInfo, status, apiStatus, now }: { snapshot: LiveSnapshot | null; series: Series | null; networkInfo: Network | null; status: LiveStatus; apiStatus: StatusResponse | null; now: number }) {
  const maxReplayError = series ? Math.max(0, ...series.points.map((p) => p.replayErrorBips)) : 0;
  const estimated = series ? series.points.filter((p) => p.replayErrorBips > 200).length : 0;
  const sampledAgo = snapshot ? (now - new Date(snapshot.sampledAt).getTime()) / 1000 : null;
  const networkStatus = apiStatus?.networks.find((n) => n.name === networkInfo?.name);
  const webVersion = process.env.NEXT_PUBLIC_APP_VERSION ?? "dev";
  return (
    <footer className="vw-rule py-8 text-xs leading-relaxed text-ink-2">
      <div className="grid gap-6 md:grid-cols-3">
        <div>
          <h2 className="mb-2 text-sm font-semibold text-ink">What is live</h2>
          <p>
            Base fee, backlogs, prices and gas per second update on new heads when a WebSocket head feed is configured, or every 3 s by default on public RPC networks. Fee-account balances and
            L1 pricer values refresh from a separate slow sample every 60 s. Live snapshots are pushed over a WebSocket ({STATUS_COPY[status].label}).
          </p>
        </div>
        <div>
          <h2 className="mb-2 text-sm font-semibold text-ink">What is replayed</h2>
          <p>
            Per-constraint backlogs in history are reconstructed by replaying block headers through the pricer and re-anchoring to sampled backlogs whenever a sample exists. The replay error
            is the gap between the predicted and the actual base fee.
          </p>
          <dl className="num mt-2 grid grid-cols-[auto_1fr] gap-x-3 gap-y-0.5 text-ink">
            <dt className="text-ink-3">latest block</dt>
            <dd>{snapshot ? `${snapshot.replayErrorBips} bips` : "n/a"}</dd>
            <dt className="text-ink-3">max in range</dt>
            <dd>
              {series ? `${maxReplayError} bips` : "n/a"}
              {estimated > 0 ? ` (${formatInteger(estimated)} buckets above 2% are estimates)` : ""}
            </dd>
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
