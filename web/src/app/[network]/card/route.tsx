import { ImageResponse } from "next/og";
import { serverApiBase } from "@/lib/api/core";
import { getLive } from "@/lib/api/live";
import { CARD_COLORS, CARD_DIAL_H, CARD_DIAL_W, CARD_PAD_X, CARD_TONE_COLORS, READOUT_GAP, cardDialSvg, cardNumerals, cardReading, svgDataUri, type CardReading } from "@/lib/card";
import { CARD_SIZE, networkDisplayName, SITE_NAME, SITE_URL } from "@/lib/seo";

/** The line under the wordmark on the static card too, so the two read as one set. */
const KICKER = "NITRO BASE FEE TELEMETRY";

// Rendered per request, not at build time: a card whose whole point is to be current has nothing to gain
// from being cached with the build.
export const dynamic = "force-dynamic";

/**
 * How long the card waits on the api. Short, and without the usual retries: a crawler unfurling a link
 * gives up long before the client's ten seconds, and a card without a reading beats no card at all.
 */
const CARD_TIMEOUT_MS = 3_000;

/** How long a platform may hold the card before asking again. A minute keeps a re-share current without
 * putting a render behind every hit of a page that embeds it. */
const CARD_MAX_AGE_S = 60;

async function readingFor(network: string): Promise<CardReading> {
  try {
    return cardReading(await getLive(network, { baseUrl: serverApiBase(SITE_URL), timeoutMs: CARD_TIMEOUT_MS, retries: 0 }));
  } catch {
    return null;
  }
}

function Wordmark() {
  return (
    <div
      style={{
        display: "flex",
        fontSize: 44,
        letterSpacing: 4,
        backgroundImage: `linear-gradient(90deg, ${CARD_COLORS.accent}, ${CARD_COLORS.chromeMid} 55%, ${CARD_COLORS.accent2})`,
        backgroundClip: "text",
        color: "transparent",
      }}
    >
      {SITE_NAME}
    </div>
  );
}

/**
 * The gauge, as an image with the ring's numerals laid over it. The numerals are not in the SVG because
 * that is rasterised with its own fallback face; see the note in lib/card.
 */
function Dial({ multiplier }: { multiplier: number | null }) {
  return (
    <div style={{ display: "flex", position: "relative", width: CARD_DIAL_W, height: CARD_DIAL_H }}>
      {/* eslint-disable-next-line @next/next/no-img-element -- the card is rasterised, not served to a browser, so there is no Image to optimise. */}
      <img src={svgDataUri(cardDialSvg(multiplier))} width={CARD_DIAL_W} height={CARD_DIAL_H} alt="" />
      {cardNumerals().map((numeral) => (
        <div
          key={numeral.text}
          style={{ position: "absolute", display: "flex", justifyContent: "center", left: numeral.x - 40, top: numeral.y - 14, width: 80, fontSize: 22, color: CARD_COLORS.ink2 }}
        >
          {numeral.text}
        </div>
      ))}
    </div>
  );
}

function Readout({ reading }: { reading: CardReading }) {
  return (
    <div style={{ display: "flex", flexDirection: "column", flexGrow: 1 }}>
      <div style={{ display: "flex", fontSize: 19, letterSpacing: 3, color: CARD_COLORS.label }}>BASE FEE NOW</div>
      <div style={{ display: "flex", alignItems: "flex-end", marginTop: 10 }}>
        <div style={{ display: "flex", fontSize: reading === null ? 96 : reading.fee.size, lineHeight: 1, color: CARD_COLORS.ink }}>{reading === null ? "n/a" : reading.fee.text}</div>
        {reading?.feeUnit === true && <div style={{ display: "flex", fontSize: 32, marginLeft: 12, marginBottom: 6, color: CARD_COLORS.ink2 }}>gwei</div>}
      </div>
      {reading === null ? (
        <div style={{ display: "flex", marginTop: 22, fontSize: 24, color: CARD_COLORS.ink3 }}>no reading right now</div>
      ) : (
        <div style={{ display: "flex", alignItems: "baseline", marginTop: 22 }}>
          <div style={{ display: "flex", fontSize: reading.multiplier.size, color: CARD_TONE_COLORS[reading.tone] }}>{reading.multiplier.text}</div>
          <div style={{ display: "flex", fontSize: 19, letterSpacing: 2, marginLeft: 12, color: CARD_COLORS.ink3 }}>OVER FLOOR</div>
        </div>
      )}
    </div>
  );
}

/**
 * The card for a network, drawn when it is asked for. A route of its own rather than an opengraph-image
 * file, because that convention only covers the segment it sits in: the chart and how-it-works pages under
 * /[network] would have been left with no card at all. lib/seo points every one of them here.
 */
export async function GET(_request: Request, { params }: { params: Promise<{ network: string }> }) {
  const { network } = await params;
  const reading = await readingFor(network);
  const host = SITE_URL.replace(/^https?:\/\//, "");
  const footer = reading === null ? "" : `block ${reading.block}  ·  floor ${reading.floor} gwei`;
  return new ImageResponse(
    (
      <div
        style={{
          width: "100%",
          height: "100%",
          display: "flex",
          flexDirection: "column",
          padding: `52px ${CARD_PAD_X}px`,
          backgroundColor: CARD_COLORS.page,
          backgroundImage: `linear-gradient(135deg, ${CARD_COLORS.panel} 0%, ${CARD_COLORS.page} 55%, #1a0940 100%)`,
        }}
      >
        <div style={{ display: "flex", alignItems: "center", justifyContent: "space-between" }}>
          <div style={{ display: "flex", flexDirection: "column" }}>
            <Wordmark />
            <div style={{ display: "flex", marginTop: 8, fontSize: 15, letterSpacing: 4, color: CARD_COLORS.label }}>{KICKER}</div>
          </div>
          <div style={{ display: "flex", fontSize: 26, color: CARD_COLORS.ink2 }}>{networkDisplayName(network)}</div>
        </div>
        <div style={{ display: "flex", alignItems: "center", flexGrow: 1, marginTop: 8 }}>
          <Dial multiplier={reading === null ? null : reading.value} />
          <div style={{ display: "flex", flexGrow: 1, marginLeft: READOUT_GAP }}>
            <Readout reading={reading} />
          </div>
        </div>
        <div style={{ display: "flex", alignItems: "center", justifyContent: "space-between", fontSize: 21, color: CARD_COLORS.ink3 }}>
          <div style={{ display: "flex" }}>{footer}</div>
          <div style={{ display: "flex" }}>{host}</div>
        </div>
      </div>
    ),
    { ...CARD_SIZE, headers: { "cache-control": `public, max-age=${CARD_MAX_AGE_S}` } },
  );
}
