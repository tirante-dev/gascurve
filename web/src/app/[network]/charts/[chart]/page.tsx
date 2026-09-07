import type { Metadata } from "next";
import { Suspense } from "react";
import { ChartDetail } from "@/components/ChartDetail";
import { getChartView } from "@/lib/chartViews";

// Props are typed by hand rather than with Next's generated PageProps helper so
// the standalone typecheck (which excludes .next) sees the same shape as the build.
type Props = { params: Promise<{ network: string; chart: string }> };

export async function generateMetadata({ params }: Props): Promise<Metadata> {
  const { network, chart } = await params;
  const view = getChartView(chart);
  return view === null ? { title: `Chart not found on ${network}` } : { title: `${view.title} on ${network}`, description: view.description };
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
