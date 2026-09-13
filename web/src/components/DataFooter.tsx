"use client";

import { useTicker } from "@/hooks/useTicker";
import { unvouchedKind } from "@/lib/fidelity";
import type { LiveSnapshot, LiveStatus, Network, NetworkStatus, Series, StatusResponse } from "@/types";
import { formatAgo, formatDateTime, formatInteger } from "@/utils/format";
import { Bips } from "./primitives";

const TRANSPORT_COPY: Record<LiveStatus, string> = {
  connecting: "WebSocket connecting",
  open: "WebSocket connected",
  reconnecting: "WebSocket reconnecting",
  polling: "HTTP polling fallback active",
};

/** The count is one often enough that "1 buckets are estimates" would be on the page most days. */
function estimatedNote(count: number): string {
  return ` (${formatInteger(count)} ${count === 1 ? "bucket" : "buckets"} above 2% ${count === 1 ? "is an estimate" : "are estimates"})`;
}

/** What the live head says the replay stands on: the version producing blocks now, and whether the
 * pricer has been measured against it. */
function modelStanding(snapshot: LiveSnapshot | null): string {
  const version = snapshot?.arbosVersion;
  if (typeof version !== "number") return "n/a";
  const fidelity = snapshot?.replayFidelity;
  return fidelity === "unverified" ? `ArbOS ${formatInteger(version)}, not yet measured` : `ArbOS ${formatInteger(version)}`;
}

function counted(count: number, noun: string): string {
  return `${formatInteger(count)} ${noun}${count === 1 ? "" : "s"}`;
}

function missingHistoryStatus(network: NetworkStatus | null): string {
  if (!network) return "unknown";
  const holes = network.holes;
  if (holes.checkpointError) return "unknown (checkpoint unreadable)";
  if (holes.blocks === 0) return "0 missing blocks recorded";
  return `${counted(holes.blocks, "missing block")}; ${counted(holes.pending, "queued range")} (${counted(holes.retrying, "retrying range")}), ${counted(holes.unfillable, "unfillable range")}`;
}

function capacityStatus(network: NetworkStatus | null): string {
  if (!network) return "unknown";
  if (network.capacity.checkpointError) return "unknown (checkpoint unreadable)";
  if (network.capacity.at === null) return "unknown (not sampled)";
  return network.capacity.saturated ? "saturated" : "not saturated";
}

function endpointSummary(network: NetworkStatus | null): string {
  if (!network || network.endpoints.length === 0) return "unknown";
  const endpoint = network.endpoints.find((item) => item.index === network.activeEndpoint);
  if (!endpoint) return `unknown (reported endpoint ${formatInteger(network.activeEndpoint + 1)})`;
  const role = endpoint.index === 0 ? "primary" : "fallback";
  const state = endpoint.disabled ? "disabled" : endpoint.error ? `error: ${endpoint.error}` : "available";
  const archive = endpoint.archive ? ", archive" : "";
  const headFeed = endpoint.ws ? ", WebSocket configured" : ", no WebSocket configured";
  const cooling = endpoint.wsCooling ? ", WebSocket cooling" : "";
  const wsError = endpoint.wsError ? `, WebSocket error: ${endpoint.wsError}` : "";
  return `${role} endpoint ${formatInteger(endpoint.index + 1)} (JSON-RPC/HTTP${archive}, ${state}${headFeed}${cooling}${wsError}); ${counted(network.endpoints.length, "configured endpoint")}; ${counted(network.failovers, "failover")}`;
}

function listenerStatus(apiStatus: StatusResponse | null): string {
  const listener = apiStatus?.listener;
  if (!listener) return "unknown";
  const error = listener.lastError ? `; last error: ${listener.lastError}` : "";
  return `${listener.ready ? "ready" : "not ready"}; ${counted(listener.reconnects, "reconnect")}${error}`;
}

/** `now` is for a caller that fixes the clock; left out, the footer keeps its own so the page above it does not tick. */
export function DataFooter({ snapshot, series, networkInfo, status, apiStatus, now }: { snapshot: LiveSnapshot | null; series: Series | null; networkInfo: Network | null; status: LiveStatus; apiStatus: StatusResponse | null; now?: number }) {
  const ticked = useTicker(now === undefined ? 1000 : 0);
  const clock = now ?? ticked;
  const maxReplayError = series && series.points.length > 0 ? Math.max(...series.points.map((p) => p.replayErrorBips)) : null;
  const estimated = series ? series.points.filter((p) => p.replayErrorBips > 200).length : 0;
  // Buckets whose replay the measurement does not cover, which is a different fact from a large error:
  // the numbers may be right, nobody has checked the model that produced them.
  const unvouched = series ? series.points.filter((p) => unvouchedKind(p) !== null).length : 0;
  const sampledAgo = snapshot ? (clock - new Date(snapshot.sampledAt).getTime()) / 1000 : null;
  const chainId = snapshot?.chainId ?? networkInfo?.chainId;
  const networkStatus = apiStatus?.networks.find((network) => (chainId === undefined ? false : network.chainId === chainId)) ?? apiStatus?.networks.find((network) => network.name === networkInfo?.name) ?? null;
  const collector = networkStatus?.collector ?? null;
  const heartbeatAt = collector?.heartbeatAt ?? null;
  const telemetryAt = heartbeatAt ? Date.parse(heartbeatAt) : Number.NaN;
  const telemetry = heartbeatAt && Number.isFinite(telemetryAt) ? `${formatDateTime(heartbeatAt)} (${formatAgo(Math.max(0, (clock - telemetryAt) / 1000))})` : "unknown";
  const webVersion = process.env.NEXT_PUBLIC_APP_VERSION ?? "dev";
  return (
    <footer className="vw-rule py-8 text-xs leading-relaxed text-ink-2">
      <div className="grid gap-6 md:grid-cols-3">
        <div>
          <h2 className="mb-2 text-sm font-semibold text-ink">What is live</h2>
          <p>
            Base fee, backlogs, prices, and compute gas per second update on new heads when a WebSocket head feed is configured, or every 3 s by default on public RPC networks. Compute gas comes
            from receipt-backed block history. Fee-account balances and L1 pricer values refresh from a separate slow sample every 60 s. Browser transport: {TRANSPORT_COPY[status]}. This transport
            state reports delivery to this browser, not collector data health.
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
            <dt className="text-ink-3">max indexed in range</dt>
            <dd className="relative">
              {maxReplayError === null ? "n/a" : <Bips value={maxReplayError} />}
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
            <dt className="text-ink-3">network data health</dt>
            <dd>{networkStatus?.status ?? "unknown"}</dd>
            <dt className="text-ink-3">degraded reasons</dt>
            <dd>{networkStatus ? (networkStatus.degradedReasons.length > 0 ? networkStatus.degradedReasons.join("; ") : "none reported") : "unknown"}</dd>
            <dt className="text-ink-3">last sample</dt>
            <dd>{snapshot ? `${formatDateTime(snapshot.sampledAt)} (${formatAgo(sampledAgo ?? 0)})` : "unknown"}</dd>
            <dt className="text-ink-3">displayed live block</dt>
            <dd>{snapshot ? formatInteger(snapshot.block.number) : "unknown"}</dd>
            <dt className="text-ink-3">status indexed head</dt>
            <dd>{collector ? formatInteger(collector.indexedHead) : "unknown"}</dd>
            <dt className="text-ink-3">status observed head</dt>
            <dd>{collector ? formatInteger(collector.observedHead) : "unknown"}</dd>
            <dt className="text-ink-3">status head lag</dt>
            <dd>{collector ? counted(collector.headLagBlocks, "block") : "unknown"}</dd>
            <dt className="text-ink-3">status telemetry</dt>
            <dd>{telemetry}</dd>
            <dt className="text-ink-3">missing history</dt>
            <dd>{missingHistoryStatus(networkStatus)}</dd>
            <dt className="text-ink-3">RPC capacity</dt>
            <dd>{capacityStatus(networkStatus)}</dd>
            <dt className="text-ink-3">collector RPC route</dt>
            <dd>{endpointSummary(networkStatus)}</dd>
            <dt className="text-ink-3">notification listener</dt>
            <dd>{listenerStatus(apiStatus)}</dd>
            <dt className="text-ink-3">rate limits</dt>
            <dd>{networkStatus ? counted(networkStatus.rateLimitEvents, "event") : "unknown"}</dd>
            <dt className="text-ink-3">last error</dt>
            <dd>{networkStatus ? (networkStatus.lastError ?? "none") : "unknown"}</dd>
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
