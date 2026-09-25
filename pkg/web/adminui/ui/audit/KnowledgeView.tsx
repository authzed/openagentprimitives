import * as React from "react";
import { Network, Search, X } from "lucide-react";
import { Alert, AlertDescription, Badge } from "@ap/design";
import {
  kgSearch, kgEntity, kgFacts, kgRelated, kgCommunities,
  type KGFact, type KGEntity, type KGCommunity,
} from "../lib/api";

// EntityDrill is the selected-entity panel: the entity plus its facts and
// related entities, fetched together when an entity chip is clicked.
interface EntityDrill {
  uuid: string;
  loading: boolean;
  entity: KGEntity | null;
  facts: KGFact[];
  related: KGEntity[];
  error?: string;
}

// EntityChip is a clickable entity pill (the prototype's "Top entities" look),
// drilling into the entity's facts + related entities.
function EntityChip({ uuid, label, onOpen }: { uuid: string; label: string; onOpen: (u: string) => void }) {
  if (!uuid) return <span className="font-mono text-xs text-muted-foreground">{label || "—"}</span>;
  return (
    <button
      type="button"
      onClick={() => onOpen(uuid)}
      className="inline-flex items-center gap-2 rounded-full border bg-background px-3 py-1 font-mono text-xs hover:bg-accent hover:text-accent-foreground"
    >
      <span className="h-1.5 w-1.5 rounded-full bg-state" />
      {label || uuid}
    </button>
  );
}

function FactList({ facts, onOpen }: { facts: KGFact[]; onOpen: (u: string) => void }) {
  if (facts.length === 0) {
    return <p className="text-sm text-muted-foreground">No facts.</p>;
  }
  return (
    <ul className="space-y-2">
      {facts.map((f) => (
        <li key={f.uuid} className="rounded-md border bg-background p-3">
          <div className="text-sm text-foreground">{f.fact || f.name}</div>
          <div className="mt-1.5 flex flex-wrap items-center gap-1.5">
            <EntityChip uuid={f.fromEntity} label={f.fromEntity} onOpen={onOpen} />
            <span className="text-muted-foreground">→</span>
            <EntityChip uuid={f.toEntity} label={f.toEntity} onOpen={onOpen} />
            {f.validAt && (
              <span className="ml-1 font-mono text-[11px] text-muted-foreground">
                {new Date(f.validAt).toLocaleDateString()}
              </span>
            )}
          </div>
        </li>
      ))}
    </ul>
  );
}

// KnowledgeView is the Audit › Knowledge panel: a graph-aware fact search,
// entity drill-in (facts + related entities), and a communities section. It
// degrades to a clean "not configured" panel when the operator has no
// --graphiti-endpoint (the KGProvider is nil and the API returns
// available:false) — never an error page.
export function KnowledgeView({ apiBase }: { apiBase: string }) {
  const [available, setAvailable] = React.useState<boolean | null>(null);
  const [note, setNote] = React.useState("");
  const [loadError, setLoadError] = React.useState<string | null>(null);
  const [communities, setCommunities] = React.useState<KGCommunity[]>([]);

  const [input, setInput] = React.useState("");
  const [query, setQuery] = React.useState("");
  const [facts, setFacts] = React.useState<KGFact[] | null>(null);
  const [searchError, setSearchError] = React.useState<string | null>(null);

  const [drill, setDrill] = React.useState<EntityDrill | null>(null);
  // Tracks the entity whose drill-in is currently in flight, so a slow earlier
  // click can't overwrite a faster later one with stale data.
  const openUUID = React.useRef<string | null>(null);

  // Initial probe: communities doubles as the availability check — it needs no
  // input, and its `available` flag drives the degraded panel.
  React.useEffect(() => {
    let active = true;
    kgCommunities(apiBase)
      .then((d) => {
        if (!active) return;
        setAvailable(d.available);
        setNote(d.note ?? "");
        setCommunities(d.communities ?? []);
        setLoadError(d.error ?? null);
      })
      .catch((e: Error) => { if (active) { setAvailable(true); setLoadError(e.message); } });
    return () => { active = false; };
  }, [apiBase]);

  // Fact search runs when the submitted query changes.
  React.useEffect(() => {
    if (!query) { setFacts(null); setSearchError(null); return; }
    let active = true;
    kgSearch(apiBase, query)
      .then((d) => { if (active) { setFacts(d.facts ?? []); setSearchError(d.error ?? null); } })
      .catch((e: Error) => { if (active) { setFacts([]); setSearchError(e.message); } });
    return () => { active = false; };
  }, [apiBase, query]);

  const openEntity = React.useCallback((uuid: string) => {
    if (!uuid) return;
    openUUID.current = uuid;
    setDrill({ uuid, loading: true, entity: null, facts: [], related: [] });
    Promise.all([kgEntity(apiBase, uuid), kgFacts(apiBase, uuid), kgRelated(apiBase, uuid)])
      .then(([e, f, r]) => {
        // Bail if a later click superseded this one — the ref holds the most
        // recent uuid, so a slower earlier response can't clobber it.
        if (openUUID.current !== uuid) return;
        setDrill({
          uuid,
          loading: false,
          entity: e.entity ?? null,
          facts: f.facts ?? [],
          related: r.entities ?? [],
          error: e.error || f.error || r.error || undefined,
        });
      })
      .catch((err: Error) => {
        if (openUUID.current !== uuid) return;
        setDrill({ uuid, loading: false, entity: null, facts: [], related: [], error: err.message });
      });
  }, [apiBase]);

  const onSubmit = (e: React.FormEvent) => {
    e.preventDefault();
    setQuery(input.trim());
  };

  // Degraded — KG/graphiti not configured. A clean panel, not an error page.
  if (available === false) {
    return (
      <div className="rounded-xl border border-dashed bg-card p-6 shadow">
        <div className="flex items-center gap-2 text-sm font-semibold text-foreground">
          <Network className="h-4 w-4 text-muted-foreground" /> Knowledge graph not configured
        </div>
        <p className="mt-2 text-sm text-muted-foreground">
          {note || "The knowledge graph is not available."} Set{" "}
          <span className="font-mono">--graphiti-endpoint</span> on the operator to enable graph-aware
          entity, fact, and community search.
        </p>
        <p className="mt-3 text-[11px] text-muted-foreground">
          oap kg search / entity / facts / related / communities
        </p>
      </div>
    );
  }

  if (available === null && !loadError) {
    return <p className="text-sm text-muted-foreground">Loading knowledge graph…</p>;
  }

  const searching = query !== "";

  return (
    <div className="space-y-4">
      {loadError && (
        <Alert variant="destructive">
          <AlertDescription>Could not load knowledge graph: {loadError}</AlertDescription>
        </Alert>
      )}

      <form onSubmit={onSubmit} className="flex items-center gap-2">
        <label className="flex h-9 flex-1 items-center gap-2 rounded-md border border-input bg-background px-3 text-xs">
          <Search className="h-3.5 w-3.5 text-muted-foreground" />
          <input
            type="search"
            value={input}
            onChange={(e) => setInput(e.target.value)}
            placeholder="search facts — graph-aware…"
            aria-label="Search facts"
            className="flex-1 bg-transparent text-xs text-foreground outline-none placeholder:text-muted-foreground"
          />
        </label>
      </form>

      {searchError && (
        <Alert variant="destructive">
          <AlertDescription>Fact search degraded: {searchError}</AlertDescription>
        </Alert>
      )}

      {searching && (
        <div className="rounded-xl border bg-card p-4 shadow">
          <div className="text-sm font-semibold text-foreground">
            Facts matching “{query}”
          </div>
          <div className="mt-3">
            {facts === null ? (
              <p className="text-sm text-muted-foreground">Searching…</p>
            ) : (
              <FactList facts={facts} onOpen={openEntity} />
            )}
          </div>
        </div>
      )}

      {drill && (
        <div className="rounded-xl border bg-card p-4 shadow">
          <div className="flex items-start justify-between">
            <div>
              <div className="text-sm font-semibold text-foreground">
                {drill.entity?.name || drill.uuid}
              </div>
              {drill.entity?.summary && (
                <div className="mt-0.5 text-xs text-muted-foreground">{drill.entity.summary}</div>
              )}
            </div>
            <button
              type="button"
              aria-label="Close entity"
              onClick={() => setDrill(null)}
              className="inline-flex h-6 w-6 items-center justify-center rounded border border-input bg-background text-muted-foreground hover:bg-accent hover:text-accent-foreground"
            >
              <X className="h-3 w-3" />
            </button>
          </div>

          {drill.error && (
            <Alert variant="destructive" className="mt-3">
              <AlertDescription>Entity lookup degraded: {drill.error}</AlertDescription>
            </Alert>
          )}

          {drill.loading ? (
            <p className="mt-3 text-sm text-muted-foreground">Loading entity…</p>
          ) : (
            <div className="mt-3 space-y-4">
              <div>
                <div className="mb-2 text-xs font-semibold text-muted-foreground">Facts</div>
                <FactList facts={drill.facts} onOpen={openEntity} />
              </div>
              <div>
                <div className="mb-2 text-xs font-semibold text-muted-foreground">Related entities</div>
                {drill.related.length === 0 ? (
                  <p className="text-sm text-muted-foreground">No related entities.</p>
                ) : (
                  <div className="flex flex-wrap gap-2">
                    {drill.related.map((e) => (
                      <EntityChip key={e.uuid} uuid={e.uuid} label={e.name || e.uuid} onOpen={openEntity} />
                    ))}
                  </div>
                )}
              </div>
            </div>
          )}
        </div>
      )}

      <div className="rounded-xl border bg-card p-4 shadow">
        <div className="text-sm font-semibold text-foreground">Communities</div>
        {communities.length === 0 ? (
          <p className="mt-2 text-sm text-muted-foreground">No communities yet.</p>
        ) : (
          <ul className="mt-3 space-y-2">
            {communities.map((c) => (
              <li key={c.uuid} className="rounded-md border bg-background p-3">
                <div className="flex items-center gap-2">
                  <span className="font-mono text-xs text-foreground">{c.name || c.uuid}</span>
                  <Badge variant="outline">{c.members.length} members</Badge>
                </div>
                {c.summary && <div className="mt-1 text-xs text-muted-foreground">{c.summary}</div>}
              </li>
            ))}
          </ul>
        )}
        <p className="mt-3 text-[11px] text-muted-foreground">
          Graphiti knowledge graph — entities, facts, communities · oap kg search / entity / facts / related
        </p>
      </div>
    </div>
  );
}
