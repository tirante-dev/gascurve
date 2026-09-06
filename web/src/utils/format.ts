// Formatting helpers. Wei is always handled as BigInt or a decimal string;
// Number is only used once a value has been scaled down to gwei or ETH.

const WEI_PER_GWEI = 1_000_000_000n;
const WEI_PER_ETH = 1_000_000_000_000_000_000n;
const WEI_PER_MICROETH = 1_000_000_000_000n;

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

/** ETH as a float with microether resolution. Division happens in BigInt. */
export function weiToEthNumber(wei: string | bigint): number {
  const w = toBigInt(wei);
  return Number(w / WEI_PER_MICROETH) / 1e6;
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

/** Gas amounts: thousands separators below 1M, then M, G and T suffixes. */
export function formatGas(gas: number): string {
  if (!Number.isFinite(gas)) return "n/a";
  const abs = Math.abs(gas);
  const sign = gas < 0 ? "-" : "";
  if (abs < 1_000_000) return sign + withThousands(Math.round(abs).toString());
  const units: [number, string][] = [
    [1e12, "T"],
    [1e9, "G"],
    [1e6, "M"],
  ];
  for (const [scale, suffix] of units) {
    if (abs >= scale) {
      const scaled = abs / scale;
      const decimals = scaled >= 100 ? 0 : scaled >= 10 ? 1 : 2;
      return sign + stripTrailingZeros(scaled.toFixed(decimals)) + suffix;
    }
  }
  return sign + withThousands(Math.round(abs).toString());
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

/** Local date and time, YYYY-MM-DD HH:MM. */
export function formatDateTime(value: string | number | Date): string {
  const d = toDate(value);
  if (Number.isNaN(d.getTime())) return "n/a";
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}`;
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
