import type { Metadata } from "next";
import { Suspense } from "react";
import { ChartDetail } from "@/components/ChartDetail";
import { getChartView } from "@/lib/chartViews";
import { networkDisplayName, pageMetadata } from "@/lib/seo";
import { isChainIdParam } from "@/utils/network";

// Props are typed by hand rather than with Next's generated PageProps helper so
// the standalone typecheck (which excludes .next) sees the same shape as the build.
type Props = { params: Promise<{ network: string; chart: string }> };

export async function generateMetadata({ params }: Props): Promise<Metadata> {
  const { network, chart } = await params;
  const name = networkDisplayName(network);
  const view = getChartView(chart);
  const path = `/${encodeURIComponent(network)}/charts/${encodeURIComponent(chart)}`;
  // A chart id that is not one of ours has nothing to index: the page says so
  // and the route stays out of the index whichever form names the network.
  if (view === null) return pageMetadata({ title: `Chart not found on ${name}`, description: `No such chart on ${name}.`, path, canonical: false });
  return pageMetadata({ title: `${view.title} on ${name}`, description: view.description, path, canonical: !isChainIdParam(network) });
}

export default async function Page({ params }: Props) {
  const { network, chart } = await params;
  // ChartDetail reads ?range= and ?constraint= with useSearchParams, which has
  // to sit inside a Suspense boundary: the page around it is prerendered and
  // the chart itself is filled in on the client.
  return (
    <Suspense fallback={null}>
      <ChartDetail network={network} chart={chart} />
    </Suspense>
  );
}
