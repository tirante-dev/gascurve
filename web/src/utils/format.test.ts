import { describe, expect, it } from "vitest";
import {
  bipsToMultiplier,
  bipsToX,
  costWei,
  formatAgo,
  formatDateTime,
  formatDuration,
  formatEth,
  formatGas,
  formatGwei,
  formatInteger,
  formatPercent,
  formatSecondsOfTarget,
  formatSignificant,
  formatTick,
  formatTime,
  formatUtc,
  formatUtcOffset,
  formatZone,
  formatX,
  secondsOfTarget,
  shortAddress,
  shortHash,
  toBigInt,
  weiToEthNumber,
  weiToGweiNumber,
} from "./format";

describe("toBigInt", () => {
  it("accepts strings, numbers and bigints", () => {
    expect(toBigInt("123")).toBe(123n);
    expect(toBigInt(" 42 ")).toBe(42n);
    expect(toBigInt("")).toBe(0n);
    expect(toBigInt(7.9)).toBe(7n);
    expect(toBigInt(5n)).toBe(5n);
  });
});

describe("formatGwei", () => {
  it("uses precision that follows the magnitude", () => {
    expect(formatGwei("20000000")).toBe("0.02");
    expect(formatGwei("399726000")).toBe("0.3997");
    expect(formatGwei("5310000000")).toBe("5.31");
    expect(formatGwei("123456789000")).toBe("123");
    expect(formatGwei("1234567890000")).toBe("1,235");
    expect(formatGwei("0")).toBe("0");
    expect(formatGwei(2369608n)).toBe("0.00237");
  });
});

describe("formatSignificant", () => {
  it("handles edge cases", () => {
    expect(formatSignificant(Number.NaN, 3)).toBe("n/a");
    expect(formatSignificant(0, 3)).toBe("0");
    expect(formatSignificant(12345.6, 3)).toBe("12,346");
    expect(formatSignificant(0.000001234, 2)).toBe("0.0000012");
  });
});

describe("formatGas", () => {
  it("uses separators below a million and suffixes above", () => {
    expect(formatGas(402113)).toBe("402,113");
    expect(formatGas(60_000_000)).toBe("60M");
    expect(formatGas(3_111_506)).toBe("3.11M");
    expect(formatGas(2_970_000_000)).toBe("2.97G");
    expect(formatGas(11_194_391_810_886)).toBe("11.2T");
    expect(formatGas(123_456_789)).toBe("123M");
    expect(formatGas(-1500)).toBe("-1,500");
    expect(formatGas(Number.POSITIVE_INFINITY)).toBe("n/a");
    expect(formatGas(0)).toBe("0");
  });
});

describe("formatInteger", () => {
  it("adds separators", () => {
    expect(formatInteger(55_812_345)).toBe("55,812,345");
    expect(formatInteger(-12)).toBe("-12");
    expect(formatInteger(Number.NaN)).toBe("n/a");
  });
});

describe("formatDuration and seconds of target", () => {
  it("picks a single unit", () => {
    expect(formatDuration(0.0518)).toBe("0.05 s");
    expect(formatDuration(12.34)).toBe("12.3 s");
    expect(formatDuration(270)).toBe("4.5 min");
    expect(formatDuration(3600 * 77.7)).toBe("77.7 h");
    expect(formatDuration(86400 * 5.2)).toBe("5.2 d");
    expect(formatDuration(-5)).toBe("-5 s");
    expect(formatDuration(Number.NaN)).toBe("n/a");
  });
  it("converts backlog to seconds of target", () => {
    expect(secondsOfTarget(3_111_506, 60_000_000)).toBeCloseTo(0.0519, 3);
    expect(secondsOfTarget(100, 0)).toBe(0);
    expect(formatSecondsOfTarget(11_194_391_810_886, 40_000_000)).toBe("77.7 h");
  });
});

describe("bips helpers", () => {
  it("formats multipliers", () => {
    expect(bipsToMultiplier(199_900)).toBe("19.99×");
    expect(bipsToMultiplier(10_000)).toBe("1.00×");
    expect(bipsToMultiplier(1_234_500)).toBe("123.5×");
    expect(bipsToMultiplier(12_345_000)).toBe("1235×");
    expect(bipsToMultiplier(Number.NaN)).toBe("n/a");
  });
  it("formats x", () => {
    expect(bipsToX(32_425)).toBeCloseTo(3.2425);
    expect(formatX(32_425)).toBe("3.2425");
    expect(formatX(34, 2)).toBe("0.00");
  });
  it("formats percentages", () => {
    expect(formatPercent(0.9499)).toBe("95.0%");
    expect(formatPercent(0.5, 0)).toBe("50%");
    expect(formatPercent(Number.NaN)).toBe("n/a");
  });
});

describe("formatEth", () => {
  it("never goes through Number", () => {
    expect(formatEth("402000000000000000000")).toBe("402 ETH");
    expect(formatEth("10706123456789012345678")).toBe("10,706.1 ETH");
    expect(formatEth("1500000000000000000")).toBe("1.5 ETH");
    expect(formatEth("8393700000000")).toBe("0.00000839 ETH");
    expect(formatEth("190000000000000")).toBe("0.00019 ETH");
    expect(formatEth("0")).toBe("0 ETH");
    expect(formatEth("-190000000000000", { unit: false })).toBe("-0.00019");
    expect(formatEth(1n)).toBe("0.000000000000000001 ETH");
    expect(formatEth("123456789012345678901234567890")).toBe("123,456,789,012.3 ETH");
  });
});

describe("wei conversions", () => {
  it("scales to gwei and ETH", () => {
    expect(weiToGweiNumber("399726000")).toBeCloseTo(0.399726);
    expect(weiToEthNumber("1500000000000000000")).toBeCloseTo(1.5);
    expect(weiToEthNumber("123456789012345678901234")).toBeCloseTo(123456.789012);
  });
  it("keeps values below one microether", () => {
    // A 500 Gwei batch is 5e-7 ETH, not zero.
    expect(weiToEthNumber("500000000000")).toBeCloseTo(5e-7, 12);
    expect(weiToEthNumber(1n)).toBeCloseTo(1e-18, 24);
    expect(weiToEthNumber("0")).toBe(0);
    let sum = 0;
    for (let i = 0; i < 200; i++) sum += weiToEthNumber("500000000000");
    expect(sum).toBeCloseTo(0.0001, 9);
    expect(weiToEthNumber("-500000000000")).toBeCloseTo(-5e-7, 12);
  });
  it("computes cost in wei", () => {
    expect(costWei(21000, "399726000")).toBe("8394246000000");
  });
});

describe("addresses and hashes", () => {
  it("shortens", () => {
    expect(shortAddress("0x5a2B80a9aBcDeF0123456789abcdef0123459BE7")).toBe("0x5a2B80…9BE7");
    expect(shortAddress("0xabc")).toBe("0xabc");
    expect(shortHash("0x1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef")).toBe(
      "0x12345678…cdef",
    );
    expect(shortHash("0xshort")).toBe("0xshort");
  });
});

describe("time formatting (TZ pinned to America/Chicago)", () => {
  it("renders local time", () => {
    // 2026-09-06T07:20:00Z is 02:20 in Chicago (CDT, UTC-5).
    expect(formatTime("2026-09-06T07:20:00Z")).toBe("02:20:00");
    expect(formatTime(1788679200)).toBe("02:20:00");
    expect(formatTime(new Date("2026-09-06T07:20:00Z"))).toBe("02:20:00");
    expect(formatDateTime("2026-09-06T07:20:00Z")).toBe("2026-09-06 02:20 CDT");
    expect(formatUtc("2026-09-06T07:20:00Z")).toBe("2026-09-06 07:20 UTC");
    expect(formatTime("garbage")).toBe("n/a");
    expect(formatDateTime("garbage")).toBe("n/a");
    expect(formatUtc("garbage")).toBe("n/a");
  });
  it("disambiguates the repeated hour at the daylight-saving change with the zone", () => {
    // 01:30 happens twice on 2026-11-01 in Chicago: first in CDT, then in CST.
    const first = formatDateTime("2026-11-01T06:30:00Z");
    const second = formatDateTime("2026-11-01T07:30:00Z");
    expect(first).toBe("2026-11-01 01:30 CDT");
    expect(second).toBe("2026-11-01 01:30 CST");
    expect(first).not.toBe(second);
  });
  it("falls back to a numeric offset when Intl has no zone name", () => {
    const d = new Date("2026-09-06T07:20:00Z");
    expect(formatUtcOffset(d)).toBe("UTC-05:00");
    expect(formatUtcOffset(new Date("2026-01-06T07:20:00Z"))).toBe("UTC-06:00");
    expect(formatZone(d, null)).toBe("UTC-05:00");
    const blank = { formatToParts: () => [{ type: "timeZoneName", value: " " }] } as unknown as Intl.DateTimeFormat;
    expect(formatZone(d, blank)).toBe("UTC-05:00");
    const throwing = {
      formatToParts: () => {
        throw new Error("no ICU");
      },
    } as unknown as Intl.DateTimeFormat;
    expect(formatZone(d, throwing)).toBe("UTC-05:00");
    expect(formatZone(d)).toBe("CDT");
  });
  it("formats axis ticks by span", () => {
    expect(formatTick(1788679200, 3600)).toBe("02:20");
    expect(formatTick(1788679200, 86400)).toBe("02:20");
    expect(formatTick(1788679200, 30 * 86400)).toBe("09-06");
  });
  it("formats relative time", () => {
    expect(formatAgo(0.2)).toBe("now");
    expect(formatAgo(3.7)).toBe("3 s ago");
    expect(formatAgo(125)).toBe("2 min ago");
    expect(formatAgo(5400)).toBe("1.5 h ago");
    expect(formatAgo(200000)).toBe("2.3 d ago");
    expect(formatAgo(-1)).toBe("n/a");
  });
});
