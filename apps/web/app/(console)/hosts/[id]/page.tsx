import { HostDetail } from "@/components/host-detail";

export default async function HostPage({ params }: { params: Promise<{ id: string }> }) {
  const { id } = await params;
  return <HostDetail id={id}/>;
}
