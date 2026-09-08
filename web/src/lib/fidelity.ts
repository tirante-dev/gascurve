// What the ArbOS versions behind a bucket say its replay is worth. The pricer reproduces one pricing
// model, measured against the ArbOS versions in docs/SPEC.md section 7.1. A bucket outside that, or one
// spanning an upgrade, still carries numbers; nothing has checked them, and drawing them like the rest
// presents a guess with the confidence of a measurement.

import type { ReplayFidelity } from "@/types";
import { formatInteger } from "@/utils/format";

/** What every point carries. Optional on purpose: an older api sends nothing, and an api that says
 * nothing is not evidence of a boundary, so it draws as a plain bucket. */
export type Versioned = { replayFidelity?: ReplayFidelity; arbosVersionMin?: number | null; arbosVersionMax?: number | null };

/** The two answers worth drawing. `unknown` covers history recorded before the version was stored,
 * which is most of an existing database: shading all of it would say something the data does not. */
export type UnvouchedKind = "boundary" | "unverified";

export function fidelityOf(point: Versioned): ReplayFidelity {
  const raw = point.replayFidelity;
  if (raw === "boundary" || raw === "unverified" || raw === "unknown" || raw === "verified") return raw;
  return "verified";
}

/** The kind a point is shaded for, or null when it is not shaded. */
export function unvouchedKind(point: Versioned): UnvouchedKind | null {
  const fidelity = fidelityOf(point);
  return fidelity === "boundary" || fidelity === "unverified" ? fidelity : null;
}

export const BOUNDARY_LABEL = "across an ArbOS upgrade";

export const UNVERIFIED_LABEL = "unverified pricing model";

export function fidelityBandLabel(kind: UnvouchedKind): string {
  return kind === "boundary" ? BOUNDARY_LABEL : UNVERIFIED_LABEL;
}

/** The ArbOS versions a point names, "51 to 61" across a boundary and "61" inside one. Null when it
 * recorded none. */
export function versionRange(point: Versioned): string | null {
  const lo = point.arbosVersionMin;
  const hi = point.arbosVersionMax;
  if (typeof lo !== "number" || typeof hi !== "number") return null;
  return lo === hi ? formatInteger(lo) : `${formatInteger(lo)} to ${formatInteger(hi)}`;
}

/** What a tooltip says about a bucket the replay cannot vouch for. `versions` is the range it names,
 * null when it recorded none. */
export function unvouchedNote(kind: UnvouchedKind, versions: string | null): string {
  if (kind === "boundary") {
    const span = versions === null ? "an ArbOS upgrade" : `ArbOS ${versions}`;
    return `spans ${span}; the replay crossed a pricing model change`;
  }
  const version = versions === null ? "this ArbOS version" : `ArbOS ${versions}`;
  return `the pricer has not been measured against ${version}`;
}

/** The same for a point, and null for a bucket the replay can vouch for. */
export function fidelityNote(point: Versioned): string | null {
  const kind = unvouchedKind(point);
  return kind === null ? null : unvouchedNote(kind, versionRange(point));
}

export type FidelityBand = { from: number; to: number; kind: UnvouchedKind };

/** One band per unvouched bucket, so a chart that draws its marks normally still shows where they are.
 * A `step` that is not positive leaves them out. */
export function fidelityBands(points: readonly (Versioned & { t: number })[], step: number): FidelityBand[] {
  if (!Number.isFinite(step) || step <= 0) return [];
  const out: FidelityBand[] = [];
  for (const point of points) {
    const kind = unvouchedKind(point);
    if (kind !== null) out.push({ from: point.t, to: point.t + step, kind });
  }
  return out;
}

export type FidelityRun = { from: number; to: number; kind: UnvouchedKind; buckets: number };

/** The bands merged into runs of one kind, so a stretch draws as one mark. Bands must arrive oldest
 * first, as `fidelityBands` and the api both give them. */
export function fidelityRuns(bands: readonly FidelityBand[]): FidelityRun[] {
  const out: FidelityRun[] = [];
  for (const band of bands) {
    const open = out.length > 0 ? out[out.length - 1] : null;
    if (open !== null && open.kind === band.kind && band.from <= open.to) {
      open.to = Math.max(open.to, band.to);
      open.buckets += 1;
      continue;
    }
    out.push({ from: band.from, to: band.to, kind: band.kind, buckets: 1 });
  }
  return out;
}

const KINDS: readonly UnvouchedKind[] = ["boundary", "unverified"];

function describe(kind: UnvouchedKind, bands: readonly FidelityBand[]): string {
  const runs = fidelityRuns(bands).length;
  const where = runs === 1 ? "" : ` in ${formatInteger(runs)} stretches`;
  return `${fidelityBandLabel(kind)} for ${formatInteger(bands.length)} buckets${where}`;
}

/** The line under a chart that marks unvouched buckets. Null when none are marked. */
export function fidelityCaption(bands: readonly FidelityBand[]): string | null {
  if (bands.length === 0) return null;
  const parts = KINDS.filter((kind) => bands.some((b) => b.kind === kind)).map((kind) => describe(kind, bands.filter((b) => b.kind === kind)));
  return `Marked, replay not verified: ${parts.join(" · ")}`;
}

/** The tooltip footnote a hovered chart row earns for sitting in an unvouched bucket, or null. */
export function fidelityRowNote(row: Record<string, unknown>): string | null {
  const kind = row.fidelity;
  if (kind !== "boundary" && kind !== "unverified") return null;
  return unvouchedNote(kind, typeof row.arbosVersions === "string" ? row.arbosVersions : null);
}
