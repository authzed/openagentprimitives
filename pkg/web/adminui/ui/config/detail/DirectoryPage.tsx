import { ConfigDetail } from "./ConfigDetail";

// DirectoryPage is the RelationshipSource detail — one directory sync (Slack,
// GitHub, 1Password) feeding SpiceDB. Tabs = Overview (kind / credential /
// endpoint / interval) + the backend's Sync, Config, Scopes (what the sync
// actually wrote into SpiceDB — absent on a parked, kind-conflicted source),
// Scope errors (a capped sample of the last pass's per-scope failures, absent
// when it had none) and Health.
//
// Health now appears for a PARTIAL failure too, not only a hard one: a source
// can be Ready=True and still have failed most of a directory, which is what
// the Scope errors tab beside it is there to show.
//
// Deliberately NOT SourcePage: "source" is SkillSource/ClusterSkillSource
// (skills synced from git), a different CRD behind a different slug. The two
// are unrelated despite both being "sources" in English — see
// directoryProjector's own note.
export function DirectoryPage({ apiBase, id, tab }: { apiBase: string; id: string; tab?: string }) {
  return (
    <ConfigDetail
      apiBase={apiBase}
      resource="directory"
      entity="directory"
      id={id}
      tab={tab}
      backRoute={{ type: "view", view: "directory" }}
    />
  );
}
