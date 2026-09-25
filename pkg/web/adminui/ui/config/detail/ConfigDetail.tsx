import * as React from "react";
import { Alert, AlertDescription } from "@ap/design";
import { DetailPage, type DetailTab } from "../../shell/DetailPage";
import { navigate, type EntityKind, type Route } from "../../lib/router";
import { ResourceName } from "../../lib/ResourceName";
import { CopyCmd } from "../../lib/CopyCmd";
import { ApiError, getConfigDetail, getConfigResource, type ResourceDetail, type Section } from "../../lib/api";
import { DegradedBox, Field, StatusPill } from "./shared";
import { SectionRenderer } from "./SectionRenderer";
import { rowId } from "./useResourceRow";

// ExtraTab is a frontend-only tab a page injects between Overview and the
// backend-provided section tabs (AgentPage's live Sessions). render receives the
// loaded detail so the tab can key off its scalars (e.g. the agent class name).
export interface ExtraTab {
  id: string;
  label: string;
  render: (detail: ResourceDetail) => React.ReactNode;
}

export interface ConfigDetailProps {
  apiBase: string;
  // resource is the /config/{resource} slug; entity is the route EntityKind used
  // for tab navigation (they differ: "tools" vs "tool").
  resource: string;
  entity: EntityKind;
  id: string;
  tab?: string;
  backRoute: Route;
  extraTabs?: ExtraTab[];
  // subtitle overrides the default (no subtitle) — e.g. a user's decoded subject.
  subtitle?: (detail: ResourceDetail) => React.ReactNode;
}

interface DetailState {
  detail: ResourceDetail | null;
  loading: boolean;
  error: string | null;
  notFound: boolean;
}

const INITIAL: DetailState = { detail: null, loading: true, error: null, notFound: false };

// useConfigDetail fetches GET {apiBase}/config/{resource}/{id} and tracks
// loading / notfound (404) / error. Guarded with an active flag so a response
// landing after unmount (or an id change) is dropped rather than setting state
// on a dead page.
//
// Bare-name resolution: the detail route id is normally "<ns>/<name>", but the
// Overview "Tokens by agent class" bars, the Budget "By agent class" keys, and
// the audit rollups all deep-link with only a BARE class name (no namespace).
// The detail endpoint would split a bare id into ns="" and 404 on a namespaced
// AgentClass. So when `id` has no "/", we first list the resource, recover the
// matching row's full "<ns>/<name>", and fetch by that. A bare id with no
// matching row (or an already-qualified "ns/name" id) falls straight through to
// the direct fetch, which 404s cleanly when the resource genuinely doesn't exist.
function useConfigDetail(apiBase: string, resource: string, id: string): DetailState {
  const [state, setState] = React.useState<DetailState>(INITIAL);

  React.useEffect(() => {
    let active = true;
    setState(INITIAL);

    const resolveAndFetch = async (): Promise<ResourceDetail> => {
      if (!id.includes("/")) {
        const rows = await getConfigResource(apiBase, resource);
        const match = rows.find((r) => r.name === id);
        if (match) return getConfigDetail(apiBase, resource, rowId(match));
      }
      return getConfigDetail(apiBase, resource, id);
    };

    resolveAndFetch()
      .then((d) => {
        if (active) setState({ detail: d, loading: false, error: null, notFound: false });
      })
      .catch((e: Error) => {
        if (!active) return;
        if (e instanceof ApiError && e.status === 404) {
          setState({ detail: null, loading: false, error: null, notFound: true });
        } else {
          setState({ detail: null, loading: false, error: e.message, notFound: false });
        }
      });
    return () => {
      active = false;
    };
  }, [apiBase, resource, id]);

  return state;
}

// Overview is the first tab of every config detail page: the description
// (prominent), the DegradedBox (full statusReason when unhealthy), a compact
// status/scope summary, the backend's curated "overview" Section (absorbed here
// so it doesn't duplicate the synthetic Overview tab), and the read-only manage
// command (when non-empty).
function Overview({ detail, section }: { detail: ResourceDetail; section?: Section }) {
  return (
    <div className="flex flex-col gap-5">
      {detail.description && <p className="text-base text-foreground">{detail.description}</p>}
      <DegradedBox status={detail.status} reason={detail.statusReason} />
      <div className="flex flex-wrap gap-8">
        <Field label="Status">
          <StatusPill status={detail.status} reason={detail.statusReason} />
        </Field>
        <Field label="Scope">
          <span className="text-sm capitalize text-foreground">{detail.scope}</span>
        </Field>
        {detail.namespace && (
          <Field label="Namespace">
            <span className="font-mono text-xs text-foreground">{detail.namespace}</span>
          </Field>
        )}
      </div>
      {section && <SectionRenderer section={section} />}
      {detail.manageCmd && (
        <Field label="Manage">
          <CopyCmd text={detail.manageCmd} />
        </Field>
      )}
    </div>
  );
}

// ConfigDetail is the shared shell for every config detail page: it loads the
// rich ResourceDetail via the config-detail endpoint, renders the DetailPage
// chrome (title, status pill, back-to-list) with tabs = Overview + any injected
// extra tabs + one tab per backend Section, surfaces loading/notfound/error, and
// renders the active tab's body (Overview scalars, an ExtraTab, or a
// SectionRenderer). Each concrete page is a thin wrapper supplying the slug,
// entity, back route, and optional extra tabs.
export function ConfigDetail({
  apiBase,
  resource,
  entity,
  id,
  tab,
  backRoute,
  extraTabs = [],
  subtitle,
}: ConfigDetailProps) {
  const { detail, loading, error, notFound } = useConfigDetail(apiBase, resource, id);

  // A backend "overview" Section is folded into the synthetic Overview tab
  // (shared id) rather than shown as a duplicate tab. Reserved ids (Overview +
  // any injected extra tab) are also excluded from the section-tab list so a
  // section can never shadow them.
  const reserved = new Set<string>(["overview", ...extraTabs.map((t) => t.id)]);
  const overviewSection = detail?.sections?.find((s) => s.id === "overview");
  const sectionTabs = (detail?.sections ?? []).filter((s) => !reserved.has(s.id));

  const tabs: DetailTab[] = [
    { id: "overview", label: "Overview" },
    ...extraTabs.map((t) => ({ id: t.id, label: t.label })),
    ...sectionTabs.map((s) => ({ id: s.id, label: s.label })),
  ];
  const tabIds = new Set(tabs.map((t) => t.id));
  // Fall back to Overview when the URL ?tab= isn't (yet) a known tab — during
  // load section tabs don't exist, and a stale deep-link may name a dropped tab.
  const activeTab = tab && tabIds.has(tab) ? tab : "overview";
  const onTab = (t: string) => navigate({ type: "detail", entity, id, tab: t });

  const heading = detail ? (
    <ResourceName ns={detail.namespace} name={detail.name} />
  ) : (
    <ResourceName id={id} />
  );
  const sub = detail && subtitle ? subtitle(detail) : undefined;

  const body = (): React.ReactNode => {
    if (loading) return <p className="text-sm text-muted-foreground">Loading…</p>;
    if (error) {
      return (
        <Alert variant="destructive">
          <AlertDescription>
            Could not load {resource}: {error}
          </AlertDescription>
        </Alert>
      );
    }
    if (notFound || !detail) return <p className="text-sm text-muted-foreground">Not found: {id}</p>;
    if (activeTab === "overview") return <Overview detail={detail} section={overviewSection} />;
    const extra = extraTabs.find((t) => t.id === activeTab);
    if (extra) return extra.render(detail);
    const section = sectionTabs.find((s) => s.id === activeTab);
    if (section) return <SectionRenderer section={section} />;
    return <Overview detail={detail} section={overviewSection} />;
  };

  return (
    <DetailPage
      title={heading}
      subtitle={sub}
      backRoute={backRoute}
      tabs={tabs}
      activeTab={activeTab}
      onTab={onTab}
      headerRight={detail ? <StatusPill status={detail.status} reason={detail.statusReason} /> : undefined}
    >
      {body()}
    </DetailPage>
  );
}
