import type { Metadata } from "next";
import { NetworkPage } from "@/components/NetworkPage";

// Props are typed by hand rather than with Next's generated PageProps helper so
// the standalone typecheck (which excludes .next) sees the same shape as the build.
type Props = { params: Promise<{ network: string }> };

export async function generateMetadata({ params }: Props): Promise<Metadata> {
  const { network } = await params;
  return { title: `${network} gas curve` };
}

export default async function Page({ params }: Props) {
  const { network } = await params;
  return <NetworkPage network={network} />;
}
