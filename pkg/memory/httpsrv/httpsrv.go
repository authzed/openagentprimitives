// Package httpsrv exposes a memory.Memory over a bearer-authed HTTP API.
//
//	POST /memory/{kind}/{ns}/{name}    body Entry  → 201 Entry
//	GET  /memory/{kind}/{ns}/{name}    ?limit=&since=&tag= → 200 QueryResult
//	POST /memory/_query/{ns}/{name}    body Query  → 200 QueryResult
//	POST /memory/_signal/{ns}/{name}   body Signal → 204
//
// The scope is ALWAYS derived from the REQUEST LINE — the request body is
// never trusted for routing keys. It is Kind="session", ID="{ns}/{name}" from
// the URL, except on the keyed route, where ?pool=<type>:<id> may name a
// RESOURCE pool instead: still the request line, and honoured only once the
// session is PROVED to hold a write grant on that resource (destinationFor).
// A pool it holds no write grant on is refused, never downgraded to the
// session scope. "_scope" is reserved: scope teardown is in-process
// (Local.DeleteScope) and answers 405 here.
//
// Auth: Authorization: Bearer <token>. A per-session token may read and write
// its OWN {ns}/{name}, and may only READ the extra scopes it was registered
// with; anything else is 403. The channelsd and authzd system tokens may read
// and append on any session; the webd system token may only read. NO system
// token may delete an entry — deletion is a per-session bearer's operation.
//
// Scope is not the whole answer: WHICH Kinds a per-session credential may author
// is the Kind's own memory.WriteAuthority, enforced at the facade for Put and
// Delete. This handler's job is to tell the facade the call arrived on a session
// credential at all (memory.WithTokenSession).
//
// ?backend= steers the shadow backend's read source, honoured only on routes
// whose capability is a read and only for a known value; anything else is a 400
// rather than a silent downgrade.
package httpsrv

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/pools"
	"github.com/authzed/openagentprimitives/pkg/memory/provenance"
	"github.com/authzed/openagentprimitives/pkg/memory/sentinel"
	"github.com/authzed/openagentprimitives/pkg/memory/shadow"
	"github.com/authzed/openagentprimitives/pkg/memory/tokens"
	"github.com/authzed/openagentprimitives/pkg/platform/artifactstore"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/subjectresolve"
	"github.com/authzed/openagentprimitives/pkg/x/safehttp"
)

// Run starts h on addr, blocking until the listener errors or ctx is cancelled.
// The bind error (e.g. EADDRINUSE) is returned VERBATIM so the caller sees the
// real failure rather than a silent goroutine death; http.ErrServerClosed maps
// to nil for clean shutdown.
//
// Returning the error is the point: an unrecovered ListenAndServe goroutine
// once turned a port-bind failure into a logged line and no exit, leaving the
// operator running with no memory API and every client on connection-refused.
func Run(ctx context.Context, addr string, h http.Handler) error {
	srv := &http.Server{Addr: addr, Handler: h}
	safehttp.HardenServer(srv)
	done := make(chan error, 1)
	go func() {
		err := srv.ListenAndServe()
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		done <- err
	}()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		_ = srv.Shutdown(context.Background())
		return nil
	}
}

// The caller identities the system component tokens map to. "system:" is the
// repo-wide vocabulary for a non-user, cross-session caller (see
// memory.Provenance.Publisher and spicedbauthorizer.isSystemCaller). Gates
// answering "is this a shared component credential?" MUST test
// systemCallerPrefix rather than enumerate the tokens, so a system token added
// later is covered the day it is added, not the day someone remembers the list.
const (
	systemCallerPrefix = "system:"
	callerChannelsd    = systemCallerPrefix + "channelsd"
	callerAuthzd       = systemCallerPrefix + "authzd"
	callerWebd         = systemCallerPrefix + "webd"
)

// componentPrincipalKey is the context key for withComponentPrincipal.
type componentPrincipalKey struct{}

// withComponentPrincipal marks ctx as authenticated as the named platform
// component over its OWN system bearer token (e.g. "channelsd").
//
// Deliberately separate from memory.WithCaller/memory.CallerFrom: those carry
// "system:channelsd" for provenance (memory_entry#creator) and for the
// systemCallerPrefix check handleDeleteEntry already makes — "is this ANY
// system caller" — which is the wrong question for a route that must accept
// exactly ONE component and refuse every other, channelsd included among
// them if it changes identity later. A route that needs the narrower answer
// states it with its own marker instead of parsing a caller string that also
// has to keep serving the broader question correctly.
func withComponentPrincipal(ctx context.Context, name string) context.Context {
	return context.WithValue(ctx, componentPrincipalKey{}, name)
}

// componentPrincipalFrom reports the component name withComponentPrincipal
// stamped onto ctx, and whether one was stamped at all. Absent for session
// bearers, webd, authzd, and any caller this package has not explicitly
// marked — callers MUST treat "absent" as "not this component", never as
// "unknown, so allow."
func componentPrincipalFrom(ctx context.Context) (string, bool) {
	name, ok := ctx.Value(componentPrincipalKey{}).(string)
	return name, ok
}

// HandlerOption mutates a handler before it's wrapped in a mux. Callers that
// need optional features (e.g. the /artifact route, which requires a k8s
// client + artifactstore) pass these to NewHandler.
type HandlerOption func(*handler)

// WithColdRegistryGrace turns on the cold-start grace on the per-session bearer
// path: for grace after the handler is constructed, a bearer that no session is
// registered under is answered 503 + Retry-After (retryable) instead of 401
// (permanent). The operator's in-process token registry starts empty on every
// process boot and the AgentSession reconciler refills it per session
// (controllers/agentsession/memorytoken.go reregisterMemoryToken), so a live
// runner writing in that window would otherwise take a terminal 401 — and fail
// its session with MemoryUnavailable — the moment the operator restarts (an OOM,
// a rollout). The memory httpclient already retries 5xx across its ~17s budget,
// so a 503 rides the warm-up out with no client-side change.
//
// grace <= 0 (the default when this option is absent) disables the behavior and
// preserves the plain 401 — which is what every in-process test harness wants,
// since it Sets tokens synchronously and has no cold window. Only the operator's
// production wiring opts in.
func WithColdRegistryGrace(grace time.Duration) HandlerOption {
	return func(h *handler) { h.coldGrace = grace }
}

// WithArtifact mounts /artifact/{ns}/{sess}/{render}/output, which serves
// rendered ArtifactRender bytes, and /artifact-bundle/{ns}/{sess}/{render}/bundle,
// which resolves a primary HTML artifact's same-session `artifact:HANDLE`
// references into a self-contained ZIP. Both are gated by the channelsd/webd
// system tokens.
func WithArtifact(c client.Client, store artifactstore.Store) HandlerOption {
	return func(h *handler) {
		h.k8sClient = c
		h.artifactStore = store
	}
}

// WithPreferences enables GET /memory/_preferences/{ns}/{name}, which
// resolves the turn-author-verified user preferences snapshot for a session's
// AgentClass, and POST /memory/_preferences_commit/{ns}/{name}, which
// channelsd calls to commit an approved set_preference save. c reads the
// AgentSession, AgentClass and AgentSettings CRs both routes need — a
// client.Reader, not a client.Client, since neither route writes a CR (the
// commit route writes the preference value itself via h.mem, not via c).
func WithPreferences(c client.Reader) HandlerOption {
	return func(h *handler) { h.prefsReader = c }
}

// WithSubjectResolution enables ?user-ref= on GET /memory/_preferences: a
// subject-NAMED preferences read, resolved server-side via
// subjectresolve.Resolve rather than the turn author. rel answers the
// RelationReader role a resource-shaped reference (or a trigger-author
// reference recursing into one) needs to consult SpiceDB's sole_user
// relation.
//
// Nil (the default, option not passed) leaves h.subjectRelations a true nil
// interface — NOT a refusal of the whole ?user-ref= route: an email-form
// reference resolves without any SpiceDB lookup at all, so it still works.
// Only a resource-shaped reference degrades, and it degrades to an
// UNRESOLVED Resolution with a "not available" reason (see
// handlePreferencesGetForUserRef), never a panic or a 5xx — subjectresolve
// itself answers a nil RelationReader with subjectresolve.ErrNoRelationReader,
// an error by design (see that type's doc), and this package is what
// translates it into the softer, resolution-shaped answer a caller with
// nothing configured deserves.
//
// Guards against a TYPED-nil POINTER reader the same way WithPools does
// (see its doc and AGENTS.md "Nil interfaces: never assign a typed-nil
// pointer directly"). Note the guard's limit: it sees only a nil pointer
// boxed directly into the interface. A VALUE-typed adapter wrapping a nil
// client (the operator's subjectResolveSpiceDBAdapter is a struct value) is
// a non-nil, non-pointer interface this guard cannot and does not inspect —
// that case is safe only because the operator constructs the adapter after
// its SpiceDB client provably exists (startup exits otherwise), not because
// of anything here.
func WithSubjectResolution(rel subjectresolve.RelationReader) HandlerOption {
	return func(h *handler) {
		if rel != nil {
			v := reflect.ValueOf(rel)
			if v.Kind() == reflect.Ptr && v.IsNil() {
				rel = nil
			}
		}
		h.subjectRelations = rel
	}
}

// PreferenceAuditWriter appends one preference_access audit entry (see
// pkg/memory/kinds/preferenceaccess). Deliberately narrow — just the one
// method this route needs — because the value the operator hands
// WithPreferenceAudit MUST be its own signing facade (a
// *provenance.SigningMemory over "system:operator"), never h.mem directly:
// preference_access is ComponentWritten AND append-only, so an unsigned Put
// through h.mem would be refused the instant the verifier saw it carried no
// Provenance envelope. A narrow interface makes that substitution the
// caller's only real choice, rather than one more thing a wider Memory
// parameter would make easy to get wrong.
type PreferenceAuditWriter interface {
	Put(ctx context.Context, e memory.Entry) (memory.Entry, error)
}

// WithPreferenceAudit wires the append-only audit trail every ?user-ref=
// read writes, resolved or not (see handlePreferencesGetForUserRef).
//
// Nil (the default) does NOT leave ?user-ref= reachable-but-unaudited: the
// route refuses outright with a clear error. An unaudited subject-named
// disclosure is not a degraded version of this feature — it is a different,
// unaudited one, and this package does not silently serve it. The
// turn-derived route (?turn=/no query at all) is entirely unaffected: it
// never had an audit requirement, and still doesn't.
//
// Guards against a TYPED-nil concrete writer the same way WithPools does
// (see its doc and AGENTS.md "Nil interfaces: never assign a typed-nil
// pointer directly") — a caller passing a nil *provenance.SigningMemory
// (e.g. a test's zero-value memHandlerDeps.OpSigned) must land here as a
// true nil interface, so h.prefAudit == nil correctly refuses ?user-ref=
// instead of later dereferencing a nil receiver inside Put.
func WithPreferenceAudit(w PreferenceAuditWriter) HandlerOption {
	return func(h *handler) {
		if w != nil {
			v := reflect.ValueOf(w)
			if v.Kind() == reflect.Ptr && v.IsNil() {
				w = nil
			}
		}
		h.prefAudit = w
	}
}

// WithPreferenceWriteAudit wires the append-only preference_write audit
// trail every POST /memory/_preferences_firstparty_commit append — see
// handlePreferencesFirstPartyCommit's doc. Same PreferenceAuditWriter shape
// as WithPreferenceAudit (a narrow Put-only interface): the value handed
// here MUST be the operator's own signing facade (a
// *provenance.SigningMemory over "system:operator"), never h.mem directly —
// preference_write is ComponentWritten AND append-only, so an unsigned Put
// would be refused the instant the verifier saw it carried no Provenance
// envelope.
//
// Nil (the default) does NOT leave the route reachable-but-unaudited: it
// refuses outright with 503, mirroring WithPreferenceAudit's ?user-ref=
// refusal — an unaudited first-party write is a different, unaudited
// feature, not a degraded version of this one.
//
// Guards against a TYPED-nil concrete writer the same way WithPreferenceAudit
// does (see its doc and AGENTS.md "Nil interfaces: never assign a typed-nil
// pointer directly") — a caller passing a nil *provenance.SigningMemory must
// land here as a true nil interface, so h.prefWriteAudit == nil correctly
// refuses the route instead of later dereferencing a nil receiver inside Put.
func WithPreferenceWriteAudit(w PreferenceAuditWriter) HandlerOption {
	return func(h *handler) {
		if w != nil {
			v := reflect.ValueOf(w)
			if v.Kind() == reflect.Ptr && v.IsNil() {
				w = nil
			}
		}
		h.prefWriteAudit = w
	}
}

// EntryDeleter is an optional interface for entry deletion. When provided
// via WithDeleter, the _entry DELETE route is enabled. *memory.Local
// satisfies this.
type EntryDeleter interface {
	Delete(ctx context.Context, scope memory.Scope, kind, id string) error
}

// EntryReindexer is an optional interface for reindexing. When the Memory
// passed to NewHandler satisfies this (e.g. *memory.Local), the
// POST /memory/_reindex/{ns}/{name} route is enabled.
type EntryReindexer interface {
	Reindex(ctx context.Context, scope memory.Scope) (int, error)
}

// WithDeleter enables the DELETE /memory/_entry/{ns}/{name} route.
func WithDeleter(d EntryDeleter) HandlerOption {
	return func(h *handler) { h.deleter = d }
}

// WithReindexer enables the POST /memory/_reindex/{ns}/{name} route.
func WithReindexer(r EntryReindexer) HandlerOption {
	return func(h *handler) { h.reindexer = r }
}

// KGQuerier is the interface for knowledge-graph query operations. When
// provided via WithKG, the GET /memory/_kg/{ns}/{name}?action= routes are
// enabled.
type KGQuerier interface {
	SearchFacts(ctx context.Context, query string, limit int) ([]memory.KGFact, error)
	GetEntity(ctx context.Context, uuid string) (*memory.KGEntity, error)
	EntityFacts(ctx context.Context, entityUUID string) ([]memory.KGFact, error)
	RelatedEntities(ctx context.Context, entityUUID string, limit int) ([]memory.KGEntity, error)
	Communities(ctx context.Context, groupID string) ([]memory.KGCommunity, error)
}

// WithKG enables the GET /memory/_kg/{ns}/{name} route.
func WithKG(kg KGQuerier) HandlerOption {
	return func(h *handler) { h.kg = kg }
}

// PublisherKeyRegistrar persists a component publisher key. Injected by
// the operator (ConfigMap-backed); nil disables the endpoint (405).
type PublisherKeyRegistrar interface {
	RegisterPublisherKey(ctx context.Context, publisher, keyID string, pub ed25519.PublicKey) error
}

// WithPublisherKeyRegistrar enables the POST /memory/_publisher_key
// route, by which a system component (channelsd/authzd) registers its
// Ed25519 signing key for verify-on-write.
func WithPublisherKeyRegistrar(r PublisherKeyRegistrar) HandlerOption {
	return func(h *handler) { h.pubKeyReg = r }
}

// WithPools sets the resource-pool discovery reader ServeHTTP consults, via
// mintPoolApprovals, to mint a session's per-pool approvals alongside its own
// session-scope one. Omitting this option — or passing a nil reader — leaves
// h.pools a true nil interface, and mintPoolApprovals's existing nil check
// means "no resource pools configured for this deployment": every request
// still gets exactly the session-scope approval it got before this option
// existed.
//
// Guards against a TYPED-nil concrete reader (e.g. a nil *someClient passed
// as pools.RelationshipReader), which — assigned directly — would produce a
// non-nil INTERFACE wrapping a nil value: h.pools == nil would then read
// false, mintPoolApprovals would call pools.ForSession on it, and the nil
// pointer would be dereferenced the moment ReadRelationships tried to use it.
// See AGENTS.md "Nil interfaces: never assign a typed-nil pointer directly."
// reflect is what lets this check apply to any concrete pointer type a
// caller passes, not just ones this package already knows about.
func WithPools(r pools.RelationshipReader) HandlerOption {
	return func(h *handler) {
		if r != nil {
			v := reflect.ValueOf(r)
			if v.Kind() == reflect.Ptr && v.IsNil() {
				r = nil
			}
		}
		h.pools = r
	}
}

func NewHandler(mem memory.Memory, reg *tokens.Registry, opts ...HandlerOption) http.Handler {
	h := &handler{mem: mem, reg: reg}
	for _, opt := range opts {
		opt(h)
	}
	// Stamp construction time AFTER options apply: coldGrace comes from an
	// option, but startedAt is always the real wall-clock birth of this handler,
	// so time.Since(startedAt) is the true process uptime the cold-start grace
	// compares against.
	h.startedAt = time.Now()
	mux := http.NewServeMux()
	mux.Handle("/memory/", h)
	if h.k8sClient != nil && h.artifactStore != nil {
		mux.HandleFunc("/artifact/", h.serveArtifact)
		mux.HandleFunc("/artifact-bundle/", h.serveBundle)
	}
	if h.artifactStore != nil {
		// /inbound-asset needs only the artifactstore (no ArtifactRender CR
		// lookup, so no k8sClient dependency) — the write counterpart to the
		// read-only /artifact/... routes above.
		mux.HandleFunc("/inbound-asset/", h.serveInboundAsset)
	}
	return mux
}

type handler struct {
	mem memory.Memory
	reg *tokens.Registry

	// startedAt / coldGrace govern the cold-start grace on the per-session
	// bearer path (the else branch in ServeHTTP's auth block). For coldGrace
	// after this handler is constructed, a bearer that no session is registered
	// under is answered 503 + Retry-After (retryable) instead of 401
	// (permanent). The operator's in-process token registry is empty on every
	// process boot and the AgentSession reconciler refills it per session
	// (controllers/agentsession/memorytoken.go), so a live runner writing in
	// that window would otherwise take a terminal 401 — and a MemoryUnavailable
	// session failure — the instant the operator restarts. coldGrace == 0 (the
	// default when WithColdRegistryGrace is absent) disables it, which is why
	// every in-process test harness — tokens Set synchronously, no window —
	// keeps the plain 401. See WithColdRegistryGrace.
	startedAt time.Time
	coldGrace time.Duration

	// Optional, set by WithArtifact: enables /artifact/{ns}/{sess}/{render}/output.
	k8sClient     client.Client
	artifactStore artifactstore.Store

	deleter     EntryDeleter
	reindexer   EntryReindexer
	kg          KGQuerier
	pubKeyReg   PublisherKeyRegistrar
	ptTagMinter PtTagMinter
	ptTagAccess PtTagAccessChecker

	// Optional, set by WithPreferences: enables GET /memory/_preferences and
	// POST /memory/_preferences_commit.
	prefsReader client.Reader

	// Optional, set by WithSubjectResolution: the RelationReader a
	// resource-shaped ?user-ref= reference needs. Nil means a resource-shaped
	// (or trigger-author) reference cannot resolve on this deployment — see
	// WithSubjectResolution's doc for why that is a softer answer than a
	// refusal, and why an email-form reference is unaffected.
	subjectRelations subjectresolve.RelationReader

	// Optional, set by WithPreferenceAudit: where ?user-ref= reads append
	// their preference_access audit entry. Nil refuses ?user-ref= outright —
	// see WithPreferenceAudit's doc.
	prefAudit PreferenceAuditWriter

	// Optional, set by WithPreferenceWriteAudit: where a first-party commit
	// (POST /memory/_preferences_firstparty_commit) appends its
	// preference_write audit entry. Nil refuses that route outright — see
	// WithPreferenceWriteAudit's doc.
	prefWriteAudit PreferenceAuditWriter

	// Optional, set by WithPools: the session's resource-pool discovery
	// reader, consulted by mintPoolApprovals to find which resource pools a
	// session's bearer should also carry approvals for. Nil means this
	// deployment has no resource pools configured — mintPoolApprovals mints
	// nothing and does not error, exactly like every other optional
	// collaborator on this struct.
	pools pools.RelationshipReader

	// Optional, set by WithAttachmentExtractor: enables extraction on
	// /inbound-asset. Nil means the route still stores bytes but reports
	// extraction as unavailable (see inbound_asset.go).
	attachmentExtractor AttachmentExtractor
	attachmentExploder  AttachmentExploder
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	authz := r.Header.Get("Authorization")
	if !strings.HasPrefix(authz, "Bearer ") {
		w.Header().Set("WWW-Authenticate", `Bearer realm="memory"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	token := strings.TrimPrefix(authz, "Bearer ")

	segs := pathSegments(r.URL.Path)

	// _publisher_key is a singleton (NOT session-scoped): it registers a
	// component publisher's signing key. Handle it before the 3-segment
	// scope routing, which it does not satisfy.
	if len(segs) >= 1 && segs[0] == "_publisher_key" {
		h.handlePublisherKey(w, r, token)
		return
	}

	// Split /memory/<...> into segments. Every other route resolves to a
	// scope of {Kind:"session", ID:"<ns>/<name>"}; the {ns}/{name} pair
	// is the last two segments for both the keyed and the _query/_signal
	// routes, so auth can be checked uniformly before dispatch.
	if len(segs) != 3 {
		http.Error(w, "not a memory path", http.StatusNotFound)
		return
	}
	ns, name := segs[1], segs[2]
	if ns == "" || name == "" {
		http.Error(w, "empty ns or name", http.StatusNotFound)
		return
	}
	urlSess := memory.NamespacedName{Namespace: ns, Name: name}

	// Auth: the channelsd and authzd system tokens operate on any session; the
	// webd token operates on any session but READ-ONLY; a per-session token
	// operates only on its own <ns>/<name>.
	ctx := r.Context()
	if h.reg.IsChannelsdToken(token) {
		ctx = memory.WithCaller(ctx, callerChannelsd)
		ctx = memory.WithSystemApproval(ctx, callerChannelsd)
		// Marks the request as channelsd specifically, not merely "a system
		// caller" — see withComponentPrincipal's doc. The preferences commit
		// route (_preferences_commit) is the first consumer: it requires
		// channelsd by name because channelsd is where the human-confirm
		// verification for a set-preference reply actually happens, over the
		// conversational surface — the operator has no way to re-verify that
		// itself and trusts channelsd's word for it, nobody else's.
		ctx = withComponentPrincipal(ctx, "channelsd")
	} else if h.reg.IsAuthzdToken(token) {
		ctx = memory.WithCaller(ctx, callerAuthzd)
		ctx = memory.WithSystemApproval(ctx, callerAuthzd)
	} else if h.reg.IsWebdToken(token) {
		// webd is browser-facing; its token may read any session's artifact
		// metadata but must never write memory or register keys. Refuse every
		// mutating route/method before dispatch.
		if memoryRouteAccess(r.Method, segs[0]) != readAccess {
			http.Error(w, "forbidden: webd token is read-only", http.StatusForbidden)
			return
		}
		ctx = memory.WithCaller(ctx, callerWebd)
		// Behind the PER-KIND read door, for the same reason the approval below
		// is session-scoped rather than wildcard: webd is browser-facing.
		//
		// The door used to key on the token-session mark, which is PROVENANCE
		// and which webd — acting on any session, belonging to none — never
		// carries. So it skipped webd entirely and the token could read
		// pt_tag_content, and every other platform-only Kind, for any session in
		// the cluster. Said explicitly here rather than inferred, so a component
		// token added later has to make its own decision instead of inheriting
		// an exemption by omission.
		ctx = memory.WithKindReadDoor(ctx, callerWebd)
		// A SESSION-SCOPED approval, NOT the wildcard WithSystemApproval the
		// other component tokens carry. webd is the only browser-facing holder
		// of a system token, and a wildcard approval satisfies any non-internal
		// permission for any resource — bounded only by every handler
		// independently remembering to force the scope from the URL. Binding
		// the capability to ns/name makes that structural: a route that forgets
		// is refused at the door instead of permitted by default.
		//
		// Every route this branch can reach (_query, _search, _kg, GET on the
		// bare {kind} route) returns ok from routePermission. A future !ok
		// mints nothing and EnsureApproval fails closed — the right direction.
		if perm, ok := routePermission(segs[0], r.Method); ok {
			ctx = memory.WithApproval(ctx, memory.ForBearerToken(perm, ns+"/"+name, tokenIDFor(token)))
			// The pools this SESSION (named in the URL, not the webd caller)
			// holds slot grants on carry their own approvals too — see
			// mintPoolApprovals. Gated on the SAME perm as the line above, for
			// the same reason: webd's own route is read-only, and minting a pool
			// direction the route itself was never granted would hand a
			// read-only, browser-facing credential write capability the moment
			// some future route dispatches to a pool scope.
			ctx = h.mintPoolApprovals(ctx, ns, name, token, perm)
		}
	} else {
		info, ok := h.reg.LookupInfo(token)
		if !ok {
			// Cold-start grace. Within coldGrace of this process starting, a bearer
			// no session is registered under is far likelier a session the operator
			// has not RE-registered yet than a forged token: the registry is process
			// memory, empty on every boot, refilled per session by the reconciler
			// (controllers/agentsession/memorytoken.go reregisterMemoryToken).
			// Answer 503 + Retry-After so the caller — the memory httpclient, which
			// retries 5xx across a ~17s budget — rides the warm-up out, instead of a
			// terminal 401 that fails a live session with MemoryUnavailable the
			// instant the operator OOMs or rolls. Past the grace, or when it is
			// disabled (coldGrace == 0, every in-process test harness), an unknown
			// bearer is a genuine auth failure and falls through to the 401 below.
			// See WithColdRegistryGrace.
			if h.coldGrace > 0 && time.Since(h.startedAt) < h.coldGrace {
				log.FromContext(ctx).Info("memory API: bearer not yet registered within the cold-start grace; answering 503 (retryable) — the operator likely restarted and has not re-reconciled this session yet",
					"session", ns+"/"+name, "path", r.URL.Path, "uptime", time.Since(h.startedAt).String(), "grace", h.coldGrace.String())
				w.Header().Set("Retry-After", "1")
				http.Error(w, "unavailable: token registry still warming after operator restart; retry", http.StatusServiceUnavailable)
				return
			}
			// Say WHICH of the two refusals this is. A registered token aimed at
			// the wrong session is answered 403 a few lines below, so the 401/403
			// split already tells a caller whether its bearer is known — naming it
			// in the body adds no oracle, and the token value never appears in
			// either half.
			//
			// It is worth naming because the two ways to arrive here look identical
			// from the client and are fixed differently: a wrong value, or a value
			// that is still the one in the session's Secret while this process has
			// never heard of it. The second is a registry that lost the session
			// (the operator restarted), and a bare "unauthorized" sent readers
			// hunting a credential bug for hours. The log line carries the session
			// the caller was asking about, which the body deliberately does not
			// repeat back to an unauthenticated caller.
			log.FromContext(ctx).Info("memory API: refusing a bearer no session is registered under; if it matches the session's memory-token Secret, this operator has not registered that session",
				"session", ns+"/"+name, "path", r.URL.Path)
			w.Header().Set("WWW-Authenticate", `Bearer realm="memory"`)
			http.Error(w, "unauthorized: this bearer token is not registered for any session", http.StatusUnauthorized)
			return
		}
		// A per-session token reaches its OWN session for everything, and the
		// extra scopes it was registered with for READS ONLY — the extras let a
		// runner fetch artifacts from the per-bundle SpiceboxSessions its
		// ToolCalls ran against, and asking Authorizes for writes too would make
		// every one of those scopes writable and deletable by the runner, which
		// no code path needs. Both halves come from the registry rather than
		// being re-derived here, so the read/write distinction lives with the
		// registration that creates it.
		authorized := h.reg.Authorizes(token, urlSess)
		if memoryRouteAccess(r.Method, segs[0]) != readAccess {
			authorized = h.reg.AuthorizesMutation(token, urlSess)
		}
		if !authorized {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		// The token's OWN identity, carried separately from the caller: it is
		// what binds an append-only entry's provenance publisher to the writer
		// (provenance.WriteVerifier). A session bearer may author entries as
		// session:<its ns>/<its name> and nothing else, in every scope the token
		// reaches — and a runner's token reaches every per-bundle scope it was
		// registered with, not just its own.
		//
		// Attached UNCONDITIONALLY, before any other per-token decision, because
		// the failure modes are asymmetric: a forgotten attach fails OPEN (the
		// verifier stops binding), while an over-broad one fails LOUD (a
		// legitimate write is refused and its hand-off gets fixed — see
		// Local.SendSignal). Prefer loud.
		ctx = memory.WithTokenSession(ctx, info.Session)
		// CallerID is the USER the token acts for. It feeds the Authorizer's
		// memory_entry#creator tuple, so it must remain absent for the non-user
		// callers that register with "".
		if info.CallerID != "" {
			ctx = memory.WithCaller(ctx, info.CallerID)
		}
		// The token authorizes urlSess; mint a scoped bearer approval so the
		// facade door permits exactly this session's data and nothing else.
		if perm, ok := routePermission(segs[0], r.Method); ok {
			ctx = memory.WithApproval(ctx, memory.ForBearerToken(perm, ns+"/"+name, tokenIDFor(token)))
			// The pools this session holds slot grants on carry their own
			// approvals too — see mintPoolApprovals. Gated on the SAME perm as
			// the line above: a read route mints only the read pools, a write
			// route only the write pools, exactly like the session-scope mint
			// itself.
			ctx = h.mintPoolApprovals(ctx, ns, name, token, perm)
		}
	}
	// Parse the query ONCE, explicitly, and refuse one that does not parse.
	//
	// (*url.URL).Query() DISCARDS url.ParseQuery's error and omits the pairs
	// that failed, so a bad percent-escape or a ';' anywhere in a pair makes
	// Get() on that key return "" — indistinguishable from the parameter not
	// being sent. For ?pool= that is the silent downgrade this handler's whole
	// destination gate exists to prevent: the write would land in the SESSION
	// scope and answer 201, reporting success for a write that went somewhere
	// the caller never addressed, and on an append-only Kind (where the
	// facade's per-scope check is provenance verification, which takes no
	// scope) nothing downstream would notice.
	//
	// A malformed query string is therefore a refusal for the WHOLE request,
	// not a per-parameter one: "the server could not read what you asked for"
	// is never safely equivalent to "you asked for nothing." Every ?-reader
	// below takes this parsed value, so none of them can reintroduce the hole.
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		http.Error(w, "malformed query string: "+err.Error(), http.StatusBadRequest)
		return
	}
	if backend := query.Get("backend"); backend != "" {
		if !backendOverrideAllowed(segs[0], r.Method) {
			http.Error(w, "?backend= may only steer a read route's read source", http.StatusBadRequest)
			return
		}
		if backend != shadow.ReadFromPrimary && backend != shadow.ReadFromSecondary {
			http.Error(w, "?backend= must be "+shadow.ReadFromPrimary+" or "+shadow.ReadFromSecondary, http.StatusBadRequest)
			return
		}
		ctx = shadow.WithReadFrom(ctx, backend)
	}
	// Keyed on PRESENCE, not on a non-empty value: `?pool=` with no value named
	// a destination the server cannot resolve, and answering it from the
	// session scope is the same silent downgrade as any other unproved pool.
	if _, named := query["pool"]; named && !poolDestinationAllowed(segs[0]) {
		http.Error(w, "?pool= may only name a destination on the /memory/{kind}/{ns}/{name} route", http.StatusBadRequest)
		return
	}
	r = r.WithContext(ctx)

	scope := memory.Scope{Kind: "session", ID: ns + "/" + name}

	switch segs[0] {
	case "_kg":
		h.handleKG(w, r, scope)
	case "_search":
		h.handleSearch(w, r, scope)
	case "_query":
		h.handleQuery(w, r, scope)
	case "_preferences":
		h.handlePreferencesGet(w, r, scope)
	case "_preferences_firstparty":
		h.handlePreferencesFirstPartyGet(w, r, scope)
	case "_preferences_commit":
		h.handlePreferencesCommit(w, r, scope)
	case "_preferences_firstparty_commit":
		h.handlePreferencesFirstPartyCommit(w, r, scope)
	case "_signal":
		h.handleSignal(w, r, scope)
	case "_entry":
		h.handleDeleteEntry(w, r, scope)
	case "_pttag_mint":
		h.handlePtTagMint(w, r, scope)
	case "_pttag_verify":
		h.handlePtTagVerify(w, r, scope)
	case "_pttag_resolve":
		h.handlePtTagResolve(w, r, scope)
	case "_reindex":
		h.handleReindex(w, r, scope)
	case "_scope":
		// Scope teardown is in-process (Local.DeleteScope), never HTTP.
		http.Error(w, "_scope is not an HTTP route", http.StatusMethodNotAllowed)
	default:
		// The keyed route is the only one whose destination may be a RESOURCE
		// pool instead of the session's own scope, and it is named on the
		// request line (?pool=) and PROVED here before handleKind ever sees it.
		// Resolved before dispatch rather than inside putEntry so the scope
		// handleKind is handed is already the decided one — putEntry keeps
		// forcing e.Scope from a server-decided value, which is the property
		// that mattered.
		dest, code, derr := h.destinationFor(r, query, scope, ns, name)
		if derr != nil {
			http.Error(w, derr.Error(), code)
			return
		}
		h.handleKind(w, r, dest, segs[0])
	}
}

// memoryRouteAccess classifies a /memory/* (method, route) pair as a read or a
// write, in the same vocabulary checkSystemBearer takes (artifact.go), so "the
// webd token may only do readAccess" is stated once and both entry points obey
// it.
//
// The two DERIVE access differently, deliberately: here from this vetted table,
// since /memory/* routes dispatch by name from one switch; on the artifact
// routes each handler declares its own, so checkSystemBearer also requires a
// safe method as a backstop. That backstop would be wrong here —
// _query/_search/_kg are POST-bodied yet read-only by nature.
func memoryRouteAccess(method, route string) access {
	switch route {
	case "_query", "_search", "_kg", "_pttag_verify", "_pttag_resolve", "_preferences_firstparty":
		// _pttag_verify / _pttag_resolve are POST-bodied but read-only by nature
		// — they read the content store (to bind ids / return entitled content),
		// mutating nothing — exactly like _query. _preferences_firstparty is a
		// plain GET, read-only by nature too.
		return readAccess
	case "_signal", "_entry", "_reindex", "_scope", "_pttag_mint", "_preferences_commit", "_preferences_firstparty_commit":
		return writeAccess
	default:
		if method == http.MethodGet {
			return readAccess
		}
		return writeAccess
	}
}

// routePermission maps a (route, method) to the memory capability permission the
// request needs. The bool is false for non-data routes (e.g. _publisher_key) and
// for _scope (not HTTP-reachable). handleKind is the bare "{kind}" route: GET
// reads, POST writes.
func routePermission(route, method string) (memory.Permission, bool) {
	switch route {
	case "_query", "_search", "_preferences", "_preferences_firstparty":
		return memory.ReadMemory, true
	case "_kg":
		return memory.ReadKG, true
	case "_entry":
		return memory.DeleteMemory, true
	case "_signal", "_preferences_commit", "_preferences_firstparty_commit":
		return memory.WriteMemory, true
	case "_pttag_mint":
		// A mint WRITES a pt_tag into the session's scope, so it needs the
		// same scope-level permission as any other memory write. The per-kind
		// write door still refuses the session credential itself — that is the
		// point of the route — but the scope door must be satisfied first, or
		// a caller could ask the operator to mint into a scope it may not
		// touch at all.
		return memory.WriteMemory, true
	case "_reindex":
		return memory.ReadMemory, true
	case "_pttag_verify", "_pttag_resolve":
		// Read the content store — a scope-level read. The per-kind read door
		// still refuses the session credential the raw pt_tag_content kind; the
		// handler reads it component-side after this scope door passes (and, for
		// resolve, gates each returned tag on the caller's pt_tag#access).
		return memory.ReadMemory, true
	case "_publisher_key", "_scope":
		return "", false
	default: // handleKind: "{kind}"
		if method == http.MethodPost {
			return memory.WriteMemory, true
		}
		return memory.ReadMemory, true
	}
}

// backendOverrideAllowed reports whether ?backend= may steer the shadow
// backend's read source for this (route, method).
//
// Honoured ONLY where the route's own capability is a read. A mutating route
// must never let its caller choose which half of a shadow backend its INTERNAL
// reads consult: Local.Put's append-only pre-check is such a read, and aiming it
// at the ephemeral in-memory primary (empty after an operator restart) makes
// ErrAppendOnlyConflict unreachable, so the durable secondary's upsert
// overwrites the very row that pre-check protects.
//
// Derived from routePermission rather than a fourth route switch that could
// drift out of agreement with the other three. That also keeps it fail-closed
// for routes added later: a non-read permission, or a route with no data
// permission at all, refuses the override rather than inheriting it.
//
// Deliberately NOT memoryRouteAccess, which answers "may the read-only webd
// token do this?" and is writeAccess for _reindex — a route that only reads the
// backend and rewrites search indexes, and whose ?backend= steering
// `oap memory reindex --backend=` depends on.
func backendOverrideAllowed(route, method string) bool {
	perm, ok := routePermission(route, method)
	if !ok {
		return false
	}
	return perm == memory.ReadMemory || perm == memory.ReadKG
}

// poolDestinationAllowed reports whether ?pool= may name this route's
// destination. Only the keyed "{kind}" route dispatches to a scope that
// destinationFor decides; every reserved "_"-prefixed route forces its own
// scope from the URL and would simply IGNORE the parameter.
//
// Refused rather than ignored, for the same reason destinationFor refuses an
// unproved pool rather than downgrading it: a caller that addressed a pool and
// silently got the session's own data back has no way to tell, and finds out
// only when it has already acted on an answer from somewhere else.
//
// Derived from the route's own reserved prefix rather than a list, so a
// reserved route added later inherits the refusal instead of inheriting an
// exemption by omission. A future route that genuinely wants a pool
// destination has to say so here, which is the loud direction.
func poolDestinationAllowed(route string) bool {
	return !strings.HasPrefix(route, "_")
}

// tokenIDFor derives a short, loggable evidence id from a bearer token. It is
// NEVER the full token — only a short suffix, so audit logs can distinguish
// tokens without exposing a credential.
func tokenIDFor(token string) string {
	if len(token) >= 6 {
		return "tok-" + token[len(token)-6:]
	}
	return "tok"
}

// poolReadScopesKey is the context key mintPoolApprovals uses to hand its ONE
// pools.ForSession resolution to handleSearch's scope expansion, so a request
// that both mints pool approvals and searches never pays for a second SpiceDB
// streaming read, and never risks the mint and the expansion disagreeing
// about which pools the session holds.
type poolReadScopesKey struct{}

// withPoolReadScopes stashes the session's resolved read-pool scopes for a
// later handler on the SAME request to pick up via poolReadScopesFrom. Called
// only from mintPoolApprovals, and only once per request.
func withPoolReadScopes(ctx context.Context, scopes []memory.Scope) context.Context {
	return context.WithValue(ctx, poolReadScopesKey{}, scopes)
}

// poolReadScopesFrom returns the read-pool scopes mintPoolApprovals resolved
// for this request, every one of which it also minted a ReadMemory approval
// for on the same request. That pairing is the contract: a caller expanding a
// scope list with these names them straight at the per-scope door, so a scope
// stashed without its approval would be a scope the door then refuses.
//
// It returns nil — not an error — when nothing was stashed: no pools reader
// configured, pool discovery failed, or this request's permission was anything
// other than ReadMemory (a WRITE request resolves the same pools but approves
// only the write side, so it stashes nothing). nil is the fail-closed default a
// scope-list expansion should read as "expand to nothing," exactly matching
// mintPoolApprovals's own degrade path.
func poolReadScopesFrom(ctx context.Context) []memory.Scope {
	scopes, _ := ctx.Value(poolReadScopesKey{}).([]memory.Scope)
	return scopes
}

// mintPoolApprovals mints approvals for the resource pools session ns/name
// holds slot grants on, alongside the session-scope approval the caller
// already minted for perm. The memory capability door keys on scope.ID alone
// (memory.EnsureApproval matches on (perm, resource) only, with no Kind), so
// this minting IS the authorization for a pool: the existing per-scope doors
// (CompositeSearcher.Search, Local.Query) admit these approvals with no
// second check anywhere. Do not add one — a parallel door is how one of the
// two gets forgotten.
//
// perm is the SAME permission routePermission derived for this request, and
// callers MUST call this only from inside the `if perm, ok := routePermission
// (...); ok` guard — never unconditionally. That is what keeps this mint
// structural rather than a second, looser door: a read-only route (webd's,
// or a per-session bearer's read-only EXTRA scope) must never come away with
// WriteMemory on a pool just because the pool happens to be a write pool for
// this session. Read pools get ReadMemory only when perm is ReadMemory;
// write pools get WriteMemory only when perm is WriteMemory. Any other perm
// (DeleteMemory, ReadKG) mints no pool approval at all — pools are a
// read/write memory concept, not a delete or KG one.
//
// A nil h.pools means this deployment has no resource-pool reader wired
// (no binary has DI'd one in yet, or a test not exercising pools): mint
// nothing and do not error, exactly like every other optional collaborator
// on handler.
//
// A discovery failure also mints nothing, and is logged rather than
// returned: it must never widen access, and it must never break an ordinary
// session-scope request either — pools are additive on top of the session's
// own approval, which is already minted by the time this runs. Because
// nothing is stashed on the failure path either, poolReadScopesFrom degrades
// the same way: a later scope-list expansion (handleSearch) finds nothing and
// expands to nothing, exactly like the mint above it.
//
// This is the only place a READ request resolves pools: it stashes p.Read via
// withPoolReadScopes so handleSearch's scope expansion reuses this resolution
// instead of reading SpiceDB again. Only a read request — the stash sits
// inside the arm that approves those scopes, so what a later handler finds
// there is always something this request may read.
//
// A ?pool= request resolves them a SECOND time, in destinationFor, and that
// duplicate read is deliberate rather than an oversight. The two functions
// disagree on failure policy on purpose — this one logs and continues, because
// pool approvals are additive on top of a session-scope request that must keep
// working; destinationFor refuses, because it is deciding where a write LANDS.
// Feeding the destination proof from a resolution taken under the permissive
// policy would make the strict one inherit the permissive one's degradation.
// The cost is one extra SpiceDB round trip per pool request, and both TOCTOU
// orderings fail closed: a grant revoked between the two makes destinationFor
// refuse, and a grant added between them lets destinationFor allow a write for
// which no WriteMemory approval was minted — refused at the facade door for a
// mutable Kind, and decided by the later, stricter call for an append-only one,
// which is the right way round.
func (h *handler) mintPoolApprovals(ctx context.Context, ns, name, token string, perm memory.Permission) context.Context {
	if h.pools == nil {
		return ctx
	}
	if perm != memory.ReadMemory && perm != memory.WriteMemory {
		return ctx
	}
	p, err := pools.ForSession(ctx, h.pools, ns, name)
	if err != nil {
		log.FromContext(ctx).Info("memory: resource pool discovery failed; serving session scope only",
			"ns", ns, "sess", name, "err", err.Error())
		return ctx
	}
	switch perm {
	case memory.ReadMemory:
		// Stashed HERE, inside the arm that approves these same scopes, so
		// "stashed ⟹ approved on this request" is a property of the code
		// rather than of who happens to read the stash. A WRITE request
		// resolves the same Pools value but mints only write approvals;
		// stashing p.Read there would leave a later expansion naming scopes
		// this request holds no read approval for — fail-closed at the door
		// (a 403), but for a reason nothing in the stash would explain.
		ctx = withPoolReadScopes(ctx, p.Read)
		for _, sc := range p.Read {
			ctx = memory.WithApproval(ctx, memory.ForBearerToken(memory.ReadMemory, sc.ID, tokenIDFor(token)))
		}
	case memory.WriteMemory:
		for _, sc := range p.Write {
			ctx = memory.WithApproval(ctx, memory.ForBearerToken(memory.WriteMemory, sc.ID, tokenIDFor(token)))
		}
	}
	return ctx
}

// destinationFor resolves where a keyed-route request lands. Default: the
// URL's session scope, exactly as before. With ?pool=<type>:<id>: that
// resource's pool, but ONLY once the session is proved to hold a write grant
// on it.
//
// The body remains untrusted for scope (see putEntry). Naming the destination
// on the request line rather than in the entry keeps that property literally
// true, and keeps the addressing visible in the access log rather than buried
// in a payload.
//
// A named pool the session has no grant on is REFUSED, never quietly
// downgraded to the session scope: a silent downgrade writes the data
// somewhere plausible and hides the broken grant, which is the failure a human
// would not notice until the entry was needed and missing.
//
// Proved against p.Write on EVERY method, not only POST. A read-only slot
// makes a resource a read pool (pools.ForSession puts any slot_grant_* in
// Read), and a write is an egress into someone else's pool that the read grant
// never authorized — so "held" is not the question, "writable" is. A GET
// naming a pool is therefore held to the same, stricter proof; that costs
// nothing, because a pool the session may only READ is already reachable
// through _search's own scope expansion, and one gate that cannot disagree
// with itself is worth more than a second, looser one for listings.
//
// Returns (scope, 0, nil) on success and (zero, status, err) on refusal, so
// the caller answers with the status this decided rather than re-deriving one
// from the error's shape.
//
// query is ServeHTTP's already-parsed url.Values, NOT r.URL.Query(): the
// latter discards its parse error, so a query string Go rejects would make
// ?pool= look absent and land the write in the session scope. See the
// url.ParseQuery call in ServeHTTP.
func (h *handler) destinationFor(r *http.Request, query url.Values, sessionScope memory.Scope, ns, name string) (memory.Scope, int, error) {
	// Presence, not a non-empty value: `?pool=` named a destination, just not a
	// resolvable one, and the refusal below is the honest answer to it.
	vals, named := query["pool"]
	if !named {
		return sessionScope, 0, nil
	}
	if len(vals) != 1 {
		// Two destinations is not a destination. Silently taking the first is
		// how a caller's second, wrong ?pool= gets honoured over its first.
		return memory.Scope{}, http.StatusBadRequest,
			fmt.Errorf("?pool= must be given once, got %d values", len(vals))
	}
	raw := vals[0]
	// memory.ParseResourceRef owns both the split and the validation — the
	// SAME parser record_observation's Execute and the info-leakage audience
	// gate's evalPoolWrite use, so this route cannot silently accept a ref one
	// of those two would refuse (or the reverse). A second hand-rolled copy
	// here is a second place for the three to drift.
	dest, err := memory.ParseResourceRef(raw)
	if err != nil {
		return memory.Scope{}, http.StatusBadRequest, fmt.Errorf("?pool= %w", err)
	}
	if h.pools == nil {
		// No reader wired means this server cannot prove a grant, and an
		// unprovable destination is refused rather than downgraded — the same
		// answer as an absent grant, because from the caller's side they are
		// the same fact: the write did not go where it was addressed.
		return memory.Scope{}, http.StatusForbidden,
			fmt.Errorf("memory: pool writes are not enabled on this server")
	}
	p, err := pools.ForSession(r.Context(), h.pools, ns, name)
	if err != nil {
		// Fail closed: a grant set we could not read is unknown, not empty.
		// Logged as well as returned — the caller sees a 403 and cannot tell a
		// SpiceDB fault from a grant it never held, and the operator is the one
		// who can fix the first.
		log.FromContext(r.Context()).Info("memory: refusing a pool write; the session's write pools could not be resolved",
			"ns", ns, "sess", name, "pool", dest.ID, "err", err.Error())
		return memory.Scope{}, http.StatusForbidden,
			fmt.Errorf("memory: could not resolve this session's write pools: %w", err)
	}
	for _, s := range p.Write {
		if s == dest {
			return dest, 0, nil
		}
	}
	// A non-POST is proved against write grants too (see above), so on a read
	// the bare phrase "no write grant" describes an operation the caller did
	// not ask for. Name the route that WOULD serve a read-only pool rather than
	// leaving the caller to guess the pool is unreachable entirely. The proof
	// itself is not relaxed either way.
	hint := ""
	if r.Method != http.MethodPost {
		hint = "; a pool this session may only read is reachable through _search, not here"
	}
	return memory.Scope{}, http.StatusForbidden,
		fmt.Errorf("memory: this session holds no write grant on %s%s", dest.ID, hint)
}

// publisherKeyRequest is the POST body for /memory/_publisher_key.
type publisherKeyRequest struct {
	// KeyID is the hex SHA-256 prefix the Provenance envelope will cite.
	KeyID string `json:"keyId"`
	// PubKey is the base64-std Ed25519 public key; exactly 32 bytes decoded.
	PubKey string `json:"pubKey"`
}

// handlePublisherKey registers a component publisher's Ed25519 signing
// key for verify-on-write. The publisher identity is derived from the
// TOKEN ONLY — a system token maps to its component identity; per-session
// (or unknown) tokens are refused, since session keys are registered
// controller-side, not over this endpoint.
func (h *handler) handlePublisherKey(w http.ResponseWriter, r *http.Request, token string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if h.pubKeyReg == nil {
		http.Error(w, "publisher key registration not supported", http.StatusMethodNotAllowed)
		return
	}

	var publisher string
	switch {
	case h.reg.IsChannelsdToken(token):
		publisher = callerChannelsd
	case h.reg.IsAuthzdToken(token):
		publisher = callerAuthzd
	default:
		http.Error(w, "component key registration requires a system token; session keys are controller-registered", http.StatusForbidden)
		return
	}

	defer r.Body.Close()
	var req publisherKeyRequest
	limitJSONBody(w, r)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "decode publisher key: "+err.Error(), http.StatusBadRequest)
		return
	}
	pub, err := provenance.DecodePubKey(req.PubKey)
	if err != nil {
		http.Error(w, "pubKey "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.KeyID != provenance.KeyID(pub) {
		http.Error(w, "keyId does not match pubKey", http.StatusBadRequest)
		return
	}

	if err := h.pubKeyReg.RegisterPublisherKey(r.Context(), publisher, req.KeyID, pub); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// writeJSON sends v as this response's JSON body under status.
//
// The single place this package's JSON responses are written, so no handler can
// drop the encode error. The status line is already committed by the time
// encoding starts, so the failure cannot become an HTTP error — a server-side
// log is the only trace an operator can get of a response that went out
// truncated or not at all. Info rather than Error: the overwhelmingly common
// cause is a client that hung up mid-body, which is not a server fault.
func writeJSON(w http.ResponseWriter, r *http.Request, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.FromContext(r.Context()).Info("memory: encode response body failed",
			"method", r.Method, "path", r.URL.Path, "status", status, "err", err.Error())
	}
}

// pathSegments splits the path after "/memory/" into its segments. A
// path with the wrong number of segments yields a slice the caller
// rejects as a 404.
func pathSegments(p string) []string {
	rest := strings.TrimPrefix(p, "/memory/")
	if rest == p {
		// Not under /memory/ — the mux should never route here, but be
		// defensive.
		return nil
	}
	return strings.Split(strings.TrimSuffix(rest, "/"), "/")
}

// handleKind serves the keyed routes: POST appends an Entry of kind,
// GET lists the scope's entries of kind.
func (h *handler) handleKind(w http.ResponseWriter, r *http.Request, scope memory.Scope, kind string) {
	switch r.Method {
	case http.MethodPost:
		h.putEntry(w, r, scope, kind)
	case http.MethodGet:
		h.getEntries(w, r, scope, kind)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (h *handler) putEntry(w http.ResponseWriter, r *http.Request, scope memory.Scope, kind string) {
	defer r.Body.Close()
	k, ok := memory.LookupKind(kind)
	if !ok {
		http.Error(w, "unknown Kind "+kind, http.StatusBadRequest)
		return
	}
	var e memory.Entry
	limitJSONBody(w, r)
	if err := json.NewDecoder(r.Body).Decode(&e); err != nil {
		http.Error(w, "decode Entry: "+err.Error(), http.StatusBadRequest)
		return
	}
	// Force the routing keys from the REQUEST LINE — the body is untrusted for
	// scope/kind so a misbehaving client cannot write into a foreign
	// scope or mislabel an entry's Kind. scope is whatever destinationFor
	// decided: the URL's own session, or a resource pool named on the request
	// line and already proved against this session's write grants. Either way
	// it is server-decided, never body-supplied.
	e.Scope = scope
	e.Kind = kind
	if e.ID == "" {
		e.ID = memory.NewID(k)
	}
	stored, err := h.mem.Put(r.Context(), e)
	if err != nil {
		// The client here is another daemon whose own log is not where an
		// operator looks first, so log server-side too, with the routing keys —
		// a store fault or provenance rejection must be greppable at the memory
		// API, not only inferable from a caller's stack.
		status := putStatus(err)
		log.FromContext(r.Context()).Info("memory: put entry failed",
			"scope", scope.ID, "kind", kind, "id", e.ID, "status", status, "err", err.Error())
		httpError(w, err, status)
		return
	}
	writeJSON(w, r, http.StatusCreated, stored)
}

func (h *handler) getEntries(w http.ResponseWriter, r *http.Request, scope memory.Scope, kind string) {
	q := memory.Query{Scope: scope, Kinds: []string{kind}}
	query := r.URL.Query()
	if s := query.Get("limit"); s != "" {
		if n, err := strconv.Atoi(s); err == nil {
			q.Limit = n
		} else {
			http.Error(w, "bad ?limit: "+err.Error(), http.StatusBadRequest)
			return
		}
	}
	if s := query.Get("since"); s != "" {
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			http.Error(w, "bad ?since: "+err.Error(), http.StatusBadRequest)
			return
		}
		q.Since = &t
	}
	if tags := query["tag"]; len(tags) > 0 {
		q.Tags = tags
	}
	res, err := h.mem.Query(r.Context(), q)
	if err != nil {
		failRequest(w, r, scope, "list", err)
		return
	}
	writeJSON(w, r, http.StatusOK, res)
}

func (h *handler) handleQuery(w http.ResponseWriter, r *http.Request, scope memory.Scope) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	defer r.Body.Close()
	var q memory.Query
	limitJSONBody(w, r)
	if err := json.NewDecoder(r.Body).Decode(&q); err != nil {
		http.Error(w, "decode Query: "+err.Error(), http.StatusBadRequest)
		return
	}
	// Force the scope from the URL — never trust the body for routing.
	//
	// Unlike handleSearch, this is NOT widened with the session's read pools:
	// memory.Query.Scope is a single required Scope, not a list, so there is
	// no multi-scope shape to expand into without inventing one. Query stays
	// session-scope only until Query itself grows a scope-list form.
	q.Scope = scope
	res, err := h.mem.Query(r.Context(), q)
	if err != nil {
		failRequest(w, r, scope, "query", err)
		return
	}
	writeJSON(w, r, http.StatusOK, res)
}

func (h *handler) handleSearch(w http.ResponseWriter, r *http.Request, scope memory.Scope) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	defer r.Body.Close()
	var req memory.SearchRequest
	limitJSONBody(w, r)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "decode SearchRequest: "+err.Error(), http.StatusBadRequest)
		return
	}
	// Force the scope list from the URL's session, widened with the session's
	// own READ pools — never the body: a caller cannot name a scope, so the
	// only reachable pools are ones the session provably holds a slot grant
	// on. poolReadScopesFrom returns exactly the pools mintPoolApprovals
	// already minted approvals for on THIS request (same pools.ForSession
	// call, reused rather than repeated) — or nil, which leaves this the
	// session's own scope alone, same as before pools existed.
	req.Scopes = append([]memory.Scope{scope}, poolReadScopesFrom(r.Context())...)
	res, err := h.mem.Search(r.Context(), req)
	if err != nil {
		// ErrNoSearchProviders → 404 comes from sentinelStatus, so every route
		// agrees on it.
		failRequest(w, r, scope, "search", err)
		return
	}
	writeJSON(w, r, http.StatusOK, res)
}

func (h *handler) handleDeleteEntry(w http.ResponseWriter, r *http.Request, scope memory.Scope) {
	if r.Method != http.MethodDelete {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if h.deleter == nil {
		http.Error(w, "delete not supported", http.StatusMethodNotAllowed)
		return
	}
	// NO system component token may delete a session's memory, and THIS GATE is
	// the only thing enforcing it: the channelsd/authzd branches of ServeHTTP
	// mint WithSystemApproval, which satisfies DeleteMemory for ANY scope, and
	// the SpiceDB authorizer short-circuits on a "system:" caller, so nothing
	// downstream refuses. Per-entry deletion is a per-session bearer's operation.
	//
	// It tests the resolved caller against the "system:" PREFIX rather than
	// enumerating the individual tokens, so a system token added later is
	// refused the day it is added. A per-session token can never present one:
	// the AgentSession reconciler registers them with an empty or
	// canonical-user caller.
	if caller, ok := memory.CallerFrom(r.Context()); ok && strings.HasPrefix(caller, systemCallerPrefix) {
		http.Error(w, "forbidden: system component tokens may not delete entries", http.StatusForbidden)
		return
	}
	kind := r.URL.Query().Get("kind")
	id := r.URL.Query().Get("id")
	if kind == "" || id == "" {
		http.Error(w, "kind and id query parameters required", http.StatusBadRequest)
		return
	}
	if err := h.deleter.Delete(r.Context(), scope, kind, id); err != nil {
		status := deleteStatus(err)
		log.FromContext(r.Context()).Info("memory: delete entry failed",
			"scope", scope.ID, "kind", kind, "id", id, "status", status, "err", err.Error())
		httpError(w, err, status)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// putStatus maps a Local.Put error to an HTTP status. Every client is another
// daemon going through httpclient.do, which retries 5xx with backoff and treats
// 4xx as permanent, so this decides whether a failed write is retried at all.
// The arms split by "can a retry help?", not by how the error reads:
//
//   - ErrInvalidEntry — the entry is wrong (unregistered Kind, ID or link ID
//     missing its Kind's prefix). A caller bug; retrying is pointless. 400.
//   - ErrAppendOnlyConflict — an append-only entry exists with different
//     content. A policy outcome the client must see. 409.
//   - ErrProvenanceRequired / ErrBadProvenance — unsigned or forged, so not
//     authorized. Permanent, and retrying would look like a brute-force loop. 403.
//   - anything else — sentinelStatus decides, and its default is 5xx on purpose.
//     A fault is not a verdict: the pre-check Get, the backend Put, or the
//     SpiceDB relationship write failing ARE transient, and answering one 4xx
//     would turn a momentary Postgres blip into a permanently lost write.
func putStatus(err error) int {
	switch {
	case errors.Is(err, memory.ErrInvalidEntry):
		return http.StatusBadRequest
	case errors.Is(err, memory.ErrAppendOnlyConflict):
		return http.StatusConflict
	case errors.Is(err, memory.ErrProvenanceRequired), errors.Is(err, memory.ErrBadProvenance):
		return http.StatusForbidden
	default:
		return sentinelStatus(err)
	}
}

// deleteStatus maps a Local.Delete error to an HTTP status. A refused
// delete on an append-only Kind is a policy outcome (403); everything else
// falls through to the shared sentinel map.
func deleteStatus(err error) int {
	if errors.Is(err, memory.ErrAppendOnlyKind) {
		return http.StatusForbidden
	}
	return sentinelStatus(err)
}

// sentinelStatus decides whether a memory failure is PERMANENT (a caller bug, a
// policy refusal, an unconfigured feature — a 4xx the client must not retry) or
// a FAULT (a 5xx it should).
//
// EVERY route uses it, write-specific maps included, because a route that
// answers its whole error space 500 makes httpclient.do retry a caller bug
// eight times over a ~17s backoff budget. An unresolvable FieldEquals path
// (which the agent's query_memory tool can produce from LLM-supplied arguments)
// and a missing capability approval each burnt most of a turn that way.
//
// Which sentinel gets which status is NOT decided here but in sentinel.Table,
// because httpclient must rebuild the same sentinel from the same response and
// two transcriptions of one mapping drift silently. Everything the table does
// not name is a fault — a backend or SpiceDB call failing, a Graphiti sidecar
// down — so 500, and let the client's backoff ride it out.
//
// A new sentinel belongs in sentinel.Table, which reaches every route AND the
// client at once.
func sentinelStatus(err error) int {
	if status, _, ok := sentinel.StatusAndCode(err); ok {
		return status
	}
	return http.StatusInternalServerError
}

// httpError answers err with status, stamping the sentinel discriminator when
// err is one the wire contract names.
//
// The single place a memory error becomes a response, so no route can answer a
// sentinel without the header that lets the client rebuild it — and, equally,
// none can stamp the header on a refusal that is NOT a sentinel. A bare 403
// from the cross-session token check goes out unstamped, so the client cannot
// mistake it for a capability verdict.
//
// status is passed in rather than recomputed because the write routes map some
// errors of their own (putStatus's 409, deleteStatus's 403) before falling
// through here.
func httpError(w http.ResponseWriter, err error, status int) {
	if _, code, ok := sentinel.StatusAndCode(err); ok {
		w.Header().Set(sentinel.Header, code)
	}
	http.Error(w, err.Error(), status)
}

// failRequest answers err with its mapped status and logs it server-side.
//
// Both halves matter: the status decides whether the caller — another daemon
// going through httpclient.do — retries at all, and the log is the only trace
// an operator has that the memory API refused something, since the client's own
// log is not where they look first. op is a short verb for the log line.
func failRequest(w http.ResponseWriter, r *http.Request, scope memory.Scope, op string, err error) {
	status := sentinelStatus(err)
	log.FromContext(r.Context()).Info("memory: request failed",
		"op", op, "scope", scope.ID, "status", status, "err", err.Error())
	httpError(w, err, status)
}

func (h *handler) handleReindex(w http.ResponseWriter, r *http.Request, scope memory.Scope) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if h.reindexer == nil {
		http.Error(w, "reindex not supported", http.StatusNotImplemented)
		return
	}
	count, err := h.reindexer.Reindex(r.Context(), scope)
	if err != nil {
		failRequest(w, r, scope, "reindex", err)
		return
	}
	writeJSON(w, r, http.StatusOK, map[string]int{"count": count})
}

func (h *handler) handleSignal(w http.ResponseWriter, r *http.Request, scope memory.Scope) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	defer r.Body.Close()
	var sig memory.Signal
	limitJSONBody(w, r)
	if err := json.NewDecoder(r.Body).Decode(&sig); err != nil {
		http.Error(w, "decode Signal: "+err.Error(), http.StatusBadRequest)
		return
	}
	// Force the scope from the URL — never trust the body for routing.
	sig.Scope = scope
	if err := h.mem.SendSignal(r.Context(), sig); err != nil {
		// SendSignal joins every ScopeHook's error, so a 500 can mean one Kind's
		// hook failed while the rest ran. The joined text is the only place that
		// distinction exists — log it, with the signal kind failRequest does not
		// carry, rather than leave a partial dispatch to be inferred from a 500.
		status := sentinelStatus(err)
		log.FromContext(r.Context()).Info("memory: send signal failed",
			"scope", scope.ID, "signal", string(sig.Kind), "status", status, "err", err.Error())
		httpError(w, err, status)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *handler) handleKG(w http.ResponseWriter, r *http.Request, scope memory.Scope) {
	if h.kg == nil {
		http.Error(w, "knowledge graph not configured", http.StatusNotImplemented)
		return
	}
	action := r.URL.Query().Get("action")
	ctx := r.Context()
	ctx = memory.WithKGScope(ctx, scope)
	if err := memory.EnsureApproval(ctx, memory.ReadKG, scope.ID); err != nil {
		// Surface the capability detail (perm+scope) rather than a bare 403, so a
		// missing-mint bug on the KG read path is diagnosable. WRAPPED, not
		// concatenated, and answered through httpError, so this door's refusal
		// reaches the caller as the same ErrMissingApproval the facade's would:
		// two routes refusing for one reason must not arrive as two things.
		httpError(w, fmt.Errorf("forbidden: %w", err), http.StatusForbidden)
		return
	}
	switch action {
	case "search":
		query := r.URL.Query().Get("q")
		limit := 10
		if s := r.URL.Query().Get("limit"); s != "" {
			if n, err := strconv.Atoi(s); err == nil {
				limit = n
			}
		}
		facts, err := h.kg.SearchFacts(ctx, query, limit)
		if err != nil {
			failRequest(w, r, scope, "kg.search", err)
			return
		}
		writeJSON(w, r, http.StatusOK, map[string]interface{}{"facts": facts})
	case "entity":
		uuid := r.URL.Query().Get("uuid")
		if uuid == "" {
			http.Error(w, "uuid required", http.StatusBadRequest)
			return
		}
		ent, err := h.kg.GetEntity(ctx, uuid)
		if err != nil {
			// Two refusals here are PERMANENT — a malformed entity id
			// (ErrInvalidQuery) and an entity in another session's graph
			// (ErrKGScopeMismatch) — so they must not answer 500, or the caller
			// retries a settled verdict eight times.
			failRequest(w, r, scope, "kg.entity", err)
			return
		}
		writeJSON(w, r, http.StatusOK, ent)
	case "entity-facts":
		uuid := r.URL.Query().Get("uuid")
		if uuid == "" {
			http.Error(w, "uuid required", http.StatusBadRequest)
			return
		}
		facts, err := h.kg.EntityFacts(ctx, uuid)
		if err != nil {
			failRequest(w, r, scope, "kg.entity-facts", err)
			return
		}
		writeJSON(w, r, http.StatusOK, map[string]interface{}{"facts": facts})
	case "related":
		uuid := r.URL.Query().Get("uuid")
		if uuid == "" {
			http.Error(w, "uuid required", http.StatusBadRequest)
			return
		}
		limit := 10
		if s := r.URL.Query().Get("limit"); s != "" {
			if n, err := strconv.Atoi(s); err == nil {
				limit = n
			}
		}
		entities, err := h.kg.RelatedEntities(ctx, uuid, limit)
		if err != nil {
			failRequest(w, r, scope, "kg.related", err)
			return
		}
		writeJSON(w, r, http.StatusOK, map[string]interface{}{"entities": entities})
	case "communities":
		comms, err := h.kg.Communities(ctx, scope.ID)
		if err != nil {
			failRequest(w, r, scope, "kg.communities", err)
			return
		}
		writeJSON(w, r, http.StatusOK, map[string]interface{}{"communities": comms})
	default:
		http.Error(w, "unknown kg action: use search, entity, entity-facts, related, or communities", http.StatusBadRequest)
	}
}
