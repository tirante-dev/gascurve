import { describe, expect, it } from "vitest";
import {
  completenessOf,
  coverageOf,
  isPartial,
  isPartialRow,
  partialBandLabel,
  partialBands,
  partialCaption,
  partialKinds,
  partialNote,
  partialRowNote,
  partialSumNote,
  withFeeStack,
  IN_PROGRESS_LABEL,
  PARTLY_INDEXED_LABEL,
  UNKNOWN_COVERAGE_LABEL,
  WHOLE,
  type PartialKind,
} from "./partial";

describe("coverage", () => {
  it("reads a whole bucket as one and a partial one as its share", () => {
    expect(coverageOf({ coverage: 1 })).toBe(WHOLE);
    expect(coverageOf({ coverage: 0.25 })).toBe(0.25);
    expect(isPartial({ coverage: 0.25 })).toBe(true);
    expect(isPartial({ coverage: 1 })).toBe(false);
  });
  it("keeps an unmeasurable share null and reads the explicit three-state contract", () => {
    expect(coverageOf({ coverage: null, completeness: "partial" })).toBeNull();
    expect(completenessOf({ coverage: 1, completeness: "complete" })).toBe("complete");
    expect(completenessOf({ coverage: null, completeness: "partial" })).toBe("partial");
    expect(completenessOf({ coverage: null, completeness: "unknown" })).toBe("unknown");
    expect(isPartial({ coverage: null, completeness: "partial" })).toBe(true);
    expect(isPartial({ coverage: null, completeness: "unknown" })).toBe(true);
  });
  it("takes a point with no coverage at all as whole, so an older api draws as it always did", () => {
    expect(coverageOf({})).toBe(WHOLE);
    expect(isPartial({})).toBe(false);
    expect(coverageOf({ coverage: Number.NaN })).toBe(WHOLE);
  });
  it("bounds a share the api could never mean", () => {
    expect(coverageOf({ coverage: -0.5 })).toBe(0);
    expect(coverageOf({ coverage: 4 })).toBe(WHOLE);
  });
});

describe("which kind of partial bucket a point is", () => {
  it("calls a partial bucket at the right edge of the range the one in progress", () => {
    const points = [
      { t: 0, coverage: 0.3 },
      { t: 60, coverage: 1 },
      { t: 120, coverage: 0.2 },
    ];
    expect(partialKinds(points, { to: 133, step: 60 })).toEqual(["leading", null, "in-progress"]);
    // Reaching the edge is not enough when coverage is lower than elapsed
    // time there: that is an internal omission, not merely the live boundary.
    expect(partialKinds(points, { to: 150, step: 60 })).toEqual(["leading", null, "leading"]);
  });
  it("calls a final partial bucket that stops short of the edge partly indexed, not in progress", () => {
    // The collector stopped part way through the bucket at 120 and the range
    // runs to 3600: the last point is old history, not a bucket still filling.
    const points = [
      { t: 0, coverage: 1 },
      { t: 120, coverage: 0.4 },
    ];
    expect(partialKinds(points, { to: 3600, step: 60 })).toEqual([null, "leading"]);
    expect(partialBands(points, 60, 3600)).toEqual([{ from: 120, to: 180, kind: "leading", coverage: 0.4 }]);
    expect(partialBandLabel(partialKinds(points, { to: 3600, step: 60 })[1] as PartialKind)).toBe(PARTLY_INDEXED_LABEL);
  });
  it("falls back to array position without a usable edge to measure against", () => {
    const points = [{ t: 0, coverage: 0.3 }, { t: 60, coverage: 0.2 }];
    expect(partialKinds(points)).toEqual(["leading", "in-progress"]);
    expect(partialKinds(points, { to: Number.NaN, step: 60 })).toEqual(["leading", "in-progress"]);
    expect(partialKinds(points, { to: 3600, step: 0 })).toEqual(["leading", "in-progress"]);
    // A point with no time of its own cannot be measured either.
    expect(partialKinds([{ coverage: 0.3 }, { coverage: 0.2 }], { to: 3600, step: 60 })).toEqual(["leading", "in-progress"]);
  });
  it("does not collapse an unknown bucket into a known partial one", () => {
    const points = [
      { t: 0, coverage: null, completeness: "partial" as const },
      { t: 60, coverage: null, completeness: "unknown" as const },
    ];
    expect(partialKinds(points, { to: 120, step: 60 })).toEqual(["leading", "unknown"]);
    expect(partialBandLabel("unknown")).toBe(UNKNOWN_COVERAGE_LABEL);
  });
  it("has nothing to say about an empty range or one of whole buckets", () => {
    expect(partialKinds([])).toEqual([]);
    expect(partialKinds([{ coverage: 1 }, {}])).toEqual([null, null]);
  });
});

describe("the words", () => {
  it("says a bucket at the right edge is still filling and one further back is only partly there", () => {
    expect(partialNote(1 / 3, "in-progress")).toBe("bucket in progress, 33% elapsed");
    expect(partialNote(0.25, "leading")).toBe("partially indexed, 25% of the bucket");
  });
  it("does not invent a percentage when the exact share or completeness is unknown", () => {
    expect(partialNote(null, "leading")).toBe("partially indexed, coverage unknown");
    expect(partialNote(null, "unknown")).toBe("bucket completeness unknown");
    expect(partialNote(1, "leading")).toBe("partially indexed, missing blocks share a timestamp");
    expect(partialSumNote(null, "unknown")).toBe("bucket completeness unknown; not drawn as a bucket total");
  });
  it("adds that a sum chart leaves the bucket out", () => {
    expect(partialSumNote(0.25, "leading")).toBe("partially indexed, 25% of the bucket; not drawn as a bucket total");
  });
  it("bounds a share it was handed anyway", () => {
    expect(partialNote(Number.NaN, "in-progress")).toBe("bucket in progress, coverage unknown");
    expect(partialNote(-1, "leading")).toBe("partially indexed, 0% of the bucket");
  });
  it("labels the bands", () => {
    expect(partialBandLabel("in-progress")).toBe(IN_PROGRESS_LABEL);
    expect(partialBandLabel("leading")).toBe(PARTLY_INDEXED_LABEL);
  });
});

describe("the tooltip footnote", () => {
  it("says nothing at all about a whole bucket", () => {
    expect(partialRowNote({ partial: null, coverage: 1 })).toBeNull();
    expect(partialRowNote({})).toBeNull();
  });
  it("reads the kind and the share off the row", () => {
    expect(partialRowNote({ partial: "in-progress", coverage: 0.5 })).toBe("bucket in progress, 50% elapsed");
    expect(partialRowNote({ partial: "leading", coverage: 0.5 })).toBe("partially indexed, 50% of the bucket");
    expect(partialRowNote({ partial: "in-progress", coverage: 0.5 }, true)).toBe("bucket in progress, 50% elapsed; not drawn as a bucket total");
    expect(partialRowNote({ partial: "leading", coverage: null }, true)).toBe("partially indexed, coverage unknown; not drawn as a bucket total");
    expect(partialRowNote({ partial: "unknown", coverage: null }, true)).toBe("bucket completeness unknown; not drawn as a bucket total");
  });
  it("falls back to a whole bucket's share when the row carries none", () => {
    expect(partialRowNote({ partial: "in-progress" })).toBe("bucket in progress, 100% elapsed");
    expect(isPartialRow({ partial: "leading" })).toBe(true);
    expect(isPartialRow({ partial: "boundary" })).toBe(false);
  });
});

describe("the bands", () => {
  const points = [
    { t: 0, coverage: 0.4 },
    { t: 60, coverage: 1 },
    { t: 120, coverage: 0.75 },
  ];
  it("covers one bucket each, at both ends of the range", () => {
    expect(partialBands(points, 60)).toEqual([
      { from: 0, to: 60, kind: "leading", coverage: 0.4 },
      { from: 120, to: 180, kind: "in-progress", coverage: 0.75 },
    ]);
  });
  it("draws none at all without a bucket width to draw them over", () => {
    expect(partialBands(points, 0)).toEqual([]);
    expect(partialBands(points, Number.NaN)).toEqual([]);
    expect(partialBands([{ t: 0, coverage: 1 }], 60)).toEqual([]);
  });
  it("captions what is hatched, and nothing when nothing is", () => {
    expect(partialCaption(partialBands(points, 60))).toBe("Hatched and left out: partially indexed, 40% of the bucket · bucket in progress, 75% elapsed");
    expect(partialCaption([])).toBeNull();
  });
});

describe("the fee stack", () => {
  const row = (partial: PartialKind | null) => ({ t: 0, partial, coverage: partial === null ? 1 : 0.5, floorFeesEth: 2, surplusFeesEth: 3, posterFeesEth: 1, unsplitFeesEth: null });
  it("carries a whole bucket's parts through to the keys the stack is drawn from", () => {
    expect(withFeeStack([row(null)])[0]).toMatchObject({ stackFloorEth: 2, stackSurplusEth: 3, stackPosterEth: 1, stackUnsplitEth: null });
  });
  it("leaves a partial bucket out of the stack while keeping what it has collected so far", () => {
    const drawn = withFeeStack([row("in-progress")])[0];
    expect(drawn).toMatchObject({ stackFloorEth: null, stackSurplusEth: null, stackPosterEth: null, stackUnsplitEth: null });
    expect(drawn.floorFeesEth).toBe(2);
    expect(drawn.surplusFeesEth).toBe(3);
  });
});
