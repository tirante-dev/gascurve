// Where a series has no input, in buckets that are otherwise whole. A bucket
// aggregates poster gas as null whenever a source block was stored without
// receipts, so its compute gas per second is null while its coverage is 1 and
// its completeness "complete". The line simply broke, which read as "the
// chain carried no gas" rather than "we have no receipt data here". Neither
// the gap tint (no bucket at all) nor the partial hatch (a bucket that is not
// whole) fires on that, and the three are not the same fact: these are the
// pure parts of the third one, the runs of it and the words a chart puts on
// them.

import { isGapRow } from "@/lib/gaps";
import { isPartialRow } from "@/lib/partial";
import { formatInteger } from "@/utils/format";

/**
 * Which input a series is missing. A chart says what is actually absent
 * rather than a bare "no data": the compute gas rate is missing because the
 * blocks behind the bucket carry no receipts, a backlog because the bucket
 * recorded none for that slot.
 */
export type MissingKind = "receipts" | "backlog";

/** A run of buckets with no value for one series, in unix seconds. */
export type MissingRun = { from: number; to: number; kind: MissingKind; buckets: number };

/**
 * The least a row needs to be searched for missing runs. The chart rows carry
 * far more, and which of their keys a series reads is the caller's business:
 * a backlog slot is drawn from the key of whichever constraint set is in
 * force, so "does this row have a value" is a question only the chart can
 * answer.
 */
export type MissingRow = { t: number } & Record<string, unknown>;

/** True when the row has a value for the series in question. */
export type Present = (row: Record<string, unknown>) => boolean;

/**
 * True when something else already accounts for the row having no value: it
 * stands for a gap rather than for a bucket, or its bucket was never indexed
 * whole. A hole in the record, a bucket still filling and a series with no
 * input are three different things, so they are never stacked on one bucket
 * and the mark that fires first keeps it.
 */
function explained(row: Record<string, unknown>): boolean {
  return isGapRow(row) || isPartialRow(row);
}

/**
 * The runs of `rows` with no value for a series, one bucket per row and
 * `step` seconds wide each. Rows arrive sorted, oldest first, one per bucket:
 * pass the buckets themselves rather than the drawn rows, so the duplicate a
 * set boundary inserts cannot count twice.
 *
 * A run never reaches across a hole in the buckets themselves (rows more than
 * one step apart), because the gap shading already speaks for that stretch,
 * and it never opens on a row another mark explains. A step that is not a
 * positive number leaves the runs out rather than returning zero-width ones.
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

/** What the band over buckets with no receipts behind them says. */
export const NO_RECEIPT_DATA_LABEL = "no receipt data";

/** What the band over buckets that recorded no backlog for a slot says. */
export const NO_BACKLOG_DATA_LABEL = "no backlog data";

/** The word a shaded run wears. */
export function missingBandLabel(kind: MissingKind): string {
  return kind === "receipts" ? NO_RECEIPT_DATA_LABEL : NO_BACKLOG_DATA_LABEL;
}

/**
 * What a tooltip says about a bucket with nothing to draw for the series: the
 * cause, since the bucket itself is whole and the reader can see that the
 * charts above it are drawn over the same minute.
 */
export function missingNote(kind: MissingKind): string {
  return kind === "receipts"
    ? "no receipt data for this bucket, so compute gas per second is not drawn"
    : "no backlog recorded for this constraint in this bucket";
}

/** The kinds a caption names, in the order it names them. */
const KINDS: readonly MissingKind[] = ["receipts", "backlog"];

function describe(kind: MissingKind, runs: readonly MissingRun[]): string {
  const buckets = runs.reduce((n, run) => n + run.buckets, 0);
  const where = runs.length === 1 ? "" : ` in ${formatInteger(runs.length)} stretches`;
  return `${missingBandLabel(kind)} for ${formatInteger(buckets)} ${buckets === 1 ? "bucket" : "buckets"}${where}`;
}

/**
 * The line under a chart that dots the runs a series has no input for: what
 * the dots mean and how many buckets they cover. Null when nothing is dotted.
 */
export function missingCaption(runs: readonly MissingRun[]): string | null {
  if (runs.length === 0) return null;
  const parts = KINDS.filter((kind) => runs.some((run) => run.kind === kind)).map((kind) => describe(kind, runs.filter((run) => run.kind === kind)));
  return `Dotted: ${parts.join(" · ")}`;
}

/**
 * `note` with the missing-series footnote added, joined the way `bucketNote`
 * joins its own parts. A row another mark already explains keeps that
 * explanation and gains nothing here, exactly as the bands do.
 */
export function withMissingNote(note: (row: Record<string, unknown>) => string | null, present: Present, kind: MissingKind): (row: Record<string, unknown>) => string | null {
  return (row) => {
    const missing = !explained(row) && !present(row);
    const parts = [note(row), missing ? missingNote(kind) : null].filter((part): part is string => part !== null);
    return parts.length > 0 ? parts.join(" · ") : null;
  };
}
