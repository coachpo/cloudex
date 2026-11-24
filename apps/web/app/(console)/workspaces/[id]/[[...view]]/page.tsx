import { notFound } from "next/navigation";
import { WorkspaceDetails } from "@/components/workspace-details";

export default async function WorkspacePage({params}: {params:Promise<{id:string;view?:string[]}>}) {
  const {id,view=[]}=await params;
  if (view.length>1||!["","access","terminal","git","logs","settings"].includes(view[0]??""))notFound();
  return <WorkspaceDetails id={id} view={view[0]??""}/>;
}
