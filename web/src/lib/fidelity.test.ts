import { describe, expect, it } from "vitest";
import {
  fidelityBandLabel,
  fidelityBands,
  fidelityCaption,
  fidelityNote,
  fidelityOf,
  fidelityRowNote,
  fidelityRuns,
  unvouchedKind,
  unvouchedNote,
  versionRange,
  type Versioned,
} from "./fidelity";

const at = (t: number, point: Versioned) => ({ t, ...point });

describe("fidelityOf", () => {
  it("reads what the api says", () => {
    expect(fidelityOf({ replayFidelity: "boundary" })).toBe("boundary");
    expect(fidelityOf({ replayFidelity: "unverified" })).toBe("unverified");
    expect(fidelityOf({ replayFidelity: "unknown" })).toBe("unknown");
  });

  // An api too old to send the field is not evidence of a boundary, so its buckets draw plainly.
  it("treats an api that says nothing as a bucket to draw", () => {
    expect(fidelityOf({})).toBe("verified");
    expect(fidelityOf({ replayFidelity: undefined })).toBe("verified");
  });
});

describe("unvouchedKind", () => {
  it("marks only the two answers that question the numbers", () => {
    expect(unvouchedKind({ replayFidelity: "boundary" })).toBe("boundary");
    expect(unvouchedKind({ replayFidelity: "unverified" })).toBe("unverified");
    expect(unvouchedKind({ replayFidelity: "verified" })).toBeNull();
    // History recorded before the version was stored is most of an existing database, and shading all
    // of it would say something the data does not.
    expect(unvouchedKind({ replayFidelity: "unknown" })).toBeNull();
  });
});

describe("versionRange", () => {
  it("names one version, or the span across an upgrade", () => {
    expect(versionRange({ arbosVersionMin: 61, arbosVersionMax: 61 })).toBe("61");
    expect(versionRange({ arbosVersionMin: 51, arbosVersionMax: 61 })).toBe("51 to 61");
  });

  it("names nothing when the bucket recorded nothing", () => {
    expect(versionRange({})).toBeNull();
    expect(versionRange({ arbosVersionMin: null, arbosVersionMax: null })).toBeNull();
    expect(versionRange({ arbosVersionMin: 51, arbosVersionMax: null })).toBeNull();
  });
});

describe("notes", () => {
  it("says which fact a bucket is marked for", () => {
    expect(fidelityNote({ replayFidelity: "boundary", arbosVersionMin: 61, arbosVersionMax: 62 })).toBe("spans ArbOS 61 to 62; nobody has replayed through that upgrade");
    expect(fidelityNote({ replayFidelity: "unverified", arbosVersionMin: 62, arbosVersionMax: 62 })).toBe("the pricer has not been measured against ArbOS 62");
    expect(fidelityNote({ replayFidelity: "verified" })).toBeNull();
  });

  it("still says it without the version numbers", () => {
    expect(unvouchedNote("boundary", null)).toBe("spans an ArbOS upgrade; nobody has replayed through that upgrade");
    expect(unvouchedNote("unverified", null)).toBe("the pricer has not been measured against this ArbOS version");
  });

  it("reads a hovered chart row", () => {
    expect(fidelityRowNote({ fidelity: "boundary", arbosVersions: "61 to 62" })).toContain("spans ArbOS 61 to 62");
    expect(fidelityRowNote({ fidelity: "unverified", arbosVersions: 7 })).toBe("the pricer has not been measured against this ArbOS version");
    expect(fidelityRowNote({ fidelity: null })).toBeNull();
    expect(fidelityRowNote({})).toBeNull();
  });
});

describe("fidelityBands", () => {
  it("opens one band per unvouched bucket, a bucket wide", () => {
    const bands = fidelityBands(
      [at(0, { replayFidelity: "verified" }), at(60, { replayFidelity: "boundary" }), at(120, { replayFidelity: "unknown" })],
      60,
    );
    expect(bands).toEqual([{ from: 60, to: 120, kind: "boundary" }]);
  });

  it("draws none without a usable bucket width", () => {
    expect(fidelityBands([at(0, { replayFidelity: "boundary" })], 0)).toEqual([]);
    expect(fidelityBands([at(0, { replayFidelity: "boundary" })], Number.NaN)).toEqual([]);
  });
});

describe("fidelityRuns", () => {
  it("joins touching bands of one kind", () => {
    const runs = fidelityRuns([
      { from: 0, to: 60, kind: "boundary" },
      { from: 60, to: 120, kind: "boundary" },
      { from: 120, to: 180, kind: "unverified" },
    ]);
    expect(runs).toEqual([
      { from: 0, to: 120, kind: "boundary", buckets: 2 },
      { from: 120, to: 180, kind: "unverified", buckets: 1 },
    ]);
  });

  it("opens a new run across a gap", () => {
    const runs = fidelityRuns([
      { from: 0, to: 60, kind: "unverified" },
      { from: 600, to: 660, kind: "unverified" },
    ]);
    expect(runs).toHaveLength(2);
  });
});

describe("fidelityCaption", () => {
  it("says nothing when nothing is marked", () => {
    expect(fidelityCaption([])).toBeNull();
  });

  it("counts the buckets and the stretches they fall in", () => {
    const caption = fidelityCaption([
      { from: 0, to: 60, kind: "boundary" },
      { from: 600, to: 660, kind: "unverified" },
      { from: 660, to: 720, kind: "unverified" },
    ]);
    expect(caption).toBe(`Marked, replay not verified: ${fidelityBandLabel("boundary")} for 1 buckets · ${fidelityBandLabel("unverified")} for 2 buckets`);
  });

  it("counts stretches when one kind falls in several", () => {
    const caption = fidelityCaption([
      { from: 0, to: 60, kind: "boundary" },
      { from: 600, to: 660, kind: "boundary" },
    ]);
    expect(caption).toContain("in 2 stretches");
  });
});
