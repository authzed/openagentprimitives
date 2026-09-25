import { useState, type ReactNode } from "react";
import type { AdminAppProps } from "./types";
import { AppShell } from "./shell/AppShell";
import { Placeholder } from "./shell/Placeholder";
import { DetailPage } from "./shell/DetailPage";
import { LiveSessions } from "./live/LiveSessions";
import { ToolCallsView } from "./live/ToolCallsView";
import { ApprovalsView } from "./live/ApprovalsView";
import { WorkshopsView } from "./live/WorkshopsView";
import { SessionPage } from "./live/SessionPage";
import { AuditExplorer } from "./audit/AuditExplorer";
import { EntityRollups } from "./audit/EntityRollups";
import { ArtifactsView } from "./audit/ArtifactsView";
import { ArtifactPage } from "./audit/ArtifactPage";
import { BudgetView } from "./audit/BudgetView";
import { MemoryView } from "./audit/MemoryView";
import { KnowledgeView } from "./audit/KnowledgeView";
import { OverviewView } from "./overview/OverviewView";
import { AgentsView } from "./config/AgentsView";
import { ToolsView } from "./config/ToolsView";
import { SkillsView } from "./config/SkillsView";
import { SourcesView } from "./config/SourcesView";
import { DirectoryView } from "./config/DirectoryView";
import { ChannelsView } from "./config/ChannelsView";
import { IdentityView } from "./config/IdentityView";
import { UsersView } from "./config/UsersView";
import { AccessView } from "./config/AccessView";
import { SettingsView } from "./config/SettingsView";
import { AgentPage } from "./config/detail/AgentPage";
import { ToolPage } from "./config/detail/ToolPage";
import { SkillPage } from "./config/detail/SkillPage";
import { SourcePage } from "./config/detail/SourcePage";
import { DirectoryPage } from "./config/detail/DirectoryPage";
import { ChannelPage } from "./config/detail/ChannelPage";
import { IdentityPage } from "./config/detail/IdentityPage";
import { ProviderPage } from "./config/detail/ProviderPage";
import { UserPage } from "./config/detail/UserPage";
import { navigate, useRoute, viewForRoute, type Route } from "./lib/router";
import type { AuditQueryRequest } from "./lib/api";
import { applyStoredTheme } from "@ap/design";

// Theme is chosen per browser (design/theme.ts); apply it before the first
// render so a light-mode viewer sees one dark frame at most, never a flicker.
applyStoredTheme();

export function AdminApp({ apiBase, currentUser }: AdminAppProps) {
  const route = useRoute();
  const [auditFilter, setAuditFilter] = useState<AuditQueryRequest>({});

  // Audit deep-link: stash the filter in local state and route to the logs
  // view (the AuditExplorer reads initialFilter). Kept as pre-router flow.
  function drillToAudit(filter: AuditQueryRequest) {
    setAuditFilter(filter);
    navigate({ type: "view", view: "logs" });
  }

  // The sidebar highlight follows the route: a detail route highlights the nav
  // view its entity lives under (an agent detail highlights "agents").
  const view = viewForRoute(route);

  const content =
    route.type === "detail" ? detailContent(route, apiBase) : viewContent(route.view);

  function viewContent(v: typeof view): ReactNode {
    switch (v) {
      case "overview":
        return <OverviewView apiBase={apiBase} />;
      case "sessions":
        return <LiveSessions apiBase={apiBase} />;
      case "toolcalls":
        return <ToolCallsView apiBase={apiBase} />;
      case "approvals":
        return (
          <ApprovalsView
            apiBase={apiBase}
            onOpenSession={(ns, name) =>
              navigate({ type: "detail", entity: "session", id: `${ns}/${name}` })
            }
          />
        );
      case "workshops":
        return <WorkshopsView apiBase={apiBase} />;
      case "logs":
        return (
          <AuditExplorer
            apiBase={apiBase}
            initialFilter={auditFilter}
            key={JSON.stringify(auditFilter)}
          />
        );
      case "sessionsAudit":
        // Session-scoped audit rollups; drill-through deep-links to logs.
        return <EntityRollups apiBase={apiBase} axis="sessions" onDrill={drillToAudit} />;
      case "budget":
        return <BudgetView apiBase={apiBase} />;
      case "artifacts":
        return <ArtifactsView apiBase={apiBase} onDrill={drillToAudit} />;
      case "memory":
        return <MemoryView apiBase={apiBase} />;
      case "knowledge":
        return <KnowledgeView apiBase={apiBase} />;
      case "agents":
        return <AgentsView apiBase={apiBase} />;
      case "tools":
        return <ToolsView apiBase={apiBase} />;
      case "skills":
        return <SkillsView apiBase={apiBase} />;
      case "sources":
        return <SourcesView apiBase={apiBase} />;
      case "directory":
        return <DirectoryView apiBase={apiBase} />;
      case "channels":
        return <ChannelsView apiBase={apiBase} />;
      case "identity":
        return <IdentityView apiBase={apiBase} />;
      case "users":
        return <UsersView apiBase={apiBase} />;
      case "access":
        return <AccessView apiBase={apiBase} />;
      case "settings":
        return <SettingsView apiBase={apiBase} />;
      default:
        return <Placeholder view={v} />;
    }
  }

  return (
    <AppShell
      view={view}
      onNavigate={(v) => navigate({ type: "view", view: v })}
      currentUser={currentUser}
      apiBase={apiBase}
    >
      {content}
    </AppShell>
  );
}

// detailContent dispatches a detail route to its per-entity page. The config
// entities (agent/tool/skill/source/directory/channel/identity/provider/user),
// session, and artifact all have real pages now; any remaining kind falls back
// to DetailPlaceholder.
//
// The `default:` arm is why a missing case is INVISIBLE: a list view whose rows
// link to a detail route still renders a coherent-looking page, so the only
// symptom of forgetting a case here is a "lands in a later batch" subtitle
// nobody is looking for. Adding an EntityKind means adding a case here, and
// AdminApp.detail.test.tsx asserts every kind in ENTITY_TO_VIEW resolves to a
// real page rather than the placeholder.
function detailContent(route: Extract<Route, { type: "detail" }>, apiBase: string): ReactNode {
  const { entity, id, tab } = route;
  switch (entity) {
    case "session":
      return <SessionPage apiBase={apiBase} id={id} tab={tab} />;
    case "agent":
      return <AgentPage apiBase={apiBase} id={id} tab={tab} />;
    case "tool":
      return <ToolPage apiBase={apiBase} id={id} tab={tab} />;
    case "skill":
      return <SkillPage apiBase={apiBase} id={id} tab={tab} />;
    case "source":
      return <SourcePage apiBase={apiBase} id={id} tab={tab} />;
    case "directory":
      return <DirectoryPage apiBase={apiBase} id={id} tab={tab} />;
    case "channel":
      return <ChannelPage apiBase={apiBase} id={id} tab={tab} />;
    case "identity":
      return <IdentityPage apiBase={apiBase} id={id} tab={tab} />;
    case "provider":
      return <ProviderPage apiBase={apiBase} id={id} tab={tab} />;
    case "user":
      return <UserPage apiBase={apiBase} id={id} tab={tab} />;
    case "artifact":
      return <ArtifactPage apiBase={apiBase} id={id} tab={tab} />;
    default:
      return <DetailPlaceholder route={route} />;
  }
}

// DetailPlaceholder is the interim shell for entity kinds whose detail page is
// not built yet — kept so their deep-links still render a coherent page rather
// than falling through to the overview.
function DetailPlaceholder({ route }: { route: Extract<Route, { type: "detail" }> }): ReactNode {
  const activeTab = route.tab ?? "overview";
  return (
    <DetailPage
      title={`${route.entity} ${route.id}`}
      subtitle="Detail page lands in a later batch"
      tabs={[{ id: "overview", label: "Overview" }]}
      activeTab={activeTab}
      onTab={(tab) => navigate({ type: "detail", entity: route.entity, id: route.id, tab })}
    >
      <p className="text-sm text-muted-foreground">Detail page lands in a later batch.</p>
    </DetailPage>
  );
}
