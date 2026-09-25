import * as React from "react";
import { ResourceSection } from "./ResourceSection";
import { COLUMNS } from "./columns";

// The two identity sub-surfaces, shown as a horizontal tab bar. AgentIdentity is
// how agents authenticate to upstreams; ClusterIdentityProvider is human login
// for the admin UI & CLI. Both tabs link each row to its per-item detail page.
const TABS = [
  { id: "identities", label: "Agent identities", sub: "how agents authenticate to upstreams" },
  { id: "providers", label: "Login providers", sub: "human login for the admin UI & CLI" },
] as const;

type TabId = (typeof TABS)[number]["id"];

// IdentityView shows BOTH AgentIdentity and ClusterIdentityProvider as two tabs
// in one nav view. The Agent-identities tab adds a credential-type facet filter
// (static / oauth / federated) on top of the shared status filter.
export function IdentityView({ apiBase }: { apiBase: string }) {
  const [tab, setTab] = React.useState<TabId>("identities");
  const meta = TABS.find((t) => t.id === tab) ?? TABS[0];

  return (
    <div className="space-y-3">
      <div role="tablist" className="inline-flex h-9 items-center gap-1 rounded-lg bg-muted p-1 text-muted-foreground">
        {TABS.map((t) => {
          const on = tab === t.id;
          return (
            <button
              key={t.id}
              type="button"
              role="tab"
              aria-selected={on}
              onClick={() => setTab(t.id)}
              className={[
                "inline-flex items-center justify-center whitespace-nowrap rounded-md px-3 py-1 text-sm font-medium transition-all",
                on ? "bg-background text-foreground shadow" : "hover:text-foreground",
              ].join(" ")}
            >
              {t.label}
            </button>
          );
        })}
      </div>

      <p className="text-xs text-muted-foreground">{meta.sub}</p>

      {tab === "identities" ? (
        <ResourceSection
          apiBase={apiBase}
          resource="identities"
          entity="identity"
          columns={COLUMNS.identities}
          facet={{ key: "cred", label: "Type" }}
          note="AgentIdentity — managed via oap identity · setup flows: github_pat, oauth_mcp, federated (ID-JAG)"
          emptyText="No agent identities configured yet."
        />
      ) : (
        <ResourceSection
          apiBase={apiBase}
          resource="providers"
          entity="provider"
          columns={COLUMNS.providers}
          note="ClusterIdentityProvider — managed via oap idp"
          emptyText="No login providers configured yet."
        />
      )}
    </div>
  );
}
