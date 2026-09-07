// Formatting helpers. Wei is always handled as BigInt or a decimal string;
// Number is only used once a value has been scaled down to gwei or ETH.

import type { EthUsd } from "@/types";

const WEI_PER_GWEI = 1_000_000_000n;
const WEI_PER_ETH = 1_000_000_000_000_000_000n;

export function toBigInt(value: string | number | bigint): bigint {
  if (typeof value === "bigint") return value;
  if (typeof value === "number") return BigInt(Math.trunc(value));
  const trimmed = value.trim();
  if (trimmed === "") return 0n;
  return BigInt(trimmed);
}

function stripTrailingZeros(s: string): string {
  if (!s.includes(".")) return s;
  return s.replace(/0+$/, "").replace(/\.$/, "");
}

function withThousands(intPart: string): string {
  return intPart.replace(/\B(?=(\d{3})+(?!\d))/g, ",");
}

/** Number of gwei as a float. Safe for fee values (well below 2^53 wei). */
export function weiToGweiNumber(wei: string | bigint): number {
  const w = toBigInt(wei);
  const whole = w / WEI_PER_GWEI;
  const frac = w % WEI_PER_GWEI;
  return Number(whole) + Number(frac) / 1e9;
}

/**
 * ETH as a float. The whole ether part and the wei remainder are converted
 * separately, so amounts below one microether keep their value (a 500 Gwei
 * batch is 5e-7 ETH, not 0) and only float rounding is lost.
 */
export function weiToEthNumber(wei: string | bigint): number {
  const w = toBigInt(wei);
  const whole = w / WEI_PER_ETH;
  const frac = w % WEI_PER_ETH;
  return Number(whole) + Number(frac) / 1e18;
}

/** Formats a float with `sig` significant digits, without exponent notation. */
export function formatSignificant(value: number, sig: number): string {
  if (!Number.isFinite(value)) return "n/a";
  if (value === 0) return "0";
  const abs = Math.abs(value);
  if (abs >= 1000) {
    return withThousands(Math.round(value).toString());
  }
  const magnitude = Math.floor(Math.log10(abs));
  const decimals = Math.max(0, sig - 1 - magnitude);
  return stripTrailingZeros(value.toFixed(Math.min(decimals, 20)));
}

/** 0.02, 0.3997, 5.31, 123: gwei with precision that follows the magnitude. */
export function formatGwei(wei: string | bigint): string {
  const gwei = weiToGweiNumber(wei);
  if (gwei === 0) return "0";
  if (gwei >= 1) return formatSignificant(gwei, 3);
  return formatSignificant(gwei, 4);
}

const GAS_UNITS: [number, string][] = [
  [1e12, "T"],
  [1e9, "G"],
  [1e6, "M"],
];

/**
 * A gas amount scaled into its band, with the decimals that band prescribes
 * (two below 10, one below 100, none above) before any zero stripping, and the
 * SI prefix of the band it landed in. Rounding that carries the value into the
 * next band or prefix ("999.6M", "9.996M") is re-banded, so the result always
 * has the band's character count.
 */
function scaleGas(abs: number): { text: string; prefix: string } {
  if (abs < 1_000_000) return { text: withThousands(Math.round(abs).toString()), prefix: "" };
  for (const [scale, prefix] of GAS_UNITS) {
    if (abs < scale) continue;
    const scaled = abs / scale;
    const text = scaled.toFixed(gasDecimals(scaled));
    const rounded = Number(text);
    if (rounded >= 1000 && scale < 1e12) return scaleGas(rounded * scale);
    return { text: gasDecimals(rounded) === gasDecimals(scaled) ? text : rounded.toFixed(gasDecimals(rounded)), prefix };
  }
  return { text: withThousands(Math.round(abs).toString()), prefix: "" };
}

/**
 * The band a gas figure of this size is shown in: what to divide by, and the
 * SI prefix that goes on the unit. An axis takes its band from its top tick,
 * so every label on it is a bare figure in one unit.
 */
export function gasScale(gas: number): { divisor: number; prefix: string } {
  const abs = Math.abs(gas);
  for (const [scale, prefix] of GAS_UNITS) {
    if (abs >= scale) return { divisor: scale, prefix };
  }
  return { divisor: 1, prefix: "" };
}

function gasDecimals(scaled: number): number {
  return scaled >= 100 ? 0 : scaled >= 10 ? 1 : 2;
}

/** A figure and the unit it is in, kept apart so a tile can set the two at different sizes. */
export type UnitParts = { value: string; unit: string };

/** A figure and its unit joined by the space that always sits between them. */
export function withUnit(parts: UnitParts): string {
  return parts.unit === "" ? parts.value : `${parts.value} ${parts.unit}`;
}

/**
 * The same text with every space made non-breaking. Chart axis ticks are laid
 * out by a renderer that wraps on ordinary spaces, and "60 Mgas" split over
 * two lines is not a tick label; this keeps a figure and its unit together.
 */
export function unbroken(text: string): string {
  return text.replace(/ /g, "\u00a0");
}

/**
 * A gas amount as its figure and its unit, the SI prefix on the unit and never
 * on the number: 11.2 and "Tgas", 20.5 and "Mgas", 812,345 and "gas". `fixed`
 * keeps the band's trailing zeros ("60.0", not "60"), so an animated figure
 * holds one character count per band and never shifts its neighbours.
 */
export function gasParts(gas: number, fixed = false): UnitParts {
  if (!Number.isFinite(gas)) return { value: "n/a", unit: "" };
  const sign = gas < 0 ? "-" : "";
  const { text, prefix } = scaleGas(Math.abs(gas));
  return { value: sign + (fixed || prefix === "" ? text : stripTrailingZeros(text)), unit: `${prefix}gas` };
}

/** Gas amounts with the SI prefix on the unit: "812,345 gas", "20.5 Mgas", "11.2 Tgas". */
export function formatGas(gas: number): string {
  return withUnit(gasParts(gas));
}

/**
 * formatGas without zero stripping ("60.0 Mgas", not "60 Mgas"), so an
 * animated backlog keeps one character count per band.
 */
export function formatGasFixed(gas: number): string {
  return withUnit(gasParts(gas, true));
}

/** A gas rate as its figure and its unit: 40.0 and "Mgas/s". */
export function gasPerSecondParts(gasPerSecond: number, fixed = false): UnitParts {
  const parts = gasParts(gasPerSecond, fixed);
  return { value: parts.value, unit: parts.unit === "" ? "" : `${parts.unit}/s` };
}

/** A gas rate with the SI prefix on the unit: "60 Mgas/s", "812,345 gas/s". */
export function formatGasPerSecond(gasPerSecond: number): string {
  return withUnit(gasPerSecondParts(gasPerSecond));
}

/** formatGasPerSecond without zero stripping: "40.0 Mgas/s". */
export function formatGasPerSecondFixed(gasPerSecond: number): string {
  return withUnit(gasPerSecondParts(gasPerSecond, true));
}

/**
 * A non-negative finite number as a plain decimal string, never in exponent
 * notation: the shortest representation that round-trips, with the point
 * moved for the exponent. This is the string the value reads as, which is
 * what the rounding below has to work on.
 */
function plainDecimal(abs: number): string {
  const text = abs.toString();
  const parts = /^(\d*)(?:\.(\d*))?e([+-]?\d+)$/i.exec(text);
  if (!parts) return text;
  const digits = (parts[1] ?? "") + (parts[2] ?? "");
  const point = (parts[1] ?? "").length + Number(parts[3]);
  // JavaScript only prints an exponent below 1e-6 or at 1e21 and above, so
  // the point always lands outside the digits, never inside them.
  if (point <= 0) return `0.${"0".repeat(-point)}${digits}`;
  return digits + "0".repeat(Math.max(0, point - digits.length));
}

/** A digit string plus one, carrying left and growing at the front when every digit was a nine. */
function increment(digits: string): string {
  const out = digits.split("");
  for (let i = out.length - 1; i >= 0; i--) {
    if (out[i] === "9") {
      out[i] = "0";
      continue;
    }
    out[i] = String(Number(out[i]) + 1);
    return out.join("");
  }
  return `1${out.join("")}`;
}

/**
 * `value` rounded half away from zero to `decimals` places, as an unsigned
 * decimal string. The rounding runs on the decimal representation as a scaled
 * integer, so a literal that sits a fraction below the decimal half in binary
 * (9.9995 is stored as 9.99949999...) still rounds the way it is written, and
 * a tie never depends on which side of the half the double landed.
 */
export function roundDecimal(value: number, decimals: number): string {
  if (!Number.isFinite(value)) return "n/a";
  const plain = plainDecimal(Math.abs(value));
  const dot = plain.indexOf(".");
  const int = dot < 0 ? plain : plain.slice(0, dot);
  const frac = dot < 0 ? "" : plain.slice(dot + 1);
  if (frac.length <= decimals) return decimals === 0 ? int : `${int}.${frac.padEnd(decimals, "0")}`;
  const kept = int + frac.slice(0, decimals);
  const digits = frac.charCodeAt(decimals) >= "5".charCodeAt(0) ? increment(kept) : kept;
  const split = digits.length - decimals;
  const rounded = digits.slice(0, split);
  return decimals === 0 ? rounded : `${rounded}.${digits.slice(split)}`;
}

/**
 * A number with the decimals its magnitude band prescribes. The band is
 * chosen after rounding ("9.9996" rounds to "10.000" in the 1 to 10 band, so
 * it takes the 10 to 100 band's "10.00" instead), so every value in a band has
 * the same character count. Rounding is decimal, not binary, so an exact half
 * as written rounds up. Thousands separators apply above 1000.
 */
function fixedByBand(value: number, decimals: (abs: number) => number): string {
  if (!Number.isFinite(value)) return "n/a";
  const abs = Math.abs(value);
  const d = decimals(abs);
  const first = roundDecimal(abs, d);
  const rounded = Number(first);
  const text = decimals(rounded) === d ? first : roundDecimal(abs, decimals(rounded));
  const dot = text.indexOf(".");
  const int = dot < 0 ? text : text.slice(0, dot);
  const sign = value < 0 && rounded !== 0 ? "-" : "";
  return sign + withThousands(int) + (dot < 0 ? "" : text.slice(dot));
}

/** Reserved widths in ch for the fixed formatters: the widest band their live values move in. */
export const FIXED_WIDTH_CH = { gwei: 6, multiplier: 5, gasPerSecond: 4, eth: 10, gas: 5, x: 6, usd: 6 } as const;

/** Decimals of a gwei figure by band: below 1 four, 1 to 10 three, 10 to 100 two, otherwise one. */
function gweiDecimals(gwei: number): number {
  if (gwei < 1) return 4;
  if (gwei < 10) return 3;
  if (gwei < 100) return 2;
  return 1;
}

/**
 * A gwei figure (already scaled, as the tween holds it) at a fixed width per
 * band: "0.3997", "5.310", "12.34", "123.4". For tooltips and tables use
 * formatGwei, which trims.
 */
export function formatGweiFixed(gwei: number): string {
  return fixedByBand(gwei, gweiDecimals);
}

/** A multiplier over the floor with two decimals always: "19.99". The × sits outside. */
export function formatMultiplierFixed(multiplier: number): string {
  return fixedByBand(multiplier, () => 2);
}

/** Decimals for three significant digits at a fixed count per decade: 8.39e-6 → 8, 0.0599 → 4, 1.5 → 2. */
function ethDecimals(eth: number): number {
  if (eth === 0) return 2;
  return Math.max(0, Math.min(18, 2 - Math.floor(Math.log10(eth))));
}

/** An ETH amount (as a float, the tween's unit) with a fixed decimal count per decade: "0.00000839". The unit sits outside. */
export function formatEthFixed(eth: number): string {
  return fixedByBand(eth, ethDecimals);
}

/** Decimals of a USD figure: two below 100, one from 100 up, so the band keeps one character count. */
function usdDecimals(usd: number): number {
  return usd < 100 ? 2 : 1;
}

/** A USD amount at a fixed width per band: "0.04", "12.35", "210.4", "1,240.5". The "$" sits outside. */
export function formatUsdFixed(usd: number): string {
  return fixedByBand(usd, usdDecimals);
}

/** A quote older than this is not money any more: the fee has moved on and the price has not. */
export const ETH_USD_MAX_AGE_MS = 10 * 60 * 1000;

/**
 * The ETH/USD price a fee may be shown in, or null when there is none to use:
 * no quote at all, an unparseable or non-positive one, or one fetched more
 * than ETH_USD_MAX_AGE_MS before `nowMs`. A null sends the caller back to ETH.
 */
export function freshUsdPrice(ethUsd: EthUsd | null | undefined, nowMs: number): number | null {
  if (!ethUsd) return null;
  const at = Date.parse(ethUsd.at);
  if (Number.isNaN(at) || nowMs - at > ETH_USD_MAX_AGE_MS) return null;
  const price = Number(ethUsd.price);
  return Number.isFinite(price) && price > 0 ? price : null;
}

/** Plain integer with thousands separators. */
export function formatInteger(n: number): string {
  if (!Number.isFinite(n)) return "n/a";
  const sign = n < 0 ? "-" : "";
  return sign + withThousands(Math.round(Math.abs(n)).toString());
}

/** Durations in a compact single unit: 0.05 s, 12 s, 4.5 min, 78 h, 3.2 d. */
export function formatDuration(seconds: number): string {
  if (!Number.isFinite(seconds)) return "n/a";
  const s = Math.abs(seconds);
  const sign = seconds < 0 ? "-" : "";
  if (s < 1) return `${sign}${stripTrailingZeros(s.toFixed(2))} s`;
  if (s < 60) return `${sign}${stripTrailingZeros(s.toFixed(1))} s`;
  if (s < 3600) return `${sign}${stripTrailingZeros((s / 60).toFixed(1))} min`;
  if (s < 100 * 3600) return `${sign}${stripTrailingZeros((s / 3600).toFixed(1))} h`;
  return `${sign}${stripTrailingZeros((s / 86400).toFixed(1))} d`;
}

/** A backlog expressed as how long the constraint's target takes to pay it off. */
export function secondsOfTarget(backlog: number, target: number): number {
  if (target <= 0) return 0;
  return backlog / target;
}

/**
 * How long a rate needs to clear an amount, in the band the value asks for:
 * seconds below a minute (two decimals under one second, where a short
 * window's backlog lives), then minutes, then hours. Hours never roll over
 * into days: a backlog is read against a working span, not a calendar. The
 * decimal count is fixed per band, so an animated figure keeps one character
 * count and never shifts what sits beside it.
 */
export function formatDrainTime(seconds: number): string {
  if (!Number.isFinite(seconds)) return "n/a";
  const s = Math.max(0, seconds);
  if (s < 1) return `${s.toFixed(2)} s`;
  if (s < 60) return `${s.toFixed(1)} s`;
  if (s < 3600) return `${(s / 60).toFixed(1)} min`;
  return `${(s / 3600).toFixed(1)} h`;
}

/**
 * What a backlog figure means, as time: "= 77.8 h at 40 Mgas/s", the span the
 * chain would have to run at exactly the target for to drain it. Without a
 * positive target there is no rate to drain at and nothing to say.
 */
export function formatDrainEquivalence(backlog: number, target: number): string {
  if (!Number.isFinite(backlog) || !Number.isFinite(target) || target <= 0) return "n/a";
  return `= ${formatDrainTime(secondsOfTarget(backlog, target))} at ${formatGasPerSecond(target)}`;
}

/** Multiplier over the floor from basis points: 19.99x. */
export function bipsToMultiplier(bips: number): string {
  const m = bips / 10_000;
  if (!Number.isFinite(m)) return "n/a";
  const decimals = m >= 1000 ? 0 : m >= 100 ? 1 : 2;
  return `${m.toFixed(decimals)}×`;
}

/** The exponent x as a decimal from basis points. */
export function bipsToX(bips: number): number {
  return bips / 10_000;
}

export function formatX(bips: number, decimals = 4): string {
  return bipsToX(bips).toFixed(decimals);
}

/** Percentage of a share, 0 to 1, formatted like 95.0%. */
export function formatPercent(share: number, decimals = 1): string {
  if (!Number.isFinite(share)) return "n/a";
  return `${(share * 100).toFixed(decimals)}%`;
}

/**
 * ETH from a wei string using BigInt only.
 * Large balances keep up to two decimals, small amounts keep three significant digits.
 */
export function formatEth(wei: string | bigint, options: { unit?: boolean } = {}): string {
  const w = toBigInt(wei);
  const negative = w < 0n;
  const abs = negative ? -w : w;
  const whole = abs / WEI_PER_ETH;
  const frac = (abs % WEI_PER_ETH).toString().padStart(18, "0");
  const sign = negative ? "-" : "";
  let out: string;
  if (whole >= 1000n) {
    out = withThousands(whole.toString()) + "." + frac.slice(0, 1);
  } else if (whole >= 1n) {
    out = whole.toString() + "." + frac.slice(0, 4);
  } else if (abs === 0n) {
    out = "0";
  } else {
    const firstNonZero = frac.search(/[1-9]/);
    const keep = Math.min(18, firstNonZero + 3);
    out = "0." + frac.slice(0, keep);
  }
  out = stripTrailingZeros(out);
  if (out === "0" && abs > 0n) out = "<0.000000000000000001";
  return sign + out + (options.unit === false ? "" : " ETH");
}

/** 0x5a2B80a9...9BE7 */
export function shortAddress(address: string): string {
  if (address.length <= 14) return address;
  return `${address.slice(0, 8)}…${address.slice(-4)}`;
}

export function shortHash(hash: string): string {
  if (hash.length <= 16) return hash;
  return `${hash.slice(0, 10)}…${hash.slice(-4)}`;
}

function pad(n: number): string {
  return n.toString().padStart(2, "0");
}

function toDate(value: string | number | Date): Date {
  if (value instanceof Date) return value;
  if (typeof value === "number") return new Date(value * 1000);
  return new Date(value);
}

/** Local wall-clock time, HH:MM:SS. */
export function formatTime(value: string | number | Date): string {
  const d = toDate(value);
  if (Number.isNaN(d.getTime())) return "n/a";
  return `${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`;
}

/** Numeric offset of the local zone at `d`, like UTC-05:00. */
export function formatUtcOffset(d: Date): string {
  const offset = -d.getTimezoneOffset();
  const sign = offset < 0 ? "-" : "+";
  const abs = Math.abs(offset);
  return `UTC${sign}${pad(Math.floor(abs / 60))}:${pad(abs % 60)}`;
}

/**
 * Short zone label for the local zone at `d`: the abbreviation when Intl has
 * one (CDT, CST, GMT+2), otherwise the numeric offset. Two instants that
 * share a wall-clock time across a daylight-saving change get different labels.
 */
export function formatZone(d: Date, formatter: Intl.DateTimeFormat | null = zoneFormatter()): string {
  if (formatter) {
    try {
      const part = formatter.formatToParts(d).find((p) => p.type === "timeZoneName");
      if (part && part.value.trim() !== "") return part.value;
    } catch {
      // Fall through to the numeric offset.
    }
  }
  return formatUtcOffset(d);
}

function zoneFormatter(): Intl.DateTimeFormat | null {
  try {
    return new Intl.DateTimeFormat("en-US", { timeZoneName: "short" });
  } catch {
    return null;
  }
}

/** Local date and time with the zone, YYYY-MM-DD HH:MM CDT, so timestamps stay unambiguous across daylight-saving changes. */
export function formatDateTime(value: string | number | Date): string {
  const d = toDate(value);
  if (Number.isNaN(d.getTime())) return "n/a";
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())} ${formatZone(d)}`;
}

/** UTC date and time, for values that must match block explorers. */
export function formatUtc(value: string | number | Date): string {
  const d = toDate(value);
  if (Number.isNaN(d.getTime())) return "n/a";
  return `${d.getUTCFullYear()}-${pad(d.getUTCMonth() + 1)}-${pad(d.getUTCDate())} ${pad(d.getUTCHours())}:${pad(d.getUTCMinutes())} UTC`;
}

/** Axis tick label for a unix timestamp, chosen by the range being drawn. */
export function formatTick(unixSeconds: number, spanSeconds: number): string {
  const d = new Date(unixSeconds * 1000);
  if (spanSeconds <= 2 * 3600) return `${pad(d.getHours())}:${pad(d.getMinutes())}`;
  if (spanSeconds <= 2 * 86400) return `${pad(d.getHours())}:${pad(d.getMinutes())}`;
  return `${pad(d.getMonth() + 1)}-${pad(d.getDate())}`;
}

/** "3 s ago", "2 min ago". */
export function formatAgo(seconds: number): string {
  if (!Number.isFinite(seconds) || seconds < 0) return "n/a";
  if (seconds < 1) return "now";
  if (seconds < 60) return `${Math.floor(seconds)} s ago`;
  if (seconds < 3600) return `${Math.floor(seconds / 60)} min ago`;
  if (seconds < 86400) return `${(seconds / 3600).toFixed(1)} h ago`;
  return `${(seconds / 86400).toFixed(1)} d ago`;
}

/** Cost in wei of `gas` gas at `baseFeeWei`, as a decimal string. */
export function costWei(gas: number, baseFeeWei: string | bigint): string {
  return (BigInt(gas) * toBigInt(baseFeeWei)).toString();
}
