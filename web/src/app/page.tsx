"use client";

import { useRouter } from "next/navigation";
import { useEffect } from "react";
import { DEFAULT_NETWORK, readStoredNetwork } from "@/hooks/useNetwork";

/** The root sends viewers to their last network, or to Robinhood Chain. */
export default function IndexPage() {
  const router = useRouter();
  useEffect(() => {
    router.replace(`/${readStoredNetwork() ?? DEFAULT_NETWORK}`);
  }, [router]);
  return (
    <main className="mx-auto max-w-[1200px] px-4 py-16 text-ink-2">
      <p>
        Opening <a className="underline" href={`/${DEFAULT_NETWORK}`}>/{DEFAULT_NETWORK}</a>.
      </p>
    </main>
  );
}
