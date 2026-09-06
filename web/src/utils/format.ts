// Formatting helpers. Wei is always handled as BigInt or a decimal string;
// Number is only used once a value has been scaled down to gwei or ETH.

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
 * A gas amount scaled to its unit with the decimals of its band (two below
 * 10, one below 100, none above), before any zero stripping. Rounding that
 * carries the value into the next band or unit ("999.6M", "9.996M") is
 * re-banded so the result always has the band's character count.
 */
function gasParts(abs: number): { text: string; suffix: string } {
  if (abs < 1_000_000) return { text: withThousands(Math.round(abs).toString()), suffix: "" };
  for (const [scale, suffix] of GAS_UNITS) {
    if (abs < scale) continue;
    const scaled = abs / scale;
    const text = scaled.toFixed(gasDecimals(scaled));
    const rounded = Number(text);
    if (rounded >= 1000 && scale < 1e12) return gasParts(rounded * scale);
    return { text: gasDecimals(rounded) === gasDecimals(scaled) ? text : rounded.toFixed(gasDecimals(rounded)), suffix };
  }
  return { text: withThousands(Math.round(abs).toString()), suffix: "" };
}

function gasDecimals(scaled: number): number {
  return scaled >= 100 ? 0 : scaled >= 10 ? 1 : 2;
}

/** Gas amounts: thousands separators below 1M, then M, G and T suffixes. */
export function formatGas(gas: number): string {
  if (!Number.isFinite(gas)) return "n/a";
  const sign = gas < 0 ? "-" : "";
  const { text, suffix } = gasParts(Math.abs(gas));
  return sign + (suffix === "" ? text : stripTrailingZeros(text)) + suffix;
}

/**
 * formatGas without zero stripping ("60.0M", not "60M"), so an animated
 * backlog keeps one character count per band and never shifts its neighbours.
 */
export function formatGasFixed(gas: number): string {
  if (!Number.isFinite(gas)) return "n/a";
  const sign = gas < 0 ? "-" : "";
  const { text, suffix } = gasParts(Math.abs(gas));
  return sign + text + suffix;
}

/**
 * A number with the decimals its magnitude band prescribes. The band is
 * chosen after rounding ("9.9996" rounds to "10.000" in the 1 to 10 band, so
 * it takes the 10 to 100 band's "10.00" instead), so every value in a band has
 * the same character count. Thousands separators apply above 1000.
 */
function fixedByBand(value: number, decimals: (abs: number) => number): string {
  if (!Number.isFinite(value)) return "n/a";
  const abs = Math.abs(value);
  let d = decimals(abs);
  const rounded = Number(abs.toFixed(d));
  if (decimals(rounded) !== d) d = decimals(rounded);
  const text = abs.toFixed(d);
  const dot = text.indexOf(".");
  const int = dot < 0 ? text : text.slice(0, dot);
  const sign = value < 0 && rounded !== 0 ? "-" : "";
  return sign + withThousands(int) + (dot < 0 ? "" : text.slice(dot));
}

/** Reserved widths in ch for the fixed formatters: the widest band their live values move in. */
export const FIXED_WIDTH_CH = { gwei: 6, multiplier: 5, gasPerSecond: 4, eth: 10, gas: 6, x: 6 } as const;

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

/** Gas per second in millions with one decimal always: "44.1" for 44,100,000. The "M" sits outside. */
export function formatGasPerSecondFixed(gasPerSecond: number): string {
  return fixedByBand(gasPerSecond / 1e6, () => 1);
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

export function formatSecondsOfTarget(backlog: number, target: number): string {
  return formatDuration(secondsOfTarget(backlog, target));
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
