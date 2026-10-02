package admind

import (
	"context"
	"crypto/subtle"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb/relsource"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/platform/artifactstore"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/relsync"
	"github.com/authzed/openagentprimitives/pkg/web/admind/agentcred"
	"github.com/authzed/openagentprimitives/pkg/web/admind/audit"
	// Import registers the 8 per-CRD config.Projectors (agents, tools, skills,
	// sources, channels, identities, users, providers) consumed by the GET
	// /admin/v1/config/{resource} list handler, and the parallel set of
	// config.DetailProjectors behind GET /admin/v1/config/{resource}/{id...}.
	// No longer blank: New also calls SetSubjectIdentityReader on it directly.
	"github.com/authzed/openagentprimitives/pkg/web/admind/config/projectors"
	"github.com/authzed/openagentprimitives/pkg/web/admind/cost"
	"github.com/authzed/openagentprimitives/pkg/web/admind/health"
	"github.com/authzed/openagentprimitives/pkg/web/admind/overview"
)

// PlatformChecker is the SpiceDB seam — *spicedb.Client satisfies it.
//
// It is one interface rather than several because admind fails closed at
// CONSTRUCTION on a missing Checker (see New): every route's authorization
// arrives together or admind does not serve at all. Splitting the
// per-resource method below into an optional second field would let a
// half-wired operator mount the credential-replacement route with nothing to
// authorize it — a silent 405 at best, and one edit away from worse.
type PlatformChecker interface {
	CheckPlatformPermission(ctx context.Context, permission string, canonicalID identity.CanonicalUserID, fullyConsistent bool) (bool, error)
	// ListPlatformAdmins returns every subject on platform:platform#admin as
	// SpiceDB subject strings ("user:<canonical>" or "group:<id>#member").
	ListPlatformAdmins(ctx context.Context) ([]string, error)
	// CheckAgentIdentityUpdateCredential answers
	// agentidentity:<ns>/<name>#update_credential — may this human replace the
	// agent's OWN shared credential? Per-RESOURCE, unlike the platform-wide
	// permission above, and the authoritative gate on
	// POST /admin/v1/credentials/agent-update (credentials.go).
	//
	// It takes NO consistency parameter, deliberately: the #platform tuple that
	// makes the permission satisfiable is written by the very reconcile that
	// surfaces the dead credential, so a MinimizeLatency read can land on a
	// snapshot predating it and refuse a legitimate admin indistinguishably
	// from "not permitted". The implementation is unconditionally
	// FullyConsistent, and keeping the choice out of the signature means no
	// caller can reintroduce that bug.
	CheckAgentIdentityUpdateCredential(ctx context.Context, ns, name string, canonicalID identity.CanonicalUserID) (bool, error)
}

// SubjectIdentityReader reads the external identities a directory sync has
// linked to a canonical platform user.
//
// Deliberately NOT a fourth method on PlatformChecker: that interface answers
// authorization questions, and folding a data read into it would make a data
// outage look like an authorization-subsystem failure. OPTIONAL — admind
// degrades when it is nil (see the KG panel's same treatment), rather than
// failing construction the way the four required deps do.
//
// The result carries its own partial-failure channel (SubjectIdentities.
// Unavailable) rather than folding one absent source into the error: a single
// unreadable definition — github_org before the gh toolkit fragment is composed
// into the live schema, say — must shorten the panel, not blank it.
type SubjectIdentityReader interface {
	ListSubjectIdentities(ctx context.Context, canonicalID identity.CanonicalUserID) (spicedb.SubjectIdentities, error)
}

// SourceScopeReader reads what a directory sync's last pass actually wrote
// into SpiceDB for one RelationshipSource — *spicedb.Client.ListSourceScopes
// satisfies this.
//
// Deliberately a SEPARATE interface from SubjectIdentityReader, not a second
// method on it: the two answer different console questions from different
// sides of the same sync (subject-linked identities vs. the resource-side
// scopes a sync actually wrote — see spicedb.ListSourceScopes's own doc for
// why the subject side structurally cannot see a membership tuple), and a
// missing implementation of one must degrade only its own panel, the same
// reasoning that keeps this off PlatformChecker too. OPTIONAL — admind
// degrades when it is nil, the same treatment Identities and KG get.
type SourceScopeReader interface {
	ListSourceScopes(ctx context.Context, src relsource.Source, bridges []spicedb.ScopeLabelBridge, capPerDefinition int) (spicedb.SourceScopes, error)
}

// Config carries admind's constructor deps. Four are required — Mem, K8s,
// Checker, and Token — and New fails closed at construction rather than
// serving half-secured if any of those four is missing. Every other field is
// optional; each one's own doc says how its absence degrades (a panel reports
// "not configured"/"unavailable" rather than admind refusing to start).
type Config struct {
	Mem *memory.Local
	K8s client.Client
	// APIReader is an UNCACHED reader (mgr.GetAPIReader()) used for the
	// cluster-health snapshot's Deployment/StatefulSet/Pod lists — a one-shot
	// admin read that needs only get;list and must NOT start a cached informer
	// (those resources are not granted watch). Falls back to K8s if unset.
	APIReader client.Reader
	Checker   PlatformChecker
	Token     string // the spicebox-admind-token value; gates EVERY request
	Logger    logr.Logger
	// GraphitiURL is the Graphiti REST endpoint (env GRAPHITI_ENDPOINT),
	// passed through to the cluster-health snapshot for its liveness ping.
	// Empty => graphiti is reported "not configured", not down.
	GraphitiURL string
	// KG is the knowledge-graph provider (the operator's GraphitiKGProvider
	// when --graphiti-endpoint is set). OPTIONAL — nil means the Knowledge
	// panel degrades to an "available:false" not-configured state rather than
	// erroring. admind only ever calls KG's read methods, never Ingest. NOT in
	// the required-deps nil-check below.
	KG memory.KGProvider
	// MetadataBaseURL overrides the base URL of the cloud instance-metadata
	// server a cloud kind reads its cluster's name + location from (see
	// cloud.ClusterIdentityParams). Empty means the cloud's own default
	// endpoint; tests point it at an httptest server.
	MetadataBaseURL string
	// Clientset is the typed kubernetes.Interface the oap-install capacity
	// check reads nodes/pods through — cloud.Strategy.SchedulingCeiling needs
	// one, not the controller-runtime client K8s already is. OPTIONAL and NOT
	// in the required-deps check: nil skips the capacity preflight with a
	// notice rather than failing admind to start, because internal/cmd/operator
	// logs-and-continues when kubernetes.NewForConfig errors and admind must
	// still serve every other route.
	Clientset kubernetes.Interface
	// ArtifactStore is where a workshop's drafted .oap bundle bytes live,
	// keyed by the ref the controller recorded on Workshop.status.export. The
	// workshop-install route loads the bundle through it before running
	// install.Install. OPTIONAL and NOT in the required-deps nil-check (like
	// Clientset): the workshop-install handler fails closed with a clear 500
	// when it is nil, so admind still serves every other route on a cluster
	// with no artifact store configured.
	ArtifactStore artifactstore.Store
	// Identities reads the directory-synced identities linked to a canonical
	// user (Task 5's *spicedb.Client.ListSubjectIdentities) for the users
	// detail page's Directory identities panel. OPTIONAL and NOT in the
	// required-deps check below — nil leaves the panel off the page entirely,
	// the same degrade-not-fail treatment KG gets, rather than failing admind
	// construction over an additive read.
	Identities SubjectIdentityReader
	// Scopes reads what a directory sync's last pass actually wrote into
	// SpiceDB (*spicedb.Client.ListSourceScopes) for the directory detail
	// page's Scopes panel. OPTIONAL and NOT in the required-deps check below —
	// nil leaves the panel off the page entirely, the same degrade-not-fail
	// treatment Identities and KG get, rather than failing admind construction
	// over an additive read.
	Scopes SourceScopeReader
	// AccessTokenGrants reads an AccessToken's SpiceDB authorization grant
	// (role, scope classes, unfiltered) for the admin Tokens page's role
	// column — *spicedb.Client.ReadAccessTokenGrant (Task 3) satisfies this.
	// OPTIONAL and NOT in the required-deps check below — nil degrades every
	// row's role to "unknown" rather than failing admind construction, same
	// treatment as Identities/Scopes/KG (see tokens.go).
	AccessTokenGrants AccessTokenGrantReader
	// AccessTokenNamespace is where AccessToken CRs live — the SAME namespace
	// webd's --accesstoken-namespace mints into (default
	// "agentprimitives-system"), so the Tokens page lists exactly what the
	// /mcp bearer middleware checks against. Empty fails the tokens routes
	// closed (see tokens.go) rather than listing cluster-wide.
	AccessTokenNamespace string
}

// Admind is the admin API: mount Handler() under /admin/ on the
// operator's HTTP server, feed UpsertSession/DeleteSession from an
// AgentSession informer and HandleEnvelopeBytes from a NATS wildcard
// subscription (both via Aggregator()).
type Admind struct {
	cfg      Config
	agg      *Aggregator
	engine   *audit.Engine
	overview *overview.Engine
	health   *health.Checker
	prices   *cost.PriceMap
	// clusterMu guards the cluster-detection cache below. detectCluster is
	// otherwise re-run on every GET /cluster (on GKE that's two GCE metadata
	// fetches per header render); the cluster's identity does not change over a
	// pod's lifetime, so a short-TTL memo eliminates the repeat work.
	clusterMu sync.Mutex
	// clusterCached holds the last resolved ClusterInfo (nil until first detect).
	// BOTH a positive result and a negative/unknown one are cached, so a non-GKE
	// cluster does not re-probe on every request.
	clusterCached *ClusterInfo
	// clusterExpires is when clusterCached goes stale and detectCluster re-probes.
	clusterExpires time.Time
	// channelSetups holds the declared channels an install form has offered to
	// set up, between the request that rendered the form and the one that
	// submits it. See oapinstall_channel_pending.go for why any of this is
	// server-side rather than a field the browser sends back.
	channelSetups *channelSetupStore
	// wizardFor resolves a channel kind to its setup flow. registryWizard in
	// production; a field rather than a direct call so the route that RENDERS
	// a channel's form and the route that SUBMITS it resolve through the same
	// one — a test that replaced only half would prove nothing about the join,
	// which is the shape this branch keeps producing defects from.
	wizardFor channelWizardLookup
}

func New(cfg Config) (*Admind, error) {
	if cfg.Mem == nil || cfg.K8s == nil || cfg.Checker == nil || cfg.Token == "" {
		return nil, fmt.Errorf("admind: Mem, K8s, Checker, and Token are all required")
	}
	agg := NewAggregator(cfg.Logger.WithName("aggregator"))

	// Cluster-health checker reads via the UNCACHED APIReader (get;list only;
	// no watch granted on those workloads), falling back to the cached client
	// when no APIReader was injected (tests).
	reader := cfg.APIReader
	if reader == nil {
		reader = cfg.K8s
	}

	// Per-model price map for the Overview budget's estimated cost. Best-effort:
	// a bad override file logs and falls back to the built-in defaults rather
	// than failing admind construction.
	prices, err := cost.LoadPriceMap(os.Getenv("ADMIND_PRICE_MAP_PATH"))
	if err != nil {
		cfg.Logger.Info("admind: price map load failed; using built-in defaults",
			"path", os.Getenv("ADMIND_PRICE_MAP_PATH"), "err", err.Error())
	}
	if prices == nil {
		prices = cost.DefaultPriceMap()
	}

	// Directory identities panel: OPTIONAL, degrades to an "unavailable: <reason>"
	// text tab rather than failing admind construction (see SubjectIdentityReader's
	// doc). The wrapping happens here, not in package projectors — projectors has
	// no logger and must not grow one — so a failed read is both surfaced on the
	// page (via directoryIdentitiesSection's err branch) AND logged for whoever is
	// grepping, per the no-silent-errors rule.
	//
	// The else branch clears the injection point rather than leaving it alone:
	// subjectIdentityReader is a package-level var, not a field on this Admind, so
	// a process that constructs admind more than once (test binaries especially)
	// would otherwise leave an Identities-less instance still serving the
	// PREVIOUS instance's reader. The operator only ever constructs one, so this
	// is latent there, but the assignment must be unconditional to stay correct
	// for every caller.
	if cfg.Identities != nil {
		reader := cfg.Identities
		logger := cfg.Logger
		projectors.SetSubjectIdentityReader(func(ctx context.Context, subject string) (spicedb.SubjectIdentities, error) {
			// subject is already a "user:<canonical>" SpiceDB subject
			// reference, not a raw email — identity.Subject.CanonicalUserID() is
			// the typed strip of that prefix (mirrors every other UserIdentity
			// call site, e.g. pkg/controllers/useridentity/attested_edge.go).
			canonical, err := identity.Subject(subject).CanonicalUserID()
			if err != nil {
				logger.Info("admind: directory identity read failed: subject is not a user subject",
					"subject", subject, "err", err.Error())
				return spicedb.SubjectIdentities{}, err
			}
			ids, err := reader.ListSubjectIdentities(ctx, canonical)
			if err != nil {
				logger.Info("admind: directory identity read failed",
					"subject", subject, "err", err.Error())
			}
			// A partial read is not an error, but it is not silence either: the
			// page says so (directoryIdentitiesSection), and so does the log,
			// because an absent definition is an operator-fixable condition
			// nobody is watching the console for.
			for _, u := range ids.Unavailable {
				logger.Info("admind: directory identity probe unavailable",
					"subject", subject, "source", u.Source,
					"definition", u.Definition, "relation", u.Relation, "err", u.Err)
			}
			return ids, err
		})
	} else {
		projectors.SetSubjectIdentityReader(nil)
	}

	// Directory scopes panel: OPTIONAL, degrades to an "unavailable: <reason>"
	// text tab rather than failing admind construction — the same treatment as
	// the Directory identities panel above, and for the same reason: a data
	// read must not look like an authorization-subsystem failure.
	//
	// The spec.kind -> relsource.Source resolution happens HERE, not in
	// package projectors: projectors takes a plain spec.kind string precisely
	// so it never needs pkg/platform/relsync (see SetSourceScopeReader's own
	// doc), and this is where that import lives instead — the same split
	// admind.go already does for Identities' subject -> CanonicalUserID
	// conversion above.
	if cfg.Scopes != nil {
		reader := cfg.Scopes
		logger := cfg.Logger
		projectors.SetSourceScopeReader(func(ctx context.Context, kind string, capPerDefinition int) (spicedb.SourceScopes, error) {
			k, ok := relsync.Get(kind)
			if !ok {
				err := fmt.Errorf("no registered relsync kind %q", kind)
				logger.Info("admind: source scope read failed: kind not registered",
					"kind", kind, "err", err.Error())
				return spicedb.SourceScopes{}, err
			}
			// A kind that declares no ScopeLabeler passes nil bridges and its
			// scopes render by raw id, exactly as before the interface existed.
			// The type assertion is the whole dispatch — nothing here, and
			// nothing in package projectors, learns WHICH kind resolves names.
			var bridges []spicedb.ScopeLabelBridge
			if labeler, ok := k.(relsync.ScopeLabeler); ok {
				bridges = labeler.ScopeLabelBridges()
			}
			scopes, err := reader.ListSourceScopes(ctx, k.Source(), bridges, capPerDefinition)
			if err != nil {
				logger.Info("admind: source scope read failed", "kind", kind, "err", err.Error())
			}
			// A partial read is not an error, but it is not silence either: the
			// page says so (directoryScopesSection), and so does the log,
			// because an absent definition is an operator-fixable condition
			// nobody is watching the console for.
			for _, u := range scopes.Unavailable {
				logger.Info("admind: source scope probe unavailable",
					"kind", kind, "source", u.Source,
					"definition", u.Definition, "relation", u.Relation, "err", u.Err)
			}
			// Logged separately, and at the same level, for the same reason it
			// is a separate field: a failed label bridge leaves every row
			// present and some of them unnamed, which is a condition an
			// operator can fix (the gh toolkit fragment is not composed in yet,
			// most likely) and which nobody would otherwise ever see — an
			// unnamed row looks exactly like a row with no name to show.
			for _, u := range scopes.LabelsUnavailable {
				logger.Info("admind: source scope label bridge unavailable",
					"kind", kind, "source", u.Source,
					"definition", u.Definition, "relation", u.Relation, "err", u.Err)
			}
			return scopes, err
		})
	} else {
		projectors.SetSourceScopeReader(nil)
	}

	channelSetups := newChannelSetupStore()
	channelSetups.onCleanupError = func(err error) {
		cfg.Logger.Info("admind: expired channel prerequisite cleanup failed", "err", err.Error())
	}
	return &Admind{
		cfg: cfg,
		agg: agg,
		engine: &audit.Engine{
			Mem:          cfg.Mem,
			ResolveClass: classResolver(agg, cfg),
			Logger:       cfg.Logger.WithName("audit"),
		},
		overview: &overview.Engine{
			Mem:      cfg.Mem,
			Snapshot: liveSessionSnapshot(agg),
			Logger:   cfg.Logger.WithName("overview"),
		},
		health:        health.New(reader, cfg.GraphitiURL, cfg.Logger.WithName("health")),
		prices:        prices,
		channelSetups: channelSetups,
		wizardFor:     registryWizard,
	}, nil
}

func (a *Admind) Aggregator() *Aggregator { return a.agg }

// Memory exposes the facade for tests that seed entries.
func (a *Admind) Memory() *memory.Local { return a.cfg.Mem }

// classResolver builds the audit engine's ResolveClass hook: live aggregator
// state first (covers most queries with no API call), then the CRD; deleted
// sessions resolve to "" — audit entries outlive sessions by design.
//
// It closes over the aggregator and the config rather than hanging off *Admind
// so that everything Admind holds can be built in one expression: nothing here
// needs a half-constructed Admind.
func classResolver(agg *Aggregator, cfg Config) func(ctx context.Context, ns, name string) string {
	return func(ctx context.Context, ns, name string) string {
		if c := agg.Class(ns, name); c != "" {
			return c
		}
		var s spiceboxv1alpha1.AgentSession
		if err := cfg.K8s.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &s); err != nil {
			if !apierrors.IsNotFound(err) {
				cfg.Logger.Info("admind: resolve class failed", "session", ns+"/"+name, "err", err.Error())
			}
			return ""
		}
		return s.Spec.Class
	}
}

// liveSessionSnapshot builds the overview engine's Snapshot hook, projecting
// the aggregator's live session states into the narrow overview.LiveSession
// shape so the overview package stays free of the admind import (same seam
// discipline as audit).
func liveSessionSnapshot(agg *Aggregator) func() []overview.LiveSession {
	return func() []overview.LiveSession {
		states := agg.Snapshot()
		out := make([]overview.LiveSession, 0, len(states))
		for _, s := range states {
			var buckets []overview.ModelBucket
			if len(s.ByModel) > 0 {
				buckets = make([]overview.ModelBucket, len(s.ByModel))
				for i, b := range s.ByModel {
					buckets[i] = overview.ModelBucket{Model: b.Model, InputTokens: b.InputTokens, OutputTokens: b.OutputTokens}
				}
			}
			out = append(out, overview.LiveSession{
				Model:            s.Model,
				Class:            s.Class,
				Active:           s.Active,
				InputTokens:      s.InputTokens,
				OutputTokens:     s.OutputTokens,
				PendingApprovals: s.PendingToolGrants + s.PendingLeakageApprovals + s.PendingContentInspectionApprovals,
				ByModel:          buckets,
			})
		}
		return out
	}
}

// Audience says WHO reaches a route, which decides whether it needs a
// reverse-proxy entry in front of it.
//
// It is DATA on the route rather than a list kept somewhere else, because the
// question "is this browser-facing?" has exactly one correct answer and the
// person adding the route is the only one who knows it. A guard in
// pkg/web/adminui reads this to assert that every browser-facing route is
// actually proxied — and the defect it exists to prevent had already shipped:
// POST /agents/oap-install was registered here, called by the admin console's
// own TypeScript, and had no proxy entry, so the console's install button had
// never worked.
type Audience int

const (
	// AudienceBrowser: a person in the admin console reaches this, through
	// pkg/web/adminui's reverse proxy, which attaches the service token and the
	// authenticated subject server-side. THE ZERO VALUE, deliberately: a route
	// added without a thought about its audience is treated as browser-facing,
	// so the coverage guard complains rather than staying quiet.
	AudienceBrowser Audience = iota
	// AudienceCluster: another in-cluster component reaches this directly with
	// its own service token, and no browser ever does. It must NOT be proxied —
	// publishing it on the console's origin would put a server-to-server
	// endpoint behind a session cookie.
	AudienceCluster
)

func (a Audience) String() string {
	switch a {
	case AudienceBrowser:
		return "browser"
	case AudienceCluster:
		return "cluster"
	default:
		return "unknown"
	}
}

// Route is one route the admin API serves, as data.
//
// Routes() reports these and Handler() mounts these — the SAME rows, not a
// description of them. That is the point: a route added below is described,
// mounted, and covered by the proxy guard from one edit, and there is no second
// list to forget.
type Route struct {
	// Method is the HTTP method, exactly as the mux pattern spells it.
	Method string
	// Pattern is the full path pattern including the /admin/v1/ prefix, with
	// Go 1.22 mux wildcards ("{ns}", "{id...}") where it has them.
	Pattern string
	// Permission is the platform permission `require` gates this route on.
	// Empty only for a route that authorizes itself — see proven.
	Permission string
	// Audience decides whether a browser can reach this route at all.
	Audience Audience

	// plain is the handler for a route `require` gates. Exactly one of plain
	// and proven is non-nil on every row.
	//
	// Unexported, and a SELECTOR rather than a pre-wrapped handler, so
	// Permission is consumed in exactly one place (handlerFor) instead of being
	// repeated in each row's own require() call. A row whose declared
	// permission differed from the one it actually enforced would be the worst
	// possible kind of documentation.
	plain func(*Admind) http.HandlerFunc
	// proven is the handler for a route that performs its OWN per-resource
	// authorization. It is handed the proven subject and nothing else.
	proven func(*Admind) func(http.ResponseWriter, *http.Request, identity.CanonicalUserID)
}

// routeTable is every route the admin API serves.
var routeTable = []Route{
	{Method: http.MethodGet, Pattern: "/admin/v1/sessions", Permission: "view_sessions",
		plain: func(a *Admind) http.HandlerFunc { return a.handleSessionList }},
	{Method: http.MethodGet, Pattern: "/admin/v1/sessions/stream", Permission: "view_sessions",
		plain: func(a *Admind) http.HandlerFunc { return a.handleSessionStream }},
	{Method: http.MethodGet, Pattern: "/admin/v1/sessions/{ns}/{name}", Permission: "view_sessions",
		plain: func(a *Admind) http.HandlerFunc { return a.handleSessionDetail }},
	{Method: http.MethodGet, Pattern: "/admin/v1/sessions/{ns}/{name}/logs", Permission: "view_sessions",
		plain: func(a *Admind) http.HandlerFunc { return a.handleSessionLogs }},
	{Method: http.MethodDelete, Pattern: "/admin/v1/sessions/{ns}/{name}", Permission: "kill_session",
		plain: func(a *Admind) http.HandlerFunc { return a.handleSessionKill }},
	{Method: http.MethodGet, Pattern: "/admin/v1/toolcalls", Permission: "view_live",
		plain: func(a *Admind) http.HandlerFunc { return a.handleToolCalls }},
	{Method: http.MethodGet, Pattern: "/admin/v1/approvals", Permission: "view_live",
		plain: func(a *Admind) http.HandlerFunc { return a.handleApprovals }},
	{Method: http.MethodPost, Pattern: "/admin/v1/audit/query", Permission: "view_audit",
		plain: func(a *Admind) http.HandlerFunc { return a.handleAuditQuery }},
	{Method: http.MethodPost, Pattern: "/admin/v1/audit/facets", Permission: "view_audit",
		plain: func(a *Admind) http.HandlerFunc { return a.handleAuditFacets }},
	{Method: http.MethodGet, Pattern: "/admin/v1/audit/entities/{axis}", Permission: "view_audit",
		plain: func(a *Admind) http.HandlerFunc { return a.handleAuditEntities }},
	{Method: http.MethodGet, Pattern: "/admin/v1/artifacts", Permission: "view_audit",
		plain: func(a *Admind) http.HandlerFunc { return a.handleArtifacts }},
	{Method: http.MethodGet, Pattern: "/admin/v1/artifacts/{ns}/{name}", Permission: "view_audit",
		plain: func(a *Admind) http.HandlerFunc { return a.handleArtifactDetail }},
	{Method: http.MethodGet, Pattern: "/admin/v1/memory", Permission: "view_audit",
		plain: func(a *Admind) http.HandlerFunc { return a.handleMemory }},
	{Method: http.MethodGet, Pattern: "/admin/v1/kg/{action}", Permission: "view_audit",
		plain: func(a *Admind) http.HandlerFunc { return a.handleKG }},
	{Method: http.MethodGet, Pattern: "/admin/v1/overview", Permission: "view_overview",
		plain: func(a *Admind) http.HandlerFunc { return a.handleOverview }},
	{Method: http.MethodGet, Pattern: "/admin/v1/budget", Permission: "view_overview",
		plain: func(a *Admind) http.HandlerFunc { return a.handleBudget }},
	{Method: http.MethodGet, Pattern: "/admin/v1/health", Permission: "view_overview",
		plain: func(a *Admind) http.HandlerFunc { return a.handleHealth }},
	{Method: http.MethodGet, Pattern: "/admin/v1/cluster", Permission: "view_overview",
		plain: func(a *Admind) http.HandlerFunc { return a.handleCluster }},
	{Method: http.MethodGet, Pattern: "/admin/v1/access", Permission: "view_config",
		plain: func(a *Admind) http.HandlerFunc { return a.handleAccess }},
	// The access-token page: list is view_tokens, revoke is revoke_token — both
	// alias can_admin today (see schema.zed), but are their OWN permissions so
	// a future narrower grant (view without revoke) is a schema change, not a
	// handler change.
	{Method: http.MethodGet, Pattern: "/admin/v1/tokens", Permission: "view_tokens",
		plain: func(a *Admind) http.HandlerFunc { return a.handleTokensList }},
	{Method: http.MethodPost, Pattern: "/admin/v1/tokens/revoke", Permission: "revoke_token",
		plain: func(a *Admind) http.HandlerFunc { return a.handleTokensRevoke }},
	{Method: http.MethodPost, Pattern: "/admin/v1/agents/oap-install", Permission: "install_agent",
		plain: func(a *Admind) http.HandlerFunc { return a.handleOapInstall }},
	// The three channel routes sit behind the SAME permission as the install
	// they serve. Creating the Channel an installed agent needs — and minting
	// the Secret that holds its credentials — is part of installing that agent,
	// not a lesser act.
	{Method: http.MethodPost, Pattern: channelSetupPath, Permission: "install_agent",
		plain: func(a *Admind) http.HandlerFunc { return a.handleChannelSetup }},
	{Method: http.MethodPost, Pattern: channelHandoffPath, Permission: "install_agent",
		plain: func(a *Admind) http.HandlerFunc { return a.handleChannelHandoffBegin }},
	// The CALLBACK is gated too, which is the point rather than an oversight:
	// it is a public-facing URL that drives a credential exchange, and a public
	// one would let anyone who learned a `state` spend this operator's code. It
	// works as a browser redirect because the proxy turns the session cookie
	// the browser already carries into the forwarded subject and the service
	// token, server-side.
	{Method: http.MethodGet, Pattern: channelHandoffCallbackPath, Permission: "install_agent",
		plain: func(a *Admind) http.HandlerFunc { return a.handleChannelHandoffCallback }},
	// The one PER-RESOURCE route: agentidentity#update_credential names an
	// AgentIdentity not knowable until the request is resolved, so `require`
	// cannot gate it and the handler authorizes itself (credentials.go).
	//
	// AudienceCluster: identityd calls this server-to-server with its own
	// service token — the write lives here precisely so identityd need not hold
	// cluster-wide Secret access (pkg/platform/identityd/credupdate_agentowned.go).
	// No browser reaches it, and proxying it would publish a server-to-server
	// endpoint on the console's origin.
	{Method: http.MethodPost, Pattern: agentcred.Path, Audience: AudienceCluster,
		proven: func(a *Admind) func(http.ResponseWriter, *http.Request, identity.CanonicalUserID) {
			return a.handleAgentCredentialUpdate
		}},
	// The workshops page. Listing is view_sessions (a workshop is a
	// platform-admin object, and view_sessions/install_agent/kill_session all
	// alias can_admin — no new SpiceDB permission). Installing runs
	// install.Install under the OPERATOR SA (a.cfg.K8s) from bytes the
	// controller recorded on Workshop.status.export — never a caller-supplied
	// bundle — so it sits behind install_agent, the same gate as oap-install.
	{Method: http.MethodGet, Pattern: "/admin/v1/workshops", Permission: "view_sessions",
		plain: func(a *Admind) http.HandlerFunc { return a.handleWorkshopList }},
	{Method: http.MethodPost, Pattern: "/admin/v1/workshops/{ns}/{name}/install", Permission: "install_agent",
		plain: func(a *Admind) http.HandlerFunc { return a.handleWorkshopInstall }},
	{Method: http.MethodPost, Pattern: "/admin/v1/workshops/{ns}/{name}/decline", Permission: "install_agent",
		plain: func(a *Admind) http.HandlerFunc { return a.handleWorkshopDecline }},
	{Method: http.MethodDelete, Pattern: "/admin/v1/workshops/{ns}/{name}", Permission: "kill_session",
		plain: func(a *Admind) http.HandlerFunc { return a.handleWorkshopKill }},
	{Method: http.MethodGet, Pattern: "/admin/v1/config/settings", Permission: "view_config",
		plain: func(a *Admind) http.HandlerFunc { return a.handleConfigSettings }},
	{Method: http.MethodGet, Pattern: "/admin/v1/config/{resource}", Permission: "view_config",
		plain: func(a *Admind) http.HandlerFunc { return a.handleConfigResource }},
	// {id...} captures a namespaced id ("<ns>/<name>") or a cluster-scoped one
	// ("<name>"), so a two-segment id reaches the detail handler.
	{Method: http.MethodGet, Pattern: "/admin/v1/config/{resource}/{id...}", Permission: "view_config",
		plain: func(a *Admind) http.HandlerFunc { return a.handleConfigDetail }},
}

// Routes returns every route Handler() mounts, as data.
//
// Exported for pkg/web/adminui's coverage guard, which asserts that every
// browser-facing one has a reverse-proxy entry in front of it. It returns the
// rows Handler() iterates, not a transcription of them, so the guard cannot
// pass against a stale copy — which is exactly what a hand-written list of
// "routes that need proxying" would have reintroduced one layer up.
//
// A copy, so a caller cannot reorder or blank the table this process serves
// from.
func Routes() []Route {
	return append([]Route(nil), routeTable...)
}

// Handler returns the full /admin/v1/* mux wrapped per-route in the
// two-factor auth middleware (service token + SpiceDB platform check on
// the forwarded subject).
func (a *Admind) Handler() http.Handler {
	mux := http.NewServeMux()
	for _, rt := range routeTable {
		mux.Handle(rt.Method+" "+rt.Pattern, a.handlerFor(rt))
	}
	return mux
}

// handlerFor wraps one route's handler in the gate its row declares. It is the
// ONLY reader of Route.Permission, which is what keeps the permission a row
// declares and the permission it enforces the same string.
func (a *Admind) handlerFor(rt Route) http.Handler {
	if rt.proven != nil {
		return a.requireProvenSubject(rt.proven(a))
	}
	return a.require(rt.Permission, rt.plain(a))
}

// require enforces: (1) the bearer service token — keeps arbitrary
// in-cluster pods out; (2) a SpiceDB platform check on the webd-forwarded
// subject — defense in depth so a leaked token alone still can't act
// without an admin subject. SpiceDB ERRORS are 500, never treated as
// denied or allowed.
func (a *Admind) require(permission string, h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if tok == "" || subtle.ConstantTimeCompare([]byte(tok), []byte(a.cfg.Token)) != 1 {
			a.cfg.Logger.Info("admind: request with missing/wrong service token — check the spicebox-admind-token Secret mounts",
				"path", r.URL.Path)
			writeJSONError(w, http.StatusUnauthorized, "missing or invalid admind service token")
			return
		}
		// The subject must be exactly "user:<canonical>" with a non-empty
		// canonical part. Both disjuncts are needed and neither is redundant:
		// the prefix test also rejects the empty header, the emptiness test
		// rejects a bare "user:".
		subject := r.Header.Get("X-Admin-Subject")
		canonical := strings.TrimPrefix(subject, "user:")
		if !strings.HasPrefix(subject, "user:") || canonical == "" {
			writeJSONError(w, http.StatusUnauthorized, "missing X-Admin-Subject (want \"user:<canonical>\")")
			return
		}
		ok, err := a.cfg.Checker.CheckPlatformPermission(r.Context(), permission, identity.CanonicalFromTrusted(canonical,
			"X-Admin-Subject header, proxy-set and UNSIGNED"), false)
		if err != nil {
			a.cfg.Logger.Info("admind: platform permission check errored",
				"permission", permission, "subject", subject, "path", r.URL.Path, "err", err.Error())
			writeJSONError(w, http.StatusInternalServerError, "authorization check failed; see operator logs")
			return
		}
		if !ok {
			a.cfg.Logger.Info("admind: platform permission denied",
				"permission", permission, "subject", subject, "path", r.URL.Path)
			writeJSONError(w, http.StatusForbidden, "subject lacks platform permission "+permission)
			return
		}
		h(w, r)
	})
}
