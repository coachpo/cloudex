import { Workspaces } from "@/components/workspaces";

export default async function WorkspacesPage({ searchParams }: { searchParams: Promise<{ host_id?: string | string[] }> }) {
  const { host_id } = await searchParams;
  const hostID = typeof host_id === "string" ? host_id : "";
  return <Workspaces key={hostID} initialHostID={hostID}/>;
}
