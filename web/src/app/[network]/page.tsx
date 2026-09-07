import type { Metadata } from "next";
import { NetworkPage } from "@/components/NetworkPage";
import { networkDisplayName, pageMetadata } from "@/lib/seo";
import { isChainIdParam } from "@/utils/network";

// Props are typed by hand rather than with Next's generated PageProps helper so
// the standalone typecheck (which excludes .next) sees the same shape as the build.
type Props = { params: Promise<{ network: string }> };

export async function generateMetadata({ params }: Props): Promise<Metadata> {
  const { network } = await params;
  const name = networkDisplayName(network);
  return pageMetadata({
    title: `${name} gas tracker: live base fee`,
    description: `Live and historical gas prices for ${name}: the Nitro base fee pricer, its constraint backlogs, owner changes, fee destinations and ArbOS-attributed batch costs.`,
    path: `/${encodeURIComponent(network)}`,
    // The api serves a network by name or by chain id, so /4663 renders the
    // same page as /robinhood. Only the name form is a canonical route.
    canonical: !isChainIdParam(network),
  });
}

export default async function Page({ params }: Props) {
  const { network } = await params;
  return <NetworkPage network={network} />;
}
