import { useStateQuery } from "@/api/queries";
import { ExternalLink } from "@/components/ExternalLink";
import { safeHttpUrl } from "@/lib/links";

// The owner yields its room first, so the project's own name stays readable longest.
/** The name of the project Sortie tracks, linked to the project while the state route serves its address. */
export function ProjectName() {
  const { data } = useStateQuery();
  const project = data?.config?.tracker.project;
  if (project === undefined || project === "") {
    return undefined;
  }
  const href = safeHttpUrl(data?.config?.tracker.project_url ?? null);

  const words = (
    <span
      title={project}
      className="trim-caps min-w-0 overflow-x-clip text-body-sm text-ellipsis whitespace-nowrap"
    >
      {project}
    </span>
  );
  if (href === null) {
    return <p className="flex min-w-0 text-ink-muted">{words}</p>;
  }
  return (
    <ExternalLink href={href} className="text-ink-muted">
      {words}
    </ExternalLink>
  );
}
