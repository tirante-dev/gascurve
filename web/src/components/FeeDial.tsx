"use client";

import { BANDS_LINE, DIAL_BANDS, DIAL_CX, DIAL_CY, DIAL_RADIUS, dialArc, dialPoint, dialPosition, dialTone, TONE_SENTENCE, type DialTone } from "@/lib/dial";
import { FIXED_WIDTH_CH, formatGweiFixed, formatMultiplierFixed } from "@/utils/format";
import { Figure, HoverNote } from "./primitives";

/** The colour of each band, the page's own status tones, which both themes carry. */
const TONE_COLOR: Record<DialTone, string> = {
  good: "var(--good)",
  warning: "var(--dial-warning)",
  critical: "var(--critical)",
};

const TONE_TEXT: Record<DialTone, string> = {
  good: "text-good",
  warning: "text-dial-warning",
  critical: "text-critical",
};

/** The stroke of the three bands. */
const BAND_WIDTH = 9;

/** How far from the hub the needle reaches: short of the bands, so its tip sits inside the rim. */
const NEEDLE_RADIUS = DIAL_RADIUS - BAND_WIDTH;

/**
 * The multiplier over the floor as a speedometer: three bands, green through
 * amber to red, a needle at the multiplier on a log scale from the floor to a
 * hundred times it, and the figure itself under the dial in the band's
 * colour. It stood here before as a tile on a nine-step ramp, which put the
 * everyday state of a chain somewhere hot; three bands at round thresholds
 * say plainly which state the chain is in. The needle follows the eased
 * multiplier, so it moves with the figure beside it rather than jumping once
 * a sample.
 */
export function FeeDial({ baseFeeGwei, floorGwei, multiplier }: { baseFeeGwei: number; floorGwei: string; multiplier: number }) {
  const tone = dialTone(multiplier);
  const figure = formatMultiplierFixed(multiplier);
  const tip = dialPoint(dialPosition(multiplier), NEEDLE_RADIUS);
  const lines = [`${figure}× the ${floorGwei} gwei floor`, `${formatGweiFixed(baseFeeGwei)} gwei = ${figure} × ${floorGwei} gwei`, BANDS_LINE];
  const description = `${figure} times the ${floorGwei} gwei floor, ${TONE_SENTENCE[tone]}: ${BANDS_LINE}.`;
  return (
    <div className="vw-tile rounded-md bg-surface-2 px-3 pt-2 pb-1.5" data-testid="fee-dial">
      <HoverNote lines={lines} description={description} align="end">
        <span className="flex flex-col items-center">
          <svg viewBox="0 0 100 58" className="block h-12 w-auto" aria-hidden="true" focusable="false">
            {DIAL_BANDS.map((band) => (
              <path key={band.tone} d={dialArc(band.from, band.to)} fill="none" stroke={TONE_COLOR[band.tone]} strokeWidth={BAND_WIDTH} strokeLinecap="butt" data-band={band.tone} />
            ))}
            <line x1={DIAL_CX} y1={DIAL_CY} x2={tip.x.toFixed(2)} y2={tip.y.toFixed(2)} stroke="var(--ink)" strokeWidth={2.5} strokeLinecap="round" data-testid="fee-dial-needle" />
            <circle cx={DIAL_CX} cy={DIAL_CY} r={3.5} fill="var(--ink)" />
          </svg>
          <span className={`num mt-0.5 text-xl leading-none ${TONE_TEXT[tone]}`} data-testid="fee-dial-multiplier">
            <Figure ch={FIXED_WIDTH_CH.multiplier}>{figure}</Figure>×
          </span>
          <span className="mt-1 text-[11px] font-medium uppercase tracking-[0.08em] text-ink-3">over floor</span>
        </span>
      </HoverNote>
    </div>
  );
}
