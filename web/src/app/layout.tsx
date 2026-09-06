import type { Metadata } from "next";
import type { ReactNode } from "react";
import "./globals.css";

export const metadata: Metadata = {
  title: "gascurve",
  description: "Live and historical gas pricing for Arbitrum Nitro chains: the multi-constraint base fee pricer, its backlogs, owner changes, fee destinations and L1 posting costs.",
};

// Applies the stored theme before paint so the page does not flash. Kept tiny
// and dependency free on purpose; the toggle in the header writes the same key.
const themeScript = `(function(){try{var t=localStorage.getItem("gascurve:theme");if(t==="light"||t==="dark"){document.documentElement.setAttribute("data-theme",t);}}catch(e){}})();`;

export default function RootLayout({ children }: { children: ReactNode }) {
  return (
    <html lang="en" suppressHydrationWarning>
      <head>
        <script dangerouslySetInnerHTML={{ __html: themeScript }} />
      </head>
      <body className="min-h-full antialiased">{children}</body>
    </html>
  );
}
