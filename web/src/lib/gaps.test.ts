import { describe, expect, it } from "vitest";
import {
  BLOCK_GAP_S,
  DEFAULT_STEP_SECONDS,
  emptyRangeNote,
  findGaps,
  firstIndexed,
  gapBandLabel,
  gapCaption,
  gapModel,
  bucketSeconds,
  irregularStep,
  isGapRow,
  NOTHING_INDEXED,
  NOT_INDEXED_LABEL,
  NO_GAPS,
  stepSeconds,
  windowOf,
  withGapBreaks,
  type Gap,
} from "./gaps";
import { formatDateTime } from "@/utils/format";

const T = 1_788_679_200;

function at(...times: number[]): { t: number }[] {
  return times.map((t) => ({ t }));
}

describe("the step points are expected at", () => {
  it("takes the resolution's own spacing where the api names one", () => {
    expect(stepSeconds("5s")).toBe(5);
    expect(stepSeconds("1m")).toBe(60);
    expect(stepSeconds("15m")).toBe(900);
    expect(stepSeconds("1h")).toBe(3600);
  });

  it("judges a per-block range by the block rule, not by a bucket width", () => {
    expect(stepSeconds("block")).toBe(BLOCK_GAP_S);
  });

  it("measures the spacing when the resolution is unknown, and falls back to a minute", () => {
    expect(stepSeconds(undefined, at(T, T + 30, T + 90))).toBe(30);
    expect(stepSeconds("batch", at(T, T + 15))).toBe(15);
    expect(stepSeconds(undefined, at(T))).toBe(DEFAULT_STEP_SECONDS);
    expect(stepSeconds(undefined)).toBe(DEFAULT_STEP_SECONDS);
    // Two points at the same second measure nothing.
    expect(stepSeconds(undefined, at(T, T))).toBe(DEFAULT_STEP_SECONDS);
  });

  it("is tolerant for points that arrive on their own cadence rather than on a grid", () => {
    // Reports every 20 s or so: three intervals, not the 15 s bucket.
    expect(irregularStep(at(T, T + 20, T + 40, T + 62), 15)).toBe(60);
    // Never below the bucket the reports were grouped into.
    expect(irregularStep(at(T, T + 2), 15)).toBe(15);
    expect(irregularStep([], 15)).toBe(15);
    expect(irregularStep(at(T), 0)).toBe(0);
  });
});

describe("the width of one bucket", () => {
  it("comes from the resolution, never from the distance between two points", () => {
    expect(bucketSeconds("5s")).toBe(5);
    expect(bucketSeconds("1m")).toBe(60);
    expect(bucketSeconds("15m")).toBe(900);
    expect(bucketSeconds("1h")).toBe(3600);
    // A per-block range carries one point per block, so its bucket is a
    // second, which is all a header's timestamp resolves. Its gap threshold
    // is a separate question and stays BLOCK_GAP_S.
    expect(bucketSeconds("block")).toBe(1);
    expect(stepSeconds("block")).toBe(BLOCK_GAP_S);
  });

  it("is not thrown by the accidents of the points it is given", () => {
    // Two per-block points sharing a timestamp used to make a bucket of zero
    // width, which put every owner action in the range into one bucket.
    expect(bucketSeconds("block", at(T, T))).toBe(1);
    // A missing second point used to inflate the bucket to the size of the hole.
    expect(bucketSeconds("1m", at(T, T + 600))).toBe(60);
    // A range with one bucket in it used to read as a minute whatever it was.
    expect(bucketSeconds("15m", at(T))).toBe(900);
  });

  it("falls back to the points, and then to a minute, for a resolution the client does not know", () => {
    expect(bucketSeconds("batch", at(T, T + 15, T + 45))).toBe(15);
    expect(bucketSeconds("batch", at(T))).toBe(DEFAULT_STEP_SECONDS);
    expect(bucketSeconds(undefined)).toBe(DEFAULT_STEP_SECONDS);
  });
});

describe("the window a chart's axis spans", () => {
  it("is the one the api answered", () => {
    expect(windowOf({ from: T, to: T + 3600 }, at(T + 1800), 60)).toEqual({ from: T, to: T + 3600 });
  });

  it("falls back to the extent of the data when the api carries no window", () => {
    expect(windowOf({}, at(T, T + 60), 60)).toEqual({ from: T, to: T + 120 });
    expect(windowOf({}, [], 60)).toEqual({ from: 0, to: 0 });
  });

  it("never ends before it starts", () => {
    expect(windowOf({ from: T, to: T - 100 }, [], 60)).toEqual({ from: T, to: T });
  });
});

describe("finding the spans with nothing in them", () => {
  it("shades the history from before the first bucket", () => {
    const gaps = findGaps(at(T + 7200, T + 7260), { from: T, to: T + 7320 }, 60);
    expect(gaps).toEqual([{ from: T, to: T + 7200, kind: "leading" }]);
  });

  it("shades a hole between two buckets, and only where a bucket is actually missing", () => {
    const gaps = findGaps(at(T, T + 60, T + 300, T + 360), { from: T, to: T + 420 }, 60);
    expect(gaps).toEqual([{ from: T + 120, to: T + 300, kind: "interior" }]);
  });

  it("shades the end of the window when nothing was indexed up to it", () => {
    expect(findGaps(at(T, T + 60), { from: T, to: T + 600 }, 60)).toEqual([{ from: T + 120, to: T + 600, kind: "trailing" }]);
    // One step past the last bucket is that bucket's own width, not a gap.
    expect(findGaps(at(T, T + 60), { from: T, to: T + 120 }, 60)).toEqual([]);
  });

  it("takes a bucket that starts exactly at the window as no gap at all", () => {
    expect(findGaps(at(T, T + 60, T + 120), { from: T, to: T + 180 }, 60)).toEqual([]);
    // The window is not aligned to the grid, so a part-step offset is the grid.
    expect(findGaps(at(T + 30, T + 90), { from: T, to: T + 150 }, 60)).toEqual([]);
    // A whole step of daylight is a gap.
    expect(findGaps(at(T + 60), { from: T, to: T + 120 }, 60)).toEqual([{ from: T, to: T + 60, kind: "leading" }]);
  });

  it("takes a range with no points at all as one gap over the whole window", () => {
    expect(findGaps([], { from: T, to: T + 3600 }, 60)).toEqual([{ from: T, to: T + 3600, kind: "leading" }]);
  });

  it("draws nothing at all without a usable step or window", () => {
    expect(findGaps(at(T), { from: T, to: T + 60 }, 0)).toEqual([]);
    expect(findGaps(at(T), { from: T, to: T + 60 }, Number.NaN)).toEqual([]);
    expect(findGaps(at(T), { from: T, to: T }, 60)).toEqual([]);
  });

  it("never shades outside the window the range asked for", () => {
    // A point before the start of the window: the hole after it begins at the
    // window, not one step after a point the chart does not draw.
    expect(findGaps(at(T - 600, T + 300), { from: T, to: T + 360 }, 60)).toEqual([{ from: T, to: T + 300, kind: "interior" }]);
    // And a point past the end: the hole before it stops at the window.
    expect(findGaps(at(T, T + 900), { from: T, to: T + 300 }, 60)).toEqual([{ from: T + 60, to: T + 300, kind: "interior" }]);
    // Nothing of the hole falls inside the window at all, so nothing is shaded.
    expect(findGaps(at(T + 600, T + 900), { from: T + 600, to: T + 660 }, 60)).toEqual([]);
  });

  it("counts half a minute without a block as a gap on a per-block range", () => {
    // Ten blocks a second, then nothing for a minute: a gap, and the second of
    // blocks around it is not.
    const blocks = at(T, T + 0, T + 1, T + 2, T + 62, T + 63);
    const gaps = findGaps(blocks, { from: T, to: T + 63 }, BLOCK_GAP_S);
    expect(gaps).toEqual([{ from: T + 32, to: T + 62, kind: "interior" }]);
    expect(findGaps(at(T, T + 20, T + 45), { from: T, to: T + 45 }, BLOCK_GAP_S)).toEqual([]);
  });
});

describe("breaking a line across a gap", () => {
  const rows = [
    { t: T, fee: 1, floor: 2 },
    { t: T + 300, fee: 3, floor: 4 },
  ];
  const gaps: Gap[] = [{ from: T + 60, to: T + 300, kind: "interior" }];

  it("puts one empty row inside the hole, with every key null and none of them zero", () => {
    const out = withGapBreaks(rows, gaps);
    expect(out).toHaveLength(3);
    expect(out[1]).toEqual({ t: T + 60, gapRow: true, fee: null, floor: null });
    expect(isGapRow(out[1] as Record<string, unknown>)).toBe(true);
    expect(isGapRow(out[0] as unknown as Record<string, unknown>)).toBe(false);
  });

  it("leaves the rows alone where there is nothing to break", () => {
    expect(withGapBreaks(rows, [])).toEqual(rows);
    expect(withGapBreaks([], gaps)).toEqual([]);
    // The leading and trailing spans need no row: the line simply starts and
    // ends where the data does.
    expect(withGapBreaks(rows, [{ from: T - 600, to: T, kind: "leading" }])).toEqual(rows);
  });

  it("keeps the rows in time order however many holes there are", () => {
    const many = [{ t: T }, { t: T + 300 }, { t: T + 900 }];
    const out = withGapBreaks(many, [
      { from: T + 60, to: T + 300, kind: "interior" },
      { from: T + 360, to: T + 900, kind: "interior" },
    ]);
    expect(out.map((r) => r.t)).toEqual([T, T + 60, T + 300, T + 360, T + 900]);
  });
});

describe("what the shading says", () => {
  it("names the leading span for what it is, and an interior hole for what it is", () => {
    expect(gapBandLabel({ from: 0, to: 1, kind: "leading" })).toBe(NOT_INDEXED_LABEL);
    expect(gapBandLabel({ from: 0, to: 1, kind: "trailing" })).toBe(NOT_INDEXED_LABEL);
    expect(gapBandLabel({ from: 0, to: 1, kind: "interior" })).toBe("gap");
  });

  it("captions a chart with where the history begins and how many holes it has", () => {
    expect(gapCaption([], null)).toBeNull();
    expect(gapCaption([{ from: 0, to: T, kind: "leading" }], T)).toBe(`Shaded: not indexed yet, history before ${formatDateTime(T)}`);
    expect(gapCaption([{ from: 0, to: T, kind: "leading" }], null)).toBe("Shaded: not indexed yet");
    expect(gapCaption([{ from: T, to: T + 60, kind: "interior" }], T)).toBe("Shaded: 1 gap with no buckets");
    expect(
      gapCaption(
        [
          { from: T, to: T + 60, kind: "interior" },
          { from: T + 120, to: T + 180, kind: "interior" },
          { from: T + 240, to: T + 300, kind: "trailing" },
        ],
        T,
      ),
    ).toBe("Shaded: 2 gaps with no buckets · not indexed yet, up to the end of the range");
  });

  it("says a range with nothing in it is empty, and when indexing began where that is known", () => {
    expect(emptyRangeNote(null)).toBe(NOTHING_INDEXED);
    expect(emptyRangeNote(T)).toBe(`${NOTHING_INDEXED} Indexing began ${formatDateTime(T)}.`);
  });
});

describe("the model a range chart draws from", () => {
  it("derives the window, the step, the gaps and the first indexed point in one pass", () => {
    const points = at(T + 7200, T + 7260);
    const model = gapModel({ from: T, to: T + 7320, resolution: "1m" }, points);
    expect(model.step).toBe(60);
    expect(model.window).toEqual({ from: T, to: T + 7320 });
    expect(model.gaps).toEqual([{ from: T, to: T + 7200, kind: "leading" }]);
    expect(model.first).toBe(T + 7200);
    expect(model.empty).toBe(false);
    expect(firstIndexed([])).toBeNull();
  });

  it("takes an explicit step over the resolution's, for points on their own cadence", () => {
    const model = gapModel({ from: T, to: T + 600, resolution: "batch" }, at(T, T + 20, T + 40), 60);
    expect(model.step).toBe(60);
    expect(model.gaps).toEqual([{ from: T + 100, to: T + 600, kind: "trailing" }]);
  });

  it("is empty for a range that indexed nothing", () => {
    const model = gapModel({ from: T, to: T + 3600, resolution: "1m" }, []);
    expect(model.empty).toBe(true);
    expect(model.first).toBeNull();
    expect(model.gaps).toEqual([{ from: T, to: T + 3600, kind: "leading" }]);
    // And the constant a chart with no series at all draws from shades nothing.
    expect(NO_GAPS.gaps).toEqual([]);
    expect(NO_GAPS.empty).toBe(true);
  });
});
