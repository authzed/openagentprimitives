import { ConfigDetail } from "./ConfigDetail";

// IdentityPage is the AgentIdentity detail. Tabs = Overview (refresh threshold /
// resolved count / last setup+refresh) + Credentials (each declared credential
// badged with its type and whether it currently resolves).
export function IdentityPage({ apiBase, id, tab }: { apiBase: string; id: string; tab?: string }) {
  return (
    <ConfigDetail
      apiBase={apiBase}
      resource="identities"
      entity="identity"
      id={id}
      tab={tab}
      backRoute={{ type: "view", view: "identity" }}
    />
  );
}
