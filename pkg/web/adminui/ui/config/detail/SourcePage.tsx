import { ConfigDetail } from "./ConfigDetail";

// SourcePage is the SkillSource / ClusterSkillSource detail. Tabs = Overview
// (repo URL / ref / subpath / resolved SHA / discovered-skill count / last sync)
// + Discovery problems (list) when the sync surfaced any.
export function SourcePage({ apiBase, id, tab }: { apiBase: string; id: string; tab?: string }) {
  return (
    <ConfigDetail
      apiBase={apiBase}
      resource="sources"
      entity="source"
      id={id}
      tab={tab}
      backRoute={{ type: "view", view: "sources" }}
    />
  );
}
