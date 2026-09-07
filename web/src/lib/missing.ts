// Where a series has no input, in buckets that are otherwise whole. A bucket aggregates poster gas as
// null whenever a source block was stored without receipts, so its compute gas per second is null while
// its coverage is 1. Neither the gap tint (no bucket at all) nor the partial hatch (a bucket that is not
// whole) fires on that, and the three are not the same fact.

import { isGapRow } from "@/lib/gaps";
import { isPartialRow } from "@/lib/partial";
import { formatInteger } from "@/utils/format";

/** Which input a series is missing, so a chart says what is actually absent rather than "no data". */
export type MissingKind = "receipts" | "backlog";

export type MissingRun = { from: number; to: number; kind: MissingKind; buckets: number };

/**
 * The least a row needs to be searched for missing runs. Which keys a series reads is the caller's
 * business: a backlog slot is drawn from whichever constraint set is in force.
 */
export type MissingRow = { t: number } & Record<string, unknown>;

export type Present = (row: Record<string, unknown>) => boolean;

/**
 * True when something else already accounts for the row having no value: it stands for a gap, or its
 * bucket was never indexed whole. The three are never stacked on one bucket; the first mark keeps it.
 */
function explained(row: Record<string, unknown>): boolean {
  return isGapRow(row) || isPartialRow(row);
}

/**
 * The runs of `rows` with no value for a series, one bucket per row and `step` seconds wide. Pass the
 * buckets themselves rather than the drawn rows, so the duplicate a set boundary inserts cannot count
 * twice. A run never reaches across a hole in the buckets, because the gap shading speaks for that
 * stretch, and never opens on a row another mark explains.
 */
export function missingRuns(rows: readonly MissingRow[], present: Present, step: number, kind: MissingKind): MissingRun[] {
  if (!Number.isFinite(step) || step <= 0) return [];
  const out: MissingRun[] = [];
  let open: MissingRun | null = null;
  let last: number | null = null;
  for (const row of rows) {
    const missing = !explained(row) && !present(row);
    const contiguous = last !== null && row.t - last <= step;
    if (open !== null && (!missing || !contiguous)) {
      out.push(open);
      open = null;
    }
    if (missing) {
      if (open === null) open = { from: row.t, to: row.t + step, kind, buckets: 0 };
      open.to = row.t + step;
      open.buckets += 1;
    }
    last = row.t;
  }
  if (open !== null) out.push(open);
  return out;
}

const KINDS: readonly MissingKind[] = ["receipts", "backlog"];

export const NO_RECEIPT_DATA_LABEL = "no receipt data";

export const NO_BACKLOG_DATA_LABEL = "no backlog data";

export function missingBandLabel(kind: MissingKind): string {
  return kind === "receipts" ? NO_RECEIPT_DATA_LABEL : NO_BACKLOG_DATA_LABEL;
}

/** What a tooltip says about a bucket with nothing to draw: the cause, since the bucket itself is whole. */
export function missingNote(kind: MissingKind): string {
  return kind === "receipts"
    ? "no receipt data for this bucket, so compute gas per second is not drawn"
    : "no backlog recorded for this bucket";
}

function describe(kind: MissingKind, runs: readonly MissingRun[]): string {
  const buckets = runs.reduce((n, run) => n + run.buckets, 0);
  const where = runs.length === 1 ? "" : ` in ${formatInteger(runs.length)} stretches`;
  return `${missingBandLabel(kind)} for ${formatInteger(buckets)} ${buckets === 1 ? "bucket" : "buckets"}${where}`;
}

export function missingCaption(runs: readonly MissingRun[]): string | null {
  if (runs.length === 0) return null;
  const parts = KINDS.filter((kind) => runs.some((run) => run.kind === kind)).map((kind) => describe(kind, runs.filter((run) => run.kind === kind)));
  return `Dotted: ${parts.join(" · ")}`;
}

/** One series a note speaks for: how to tell it has a value, and what it is missing when it has none. */
export type MissingSeries = { present: Present; kind: MissingKind };

/**
 * `note` with a footnote for every one of `series` the row has no value for. Each cause is named once
 * however many series share it, and a row another mark already explains gains nothing here.
 */
export function withMissingNotes(note: (row: Record<string, unknown>) => string | null, series: readonly MissingSeries[]): (row: Record<string, unknown>) => string | null {
  return (row) => {
    const missing = new Set<MissingKind>();
    if (!explained(row)) {
      for (const one of series) {
        if (!one.present(row)) missing.add(one.kind);
      }
    }
    const parts = [note(row), ...KINDS.filter((kind) => missing.has(kind)).map(missingNote)].filter((part): part is string => part !== null);
    return parts.length > 0 ? parts.join(" · ") : null;
  };
}

export function withMissingNote(note: (row: Record<string, unknown>) => string | null, present: Present, kind: MissingKind): (row: Record<string, unknown>) => string | null {
  return withMissingNotes(note, [{ present, kind }]);
}
