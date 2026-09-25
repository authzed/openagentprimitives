import { type ResourceRow } from "../../lib/api";

// rowId is the canonical detail-route id for a projector row: "namespace/name"
// for namespaced resources, bare "name" for cluster-scoped ones. It is the
// inverse of the id EntityLink encodes into the route, so a detail page can find
// its row inside the flat /config/{resource} list. List views (AgentsView,
// ResourceTable) build EntityLink ids with it, and useConfigDetail uses it to
// re-qualify a bare-name deep-link once it recovers the row's namespace.
export function rowId(r: ResourceRow): string {
  return r.namespace ? `${r.namespace}/${r.name}` : r.name;
}
