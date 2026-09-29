import { permanentRedirect } from "next/navigation";
import { allGuides } from "@/lib/guides";

// /docs has no page of its own: it opens the lowest-order guide, as the old
// hash router did for an empty hash.
export default async function DocsIndex() {
  const [first] = await allGuides();
  permanentRedirect(`/docs/${first.slug}`);
}
