import type { Metadata } from "next";
import { HowItWorks } from "@/components/HowItWorks";
import { networkDisplayName, pageMetadata } from "@/lib/seo";
import { isChainIdParam } from "@/utils/network";

// Props are typed by hand rather than with Next's generated PageProps helper so
// the standalone typecheck (which excludes .next) sees the same shape as the build.
type Props = { params: Promise<{ network: string }> };

export async function generateMetadata({ params }: Props): Promise<Metadata> {
  const { network } = await params;
  const name = networkDisplayName(network);
  return pageMetadata({
    title: `How the ${name} gas fee works`,
    description: `The multi-constraint base fee pricer explained for ${name}: how each constraint's backlog moves the price, the floor it cannot fall below, and where the fee ends up.`,
    path: `/${encodeURIComponent(network)}/how-it-works`,
    canonical: !isChainIdParam(network),
  });
}

export default async function Page({ params }: Props) {
  const { network } = await params;
  return <HowItWorks network={network} />;
}
