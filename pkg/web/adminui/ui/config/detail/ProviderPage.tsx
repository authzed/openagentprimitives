import { ConfigDetail } from "./ConfigDetail";

// ProviderPage is the ClusterIdentityProvider (login provider) detail — the
// human-login IdPs for the admin UI & CLI. It reuses the generic ConfigDetail
// against the /config/providers projector (which has a detail endpoint); back
// returns to the Identity view, where providers live under the Login-providers
// tab. Providers are cluster-scoped, so `id` is a bare provider name.
export function ProviderPage({ apiBase, id, tab }: { apiBase: string; id: string; tab?: string }) {
  return (
    <ConfigDetail
      apiBase={apiBase}
      resource="providers"
      entity="provider"
      id={id}
      tab={tab}
      backRoute={{ type: "view", view: "identity" }}
    />
  );
}
