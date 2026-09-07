import type { Metadata, Viewport } from "next";
import { Orbitron } from "next/font/google";
import type { ReactNode } from "react";
import { CARD_IMAGE, PRIMARY_NETWORK, PRIMARY_NETWORK_NAME, SITE_DESCRIPTION, SITE_NAME, SITE_TAGLINE, SITE_TITLE, SITE_URL, absoluteUrl } from "@/lib/seo";
import "./globals.css";

export const metadata: Metadata = {
  // Every canonical link and card image below is written as a path; this is
  // what Next resolves them against. See SITE_URL for where the origin comes
  // from in a published image.
  metadataBase: new URL(SITE_URL),
  title: {
    default: SITE_TITLE,
    // Pages set the specific half and inherit the name, so a tab and a search
    // result both say which site they are from.
    template: `%s · ${SITE_NAME}`,
  },
  description: SITE_DESCRIPTION,
  applicationName: SITE_NAME,
  // No canonical here on purpose. A default in the layout is inherited by
  // every route that does not set its own, which would tell a crawler that a
  // chain id route is a duplicate of the homepage rather than of the
  // network's own page. Each route declares its own instead, page.tsx and
  // pageMetadata included.
  openGraph: {
    type: "website",
    siteName: SITE_NAME,
    url: "/",
    title: SITE_TITLE,
    description: SITE_DESCRIPTION,
    locale: "en_US",
    images: [CARD_IMAGE],
  },
  twitter: {
    card: "summary_large_image",
    title: SITE_TITLE,
    description: SITE_DESCRIPTION,
    images: [CARD_IMAGE],
  },
  robots: {
    index: true,
    follow: true,
    googleBot: { index: true, follow: true, "max-image-preview": "large", "max-snippet": -1, "max-video-preview": -1 },
  },
  category: "technology",
  other: { "format-detection": "telephone=no" },
};

export const viewport: Viewport = {
  // The two page grounds from globals.css, so the browser chrome on mobile
  // matches whichever theme the viewer lands in.
  themeColor: [
    { media: "(prefers-color-scheme: light)", color: "#faf5ff" },
    { media: "(prefers-color-scheme: dark)", color: "#0f0326" },
  ],
  colorScheme: "light dark",
};

// The display face is used for the wordmark and nothing else; it is exposed as
// a CSS variable so globals.css can compose the font stack with a fallback.
const orbitron = Orbitron({ subsets: ["latin"], weight: "700", display: "swap", variable: "--font-orbitron" });

// Applies the stored theme before paint so the page does not flash. Kept tiny
// and dependency free on purpose; the toggle in the header writes the same key.
const themeScript = `(function(){try{var t=localStorage.getItem("gascurve:theme");if(t==="light"||t==="dark"){document.documentElement.setAttribute("data-theme",t);}}catch(e){}})();`;

// Tells search engines what the site is, what it is about and where its main
// page lives. Inlined rather than fetched so it is present in the first
// response, which is all a crawler reads.
const structuredData = {
  "@context": "https://schema.org",
  "@type": "WebSite",
  name: SITE_NAME,
  alternateName: SITE_TAGLINE,
  url: SITE_URL,
  description: SITE_DESCRIPTION,
  inLanguage: "en",
  about: { "@type": "Thing", name: PRIMARY_NETWORK_NAME, sameAs: absoluteUrl(`/${PRIMARY_NETWORK}`) },
};

export default function RootLayout({ children }: { children: ReactNode }) {
  return (
    <html lang="en" className={orbitron.variable} suppressHydrationWarning>
      <head>
        <script dangerouslySetInnerHTML={{ __html: themeScript }} />
        <script type="application/ld+json" dangerouslySetInnerHTML={{ __html: JSON.stringify(structuredData) }} />
      </head>
      <body className="min-h-full antialiased">{children}</body>
    </html>
  );
}
