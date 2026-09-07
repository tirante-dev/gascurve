import type { Metadata } from "next";
import { HowItWorks } from "@/components/HowItWorks";

// Props are typed by hand rather than with Next's generated PageProps helper so
// the standalone typecheck (which excludes .next) sees the same shape as the build.
type Props = { params: Promise<{ network: string }> };

export async function generateMetadata({ params }: Props): Promise<Metadata> {
  const { network } = await params;
  return { title: `How the ${network} fee works` };
}

export default async function Page({ params }: Props) {
  const { network } = await params;
  return <HowItWorks network={network} />;
}
