import { NewWorkspaceForm } from "@/components/new-workspace";

export default async function NewPage({ searchParams }: { searchParams: Promise<{ host_id?: string | string[] }> }) {
  const { host_id } = await searchParams;
  return <NewWorkspaceForm initialHostID={typeof host_id === "string" ? host_id : ""}/>;
}
