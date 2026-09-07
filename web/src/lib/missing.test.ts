import { describe, expect, it } from "vitest";
import {
  missingBandLabel,
  missingCaption,
  missingNote,
  missingRuns,
  withMissingNote,
  NO_BACKLOG_DATA_LABEL,
  NO_RECEIPT_DATA_LABEL,
  type MissingRow,
} from "./missing";

/** A whole bucket at `t` with `gps` drawn or not. */
function bucket(t: number, gps: number | null): MissingRow {
  return { t, gps, partial: null, coverage: 1 };
}

const present = (row: Record<string, unknown>) => typeof row.gps === "number";

describe("the runs a series has no value over", () => {
  it("joins consecutive buckets with no value into one run, one bucket wide each", () => {
    const rows = [bucket(0, 5), bucket(60, null), bucket(120, null), bucket(180, 7)];
    expect(missingRuns(rows, present, 60, "receipts")).toEqual([{ from: 60, to: 180, kind: "receipts", buckets: 2 }]);
  });
  it("keeps runs either side of a bucket that has a value apart", () => {
    const rows = [bucket(0, null), bucket(60, 5), bucket(120, null)];
    expect(missingRuns(rows, present, 60, "receipts")).toEqual([
      { from: 0, to: 60, kind: "receipts", buckets: 1 },
      { from: 120, to: 180, kind: "receipts", buckets: 1 },
    ]);
  });
  it("closes a run that is still open at the last bucket", () => {
    expect(missingRuns([bucket(0, 5), bucket(60, null)], present, 60, "receipts")).toEqual([{ from: 60, to: 120, kind: "receipts", buckets: 1 }]);
  });
  it("never reaches across a hole in the buckets themselves, which the gap shading already speaks for", () => {
    // Nothing between 60 and 600: two runs, and no band over the gap.
    const rows = [bucket(0, null), bucket(60, null), bucket(600, null)];
    expect(missingRuns(rows, present, 60, "receipts")).toEqual([
      { from: 0, to: 120, kind: "receipts", buckets: 2 },
      { from: 600, to: 660, kind: "receipts", buckets: 1 },
    ]);
  });
  it("leaves the rows another mark already explains alone", () => {
    // The row that breaks a line across a gap is not a bucket, and a bucket
    // that was never indexed whole wears the partial hatch instead.
    const rows: MissingRow[] = [{ t: 0, gps: null, gapRow: true }, { t: 60, gps: null, partial: "leading", coverage: 0.25 }, bucket(120, null)];
    expect(missingRuns(rows, present, 60, "receipts")).toEqual([{ from: 120, to: 180, kind: "receipts", buckets: 1 }]);
  });
  it("takes a step that is not a positive number as nothing to draw", () => {
    const rows = [bucket(0, null)];
    expect(missingRuns(rows, present, 0, "receipts")).toEqual([]);
    expect(missingRuns(rows, present, Number.NaN, "receipts")).toEqual([]);
    expect(missingRuns([], present, 60, "receipts")).toEqual([]);
  });
  it("carries a run over buckets that share a timestamp, as a per-block range has", () => {
    const rows = [bucket(0, null), bucket(0, null), bucket(1, null)];
    expect(missingRuns(rows, present, 1, "receipts")).toEqual([{ from: 0, to: 2, kind: "receipts", buckets: 3 }]);
  });
  it("reads whichever key a series is drawn from, so a backlog slot can ask about two", () => {
    const slot = (row: Record<string, unknown>) => typeof row.b1_0 === "number" || typeof row.bu0 === "number";
    const rows: MissingRow[] = [
      { t: 0, b1_0: 10, bu0: null },
      { t: 60, b1_0: null, bu0: 12 },
      { t: 120, b1_0: null, bu0: null },
    ];
    expect(missingRuns(rows, slot, 60, "backlog")).toEqual([{ from: 120, to: 180, kind: "backlog", buckets: 1 }]);
  });
});

describe("the words", () => {
  it("says what is missing rather than a bare no data", () => {
    expect(missingBandLabel("receipts")).toBe(NO_RECEIPT_DATA_LABEL);
    expect(missingBandLabel("backlog")).toBe(NO_BACKLOG_DATA_LABEL);
    expect(missingNote("receipts")).toContain("no receipt data");
    expect(missingNote("backlog")).toContain("no backlog recorded");
  });
  it("counts the buckets and the stretches under the chart", () => {
    expect(missingCaption([{ from: 0, to: 60, kind: "receipts", buckets: 1 }])).toBe("Dotted: no receipt data for 1 bucket");
    expect(
      missingCaption([
        { from: 0, to: 120, kind: "receipts", buckets: 2 },
        { from: 300, to: 360, kind: "receipts", buckets: 1 },
      ]),
    ).toBe("Dotted: no receipt data for 3 buckets in 2 stretches");
  });
  it("names each kind once when a chart carries both", () => {
    expect(
      missingCaption([
        { from: 0, to: 60, kind: "backlog", buckets: 1 },
        { from: 0, to: 60, kind: "receipts", buckets: 1 },
      ]),
    ).toBe("Dotted: no receipt data for 1 bucket · no backlog data for 1 bucket");
  });
  it("says nothing when nothing is dotted", () => {
    expect(missingCaption([])).toBeNull();
  });
});

describe("the tooltip footnote", () => {
  const base = (row: Record<string, unknown>) => (row.t === 60 ? "owner action" : null);
  const note = withMissingNote(base, present, "receipts");
  it("adds the cause to a bucket the series has no value in", () => {
    expect(note(bucket(0, null))).toBe(missingNote("receipts"));
    expect(note(bucket(0, 5))).toBeNull();
  });
  it("keeps what the chart already said and joins the two", () => {
    expect(note({ t: 60, gps: null, partial: null })).toBe(`owner action · ${missingNote("receipts")}`);
    expect(note({ t: 60, gps: 5, partial: null })).toBe("owner action");
  });
  it("leaves a gap row and a partial bucket to their own explanations", () => {
    expect(note({ t: 0, gps: null, gapRow: true })).toBeNull();
    expect(note({ t: 0, gps: null, partial: "in-progress", coverage: 0.5 })).toBeNull();
  });
});
