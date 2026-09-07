import type { Metadata } from "next";
import { Orbitron } from "next/font/google";
import type { ReactNode } from "react";
import "./globals.css";

export const metadata: Metadata = {
  title: "gascurve",
  description: "Live and historical gas pricing for Arbitrum Nitro chains: the multi-constraint base fee pricer, its backlogs, owner changes, fee destinations and L1 posting costs.",
};

// The display face is used for the wordmark and nothing else; it is exposed as
// a CSS variable so globals.css can compose the font stack with a fallback.
const orbitron = Orbitron({ subsets: ["latin"], weight: "700", display: "swap", variable: "--font-orbitron" });

// Applies the stored theme before paint so the page does not flash. Kept tiny
// and dependency free on purpose; the toggle in the header writes the same key.
const themeScript = `(function(){try{var t=localStorage.getItem("gascurve:theme");if(t==="light"||t==="dark"){document.documentElement.setAttribute("data-theme",t);}}catch(e){}})();`;

export default function RootLayout({ children }: { children: ReactNode }) {
  return (
    <html lang="en" className={orbitron.variable} suppressHydrationWarning>
      <head>
        <script dangerouslySetInnerHTML={{ __html: themeScript }} />
      </head>
      <body className="min-h-full antialiased">{children}</body>
    </html>
  );
}
