// Package workshopprojectsrv exposes the ONE operator route that lets an
// agent-builder workshop's builder session project a CREDENTIAL-FREE
// STAND-IN for another agent it may want to hand work to during REHEARSAL
// (agent-builder plan 9b, Task 1):
//
//	POST /workshop/project-agent   body: {"namespace","name"}   →   {"name"}
//
// The request names a foreign AgentClass by {namespace, name} — the same
// pair a row of GET /workshop/agents-in-thread (plan 9a,
// pkg/web/workshopthreadsrv) reports as that row's own (Namespace, Class).
// A builder that was pulled into an existing conversation may want the
// agent it is building to hand work to an agent reachable from that
// conversation — but REHEARSING that hand-off inside the workshop must
// never invoke the real agent (its real credentials, tools, or authz
// policy), so this route projects a lightweight, ordinary AgentClass into
// the workshop namespace instead: same system prompt, same resolvable
// skills, no tools, no identity, no authz. The response names it so a
// later task (Task 2) can add it to the builder's own roster.
//
// # New reach this route grants, plainly stated
//
// The stand-in's SystemPrompt is the source class's own — copied verbatim
// into a namespace where the workshop sidecar holds full CRUD on
// AgentClass, so `workshop_get`/`workshop_list` read it back to the builder
// (and the model driving it) VERBATIM. That is genuinely new reach: the
// sidecar otherwise has no read on any AgentClass outside the workshop's
// own namespace. `project_agent`'s stateImpact: external declaration means
// a human approves every call, and that approval is the control — but it
// is only a real control when the approver knows what they are approving.
// Both this package's own tool-facing description (pkg/tools/workshopmcp's
// project_agent) and the underlying route say so outright: projecting a
// stand-in hands this workshop's builder the other agent's own
// instructions to read, not only a name to delegate to.
//
// # Ruling A — reachability, not a name, is the whole authorization story
//
// A class is projectable iff it appears in THIS builder's own
// agents-in-thread result — the exact join workshopthreadsrv.
// ResolveAgentsInThread computes for a GET to that route with the SAME
// bearer. This route calls that function directly rather than re-deriving
// the join a second time (see that function's own doc for why: 9a already
// carries one deliberate duplication — the anchor helpers, pinned by
// anchor_parity_test.go — that needed a parity test to keep honest, and a
// second copy of the whole eight-step join would be worse). A class outside
// that returned set — including one that merely exists in the caller's
// namespace but was never surfaced as a thread participant or its
// descendant — is refused. This adds no new standing: it is exactly the set
// the builder was already shown, nothing more.
//
// Every step in ResolveAgentsInThread denies closed (see that function's
// own doc): a missing/invalid bearer, an unresolvable or not-Ready
// Workshop, a nil or erroring build checker, and a false tuple all refuse
// before this route ever inspects the request body.
//
// # Ruling B — the whitelist IS the security boundary
//
// The stand-in is built as a brand NEW AgentClass value, copying ONLY:
//
//   - DisplayName, Description (Description is REPLACED, not appended
//     verbatim — see below)
//   - SystemPrompt
//   - Skills, by value — but only the RESOLVABLE subset. An AgentSkill's
//     Ref is not self-contained: it is a canonical name that resolves
//     against a Skill or ClusterSkill CR in the REFERENCING CLASS'S OWN
//     NAMESPACE (pkg/controllers/agentclass's validateSkills), so a Ref the
//     source class satisfies in its own namespace does not automatically
//     resolve for a stand-in living in the workshop namespace instead. A
//     Ref that cannot resolve there is DROPPED — see
//     resolveProjectableSkills' own doc for why keeping it would leave the
//     stand-in permanently Valid=False and the rehearsal unable to start at
//     all.
//
// and copying NONE of: AgentIdentity, MCPServers, ToolBundles,
// CredentialExplanations, SidecarToolboxes, WorkspaceSource, Budget,
// BoundEntities, Channels, Authz, Capabilities, CompletionRequirements,
// AgentUI, Model, Harness, IdentityMode and its two timeouts,
// IdentityRecommendation, ToolSessionLog, ToolGuard. See projectStandin's
// own doc for the field-by-field accounting, and
// TestAgentClassSpec_EveryFieldIsExplicitlyDecided (whitelist_test.go) for
// the guard that fails, naming it, the day AgentClassSpec grows a field
// this file has not explicitly decided about — built field BY FIELD, never
// by copying the whole spec and deleting entries: a copy-then-delete
// pattern is a blacklist wearing a whitelist's clothes, and the next field
// AgentClassSpec grows would ship silently.
//
// Subagents (the roster) is DROPPED rather than rewritten: this route
// projects exactly one foreign class per call, so a source class's own
// roster would almost certainly name classes that do not exist in this
// workshop namespace, which the AgentClass reconciler would then mark
// Valid=False for (a roster entry naming a missing class). Recursively
// projecting an entire transitive dependency graph is out of scope for this
// narrow route; SubagentModes is dropped for the same reason (its keys must
// name entries in Subagents). A stand-in therefore never itself delegates —
// it exists so this workshop's builder may hand work TO it, not so IT may
// hand work onward.
//
// # Ruling C — the stand-in is named EXACTLY what it stands in for
//
// (revised from the plan's original "<workshopID>-<name>" prefix scheme —
// see below for why.) The stand-in's metadata.name is the SOURCE class's
// own name, unprefixed, unmodified. This is deliberate, and it removes a
// defect rather than compensating for it:
//
//   - No prefix rule applies to a namespaced AgentClass. Verified against
//     the current tree, not assumed: pkg/controllers/webhooks/workshop/
//     webhook.go's checkClusterToolObject (the ONLY metadata.name-prefix
//     check in that whole admission webhook) is invoked exclusively for
//     the two CLUSTER-SCOPED kinds (SpiceboxToolspec/SpiceboxToolkit,
//     dispatched at webhook.go's checkContent switch) — never for
//     AgentClass. checkAgentClass (the AgentClass-specific content rule)
//     enforces inline prompts, a locally-scoped roster, no agent_builder
//     capability, and no class-declared authz — nothing about
//     metadata.name. A stand-in named "x" in W is exactly as admissible as
//     one named "ws-<id>-x" would have been.
//   - Export's ref-walk never follows spec.subagents at all, so it never
//     pulls a roster-named stand-in into the bundle in the first place —
//     confirmed by TestHandleExportDraft_RosterNamingAStandin_
//     ExcludesTheStandinFromTheBundle in
//     pkg/tools/workshopmcp/tools_export_test.go, not by inspection alone.
//     pkg/platform/oap/source's refDescriptors (cluster_refs.go) is the
//     WHOLE ref-graph walk table for AgentClass, and it has rows for
//     AgentIdentity, AgentUI, MCPServer, SidecarToolbox, SpiceboxClass and
//     SpiceboxToolspec — no row for AgentClass/Subagents at all.
//
// A bare name means the SAME roster entry resolves correctly in BOTH
// worlds, with no rewriting anywhere: inside the workshop, spec.subagents:
// [x] finds the stand-in (an ordinary AgentClass named x, right there in
// W); after install, into the user's OWN chosen install namespace,
// spec.subagents: [x] finds whatever AgentClass named x lives THERE. This
// is namespace-conditional, not automatic (m2): it resolves to the real
// agent only when one of that exact name already exists in the namespace
// the finished agent is installed into — an empty namespace leaves the
// installed agent Valid=False (a roster entry naming a missing class), and
// a namespace that happens to hold a DIFFERENT class of that name binds the
// hand-off to that class instead. request_install's own tool description
// and the builder-deliver skill both say this plainly to the person asking
// for the install, so the caveat travels with the decision rather than
// living only here. The original "<workshopID>-<name>" scheme was rejected
// because it does NOT have even this conditional property: verified that
// pkg/tools/workshopmcp/tools_export.go's stripWorkshopIdentity strips that
// prefix from NEITHER an AgentClass's own metadata.name NOR spec.subagents
// entries, so a prefixed stand-in's name would have shipped, unrewritten,
// straight into the installed class's roster, naming a class that could
// never exist post-install under ANY circumstance. The bare name at least
// makes resolution possible when the operator namespace is prepared for
// it; the prefixed scheme made it impossible outright.
//
// # Ruling D — collision safety is adjudicated by STATUS, never an annotation
//
// AnnotationStandinSource is an ordinary metadata.annotation this route
// stamps, but it is NOT protected: the workshop's own workshop_apply tool
// server-side-applies arbitrary annotations into W, and the admission
// webhook's checkAgentClass never inspects them. So a builder (or a
// prompt-injected model driving one) could stamp the marker onto a class it
// authored itself, and a rule that trusted the annotation's mere presence
// would then let a later, unrelated projection silently overwrite it.
//
// A bare name can collide with something real: the workshop's own builder
// may have authored an AgentClass of the same name as the one being
// projected (coincidentally, or because a prior attempt at the same agent
// used that name). createOrUpdateStandin's collision rule now consults
// Workshop.status.standins (spiceboxv1alpha1.WorkshopStatus.Standins) — an
// operator-owned status field the builder's SA holds no update on — never
// the stand-in's own annotation:
//
//   - status already records an entry naming this bare name for the SAME
//     {sourceNamespace, sourceClassName} this call is projecting ->
//     idempotent UPDATE in place. Re-running project-agent for the same
//     source class converges rather than failing or duplicating.
//   - anything else — no entry names this bare name at all, or one names it
//     for a DIFFERENT source — -> REFUSED with 409 Conflict, naming what
//     collided. NEVER overwritten, regardless of what annotation the
//     colliding object happens to carry. A builder's own authored work (or
//     one source's own prior stand-in) is never at risk from a DIFFERENT
//     projection, no matter what its source class happens to be named.
//
// The per-kind object cap (WorkshopLimits.MaxObjectsPerKind) is enforced
// against every AgentClass actually living in the workshop namespace —
// drafts AND stand-ins together — before a brand-new entry is minted:
// this route writes as the trusted operator, so it never passes through
// the admission webhook's checkLimits, which only ever fires for a write
// attributed to the sidecar's own SA. Counting only status.standins would
// undercount against checkLimits' own per-KIND semantics (every AgentClass
// counts, stand-in or not), leaving a workshop that already holds several
// builder-authored drafts free to mint stand-ins past the webhook's own
// ceiling — and, worse, leaving no budget at all for the builder's own
// draft once the stand-in cap alone was exhausted.
//
// createOrUpdateStandin also refuses outright, before ever touching the
// cluster, when source.Spec.SystemPrompt.ConfigMapRef is set: the
// admission webhook's checkAgentClass already refuses a workshop-authored
// class whose prompt sources from a ConfigMap (the ConfigMap would need to
// live in the WORKSHOP namespace to resolve, and a source class's own
// ConfigMap lives in ITS namespace instead), and this route deliberately
// does not replay any of checkAgentClass's OTHER content rules — a stand-in
// carries no roster, no capabilities, no authz block for those rules to
// ever bite on — so this is the one content rule worth reproducing here
// rather than silently minting a stand-in that can never actually serve a
// prompt.
//
// # Provenance and unmistakability
//
// With the name no longer signaling anything (it is now identical to a
// real agent's own name), Workshop.status.standins and the Description's
// stand-in-first sentence are the two things distinguishing a projected
// stand-in from a real, builder-authored AgentClass — status is the
// AUTHORITATIVE one (Ruling D, above); AnnotationStandinSource is stamped
// alongside purely as a human-readable label for a person reading `kubectl
// get agentclass -o yaml`, and MUST NEVER be trusted for anything else (see
// its own doc comment, pkg/apis/v1alpha1/workshop_types.go). Both are
// therefore mandatory and asserted directly by this package's own tests:
// status names the exact {namespace, name} of the verified-reachable
// source, the annotation mirrors it for humans, and the Description's
// FIRST sentence always states that this is a stand-in holding no
// credentials — never buried after the source's own description.
//
// # A pre-existing, RELATED consequence — recorded, not fixed here
//
// Because export's ref-walk never follows spec.subagents (the fact Ruling
// C's second bullet leans on), a builder authoring a genuine multi-agent
// system — one AgentClass whose roster names OTHER AgentClasses that same
// builder ALSO authored, as real, exportable agents, not stand-ins — would
// export a bundle missing those other classes entirely. This route neither
// causes nor fixes that: it is a property of pkg/platform/oap/source's
// ref-walk table having no row for AgentClass, orthogonal to stand-ins.
// Recorded as a finding for the plan-8 scenario work; not chased here.
//
// # Write discipline
//
// Mirrors pkg/web/workshopdraftsrv: an in-process, trusted operator caller
// (not a session bearer) writing through its own manager client. The write
// is idempotent by identity rather than by digest (workshopdraftsrv's
// analogue): re-projecting the SAME source class overwrites the existing
// stand-in's spec in place — a Create that hits AlreadyExists falls back to
// a Get+Update, refusing instead when status names anything other than this
// exact source at that identity (Ruling D) — rather than accumulating a
// second object or ever overwriting a builder's own work; the stand-in's
// identity is a pure function of (workshopID, sourceClassName), never a
// counter or a timestamp. Retries once on a conflicting ResourceVersion on
// the AgentClass update path, so two concurrent calls re-projecting the
// same source (project_agent makes this reachable) converge instead of one
// surfacing a raw 500.
//
// A fresh projection records the {name, sourceNamespace, sourceName} triple
// onto Workshop.status.standins via the status subresource, retrying on a
// conflicting ResourceVersion (fresh Get, re-find, re-append) rather than
// failing the first time a concurrent writer touches the same Workshop's
// status — five other writers touch it — so the collision rule above, and
// pkg/tools/workshopmcp's soleAuthoredClassName, both have an authoritative
// record to consult on their next call. If every retry is exhausted, the
// just-created AgentClass is DELETED and the failure logged with the
// {namespace, name, source} it names, rather than left behind as an object
// the registry has no record of: an orphan like that is indistinguishable
// from a builder-authored class to soleAuthoredClassName (export would ship
// it) and to request_install (an admin would be offered a credential-free
// copy of it under the REAL agent's own name).
//
// Before any of the above runs, this route prunes any status.standins entry
// whose own AgentClass no longer exists (deleted directly, e.g. via
// workshop_delete): otherwise that entry blocks its bare name from ever
// being re-projected (the update path 404s trying to update an object that
// is gone) and, once the builder authors a genuinely new class under that
// freed name, wrongly excludes it from soleAuthoredClassName's authored
// count. The update path additionally creates rather than errors on a
// NotFound Get, as defense in depth against the same race.
package workshopprojectsrv

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/conditions"
	"github.com/authzed/openagentprimitives/pkg/memory/tokens"
	"github.com/authzed/openagentprimitives/pkg/web/workshopthreadsrv"
)

// Path is the route this package mounts.
const Path = "/workshop/project-agent"

// MaxRequestBytes hard-caps the request body: a {namespace, name} pair is a
// few dozen bytes at most, so this exists only to keep a malformed or
// hostile POST from hanging the operator, mirroring the same discipline
// workshopdraftsrv.MaxDraftBytes documents for its own (much larger) body.
const MaxRequestBytes = 4 << 10 // 4 KiB

// AnnotationStandinSource re-exports spiceboxv1alpha1.AnnotationStandinSource
// under this route's own name (mirroring the WorkshopBuildChecker alias
// below) — declared in pkg/apis rather than here purely so both this route
// and a human inspecting its output share one constant name. NOT the
// security boundary: see that constant's own doc comment
// (pkg/apis/v1alpha1/workshop_types.go) — Workshop.status.standins is the
// authoritative registry this route's collision check and
// pkg/tools/workshopmcp's soleAuthoredClassName both consult instead.
const AnnotationStandinSource = spiceboxv1alpha1.AnnotationStandinSource

// WorkshopBuildChecker is workshopthreadsrv.WorkshopBuildChecker, reused
// under this route's own name — mirroring how workshopdraftsrv and
// workshopthreadsrv each already declare the identical interface locally
// rather than sharing a named type, so each package's tests can inject
// their own fake with no live SpiceDB, and this handler never carries a
// typed pointer that could be assigned nil into an interface field
// (CLAUDE.md's typed-nil rule).
type WorkshopBuildChecker = workshopthreadsrv.WorkshopBuildChecker

// projectRequest is the POST body: the foreign AgentClass's own
// {namespace, name} — the same pair a GET /workshop/agents-in-thread row
// reports as (Namespace, Class).
type projectRequest struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

// projectResponse is the 200 body: the stand-in's own metadata.name, in
// the workshop's own namespace (W) — the identity a later step (Task 2)
// adds to the builder's own roster.
type projectResponse struct {
	Name string `json:"name"`
}

// NewHandler returns the POST /workshop/project-agent handler.
//
//   - c resolves the caller's own Workshop CR, the builder session and its
//     thread participants' AgentSessions (via
//     workshopthreadsrv.ResolveAgentsInThread), reads the reachable source
//     AgentClass, and creates/updates the projected stand-in. The route
//     runs IN the operator, so it writes through c directly rather than
//     needing its own session bearer with mutate scope — same shape as
//     workshopdraftsrv.
//   - reg authenticates the bearer.
//   - build answers workshop:<W>#build for the derived (B, X); nil is
//     handled at request time (denied, not panicked) per the typed-nil rule.
//   - log is threaded through to workshopthreadsrv.ResolveAgentsInThread,
//     which uses it to record a per-participant delegation-closure walk
//     failure without failing the whole reachability computation.
func NewHandler(c client.Client, reg *tokens.Registry, build WorkshopBuildChecker, log logr.Logger) http.Handler {
	h := &handler{c: c, reg: reg, build: build, log: log}
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+Path, h.serveHTTP)
	return mux
}

type handler struct {
	c     client.Client
	reg   *tokens.Registry
	build WorkshopBuildChecker
	log   logr.Logger
}

func (h *handler) serveHTTP(w http.ResponseWriter, r *http.Request) {
	authz := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if !strings.HasPrefix(authz, prefix) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="workshop-project-agent"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	token := strings.TrimPrefix(authz, prefix)

	ctx := r.Context()

	// Runs the IDENTICAL join GET /workshop/agents-in-thread runs for this
	// same bearer (Ruling A): authenticate, resolve+validate the Workshop
	// CR, re-check workshop:<W>#build, resolve the builder session (B/X),
	// and compute its thread's participant/descendant set. A failure here
	// denies closed with the same status/message that route would have
	// returned for this bearer.
	res, rerr := workshopthreadsrv.ResolveAgentsInThread(ctx, h.c, h.reg, h.build, h.log, token)
	if rerr != nil {
		if rerr.Status == http.StatusUnauthorized {
			w.Header().Set("WWW-Authenticate", `Bearer realm="workshop-project-agent"`)
		}
		http.Error(w, rerr.Message, rerr.Status)
		return
	}

	defer r.Body.Close()
	limited := http.MaxBytesReader(w, r.Body, MaxRequestBytes)
	body, err := io.ReadAll(limited)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "read request body: "+err.Error(), http.StatusBadRequest)
		return
	}
	var req projectRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "decode request body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.Namespace == "" || req.Name == "" {
		http.Error(w, "namespace and name are both required", http.StatusBadRequest)
		return
	}

	// THE authorization check (Ruling A): the requested class must appear
	// as the .Class of some row in res.Agents — the EXACT set this bearer's
	// own GET /workshop/agents-in-thread would have returned. This adds no
	// new standing; it is a reachability test against what the builder was
	// already shown, nothing else.
	if !reachable(res.Agents, req.Namespace, req.Name) {
		http.Error(w, fmt.Sprintf(
			"class %s/%s is not reachable from this builder's own conversation thread; refusing to project a stand-in for it",
			req.Namespace, req.Name), http.StatusForbidden)
		return
	}

	var source spiceboxv1alpha1.AgentClass
	if err := h.c.Get(ctx, types.NamespacedName{Namespace: req.Namespace, Name: req.Name}, &source); err != nil {
		http.Error(w, fmt.Sprintf("reachable class %s/%s could not be read: %v", req.Namespace, req.Name, err), http.StatusInternalServerError)
		return
	}

	// A source whose prompt sources from a ConfigMap would project a
	// stand-in that can never resolve it — the ConfigMap lives in source's OWN
	// namespace, not the workshop's. checkAgentClass already refuses this for
	// a workshop-authored class; this is the one webhook content rule worth
	// replaying here (this route deliberately does not replay the others —
	// see this package's own doc, Ruling D), because unlike the others it
	// would otherwise silently mint a stand-in that cannot work at all.
	if source.Spec.SystemPrompt.ConfigMapRef != nil {
		http.Error(w, fmt.Sprintf(
			"class %s/%s sources its system prompt from a ConfigMap, which would not resolve inside the workshop namespace; refusing to project an unresolvable stand-in",
			req.Namespace, req.Name), http.StatusUnprocessableEntity)
		return
	}

	// Resolve each skill ref against W's own Skill/ClusterSkill CRs
	// BEFORE building the stand-in — see resolveProjectableSkills' own doc
	// for why an unresolvable ref must be dropped rather than copied, and
	// this package's own doc (Ruling B) for the rule it enforces.
	keptSkills, droppedSkillRefs, err := resolveProjectableSkills(ctx, h.c, res.WorkshopID, source.Spec.Skills)
	if err != nil {
		http.Error(w, fmt.Sprintf("resolving skills for %s/%s against the workshop namespace: %v", req.Namespace, req.Name, err), http.StatusInternalServerError)
		return
	}

	standin := projectStandin(&source, res.WorkshopID, req.Namespace, req.Name, keptSkills, droppedSkillRefs)
	wsKey := types.NamespacedName{Namespace: res.SessionNamespace, Name: spiceboxv1alpha1.WorkshopName(res.SessionName)}
	if err := createOrUpdateStandin(ctx, h.c, wsKey, standin, req.Namespace, req.Name, h.log); err != nil {
		var collision *errCollision
		if errors.As(err, &collision) {
			http.Error(w, collision.Error(), http.StatusConflict)
			return
		}
		http.Error(w, "create stand-in AgentClass: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(projectResponse{Name: standin.Name}); err != nil {
		// The stand-in already landed; only the response encode failed (e.g. a
		// client that closed the connection mid-write). Logged per CLAUDE.md's
		// never-silently-drop-an-error rule rather than dropped outright — the
		// caller cannot be told anything more at this point (WriteHeader has
		// already committed the 200 status).
		h.log.Info("workshopprojectsrv: encoding project-agent response failed", "standin", standin.Name, "err", err.Error())
	}
}

// reachable reports whether (ns, class) appears as the (Namespace, Class)
// of some row in agents — the reachability test Ruling A describes. class
// is an AgentClass name, not a session name: a row's own Name field (the
// SESSION name) is deliberately not part of this comparison.
func reachable(agents []workshopthreadsrv.AgentInThread, ns, class string) bool {
	for i := range agents {
		if agents[i].Namespace == ns && agents[i].Class == class {
			return true
		}
	}
	return false
}

// projectStandin builds a NEW, credential-free AgentClass value for
// namespace workshopID by copying ONLY the fields decided PROJECTED below
// from source — every other AgentClassSpec field is left at its zero
// value. This is a field-by-field WHITELIST, never "copy the struct, then
// delete the dangerous fields": see this file's own package doc (Ruling B)
// and TestAgentClassSpec_EveryFieldIsExplicitlyDecided
// (whitelist_test.go), which fails and NAMES any AgentClassSpec field this
// function has not been updated to account for.
//
// PROJECTED (copied from source):
//   - DisplayName  — a friendly label; carries no credential.
//   - Description  — REPLACED with a stand-in-first sentence (never
//     buried after the source's own description), which is then appended
//     when non-empty, so a reader still learns what the real agent does.
//   - SystemPrompt — copied verbatim. NOTE: if source's prompt is a
//     ConfigMapRef (rather than Inline), the reference names a ConfigMap
//     in source's OWN namespace and will not resolve inside the workshop
//     namespace — a known limitation of a plain field-by-field copy, left
//     for a follow-up to resolve inline at projection time if it proves
//     to matter in practice.
//   - Skills       — copied BY VALUE, but only the subset resolveProjectable
//     Skills found resolvable+Valid=True against W's OWN Skill/ClusterSkill
//     CRs: an AgentSkill's Ref is NOT self-contained — it resolves
//     against a Skill/ClusterSkill in the REFERENCING class's own namespace
//     — so a Ref the source satisfies in its own namespace does not
//     automatically resolve for a stand-in projected into the workshop
//     namespace instead. keptSkills is the caller-resolved subset;
//     droppedSkillRefs names what did not resolve there, purely so the
//     stand-in's own Description can tell a builder what it is missing.
//   - UserPreferences — deep-copied (deepCopyUserPreferences), unlike
//     Skills' plain value copy, because UserPreferenceSchema carries a
//     pointer (Default) and a slice (Enum) that a shallow copy would alias
//     onto source's own spec. Credential-free by construction (the field's
//     own doc forbids declaring a secret as a preference) and the values
//     themselves live per-user in the memory plane, never on the class —
//     so projecting the schema leaks nothing. Rehearsal wants behavior
//     parity: the preferences capability offers tools and a prompt section
//     that change agent behavior, and a stand-in that dropped the schema
//     would rehearse a materially different agent. The stand-in's bare
//     name differs from any workshop the source class might also live in,
//     so a rehearsal save keys a distinct preference entry ID and can
//     never collide with the real class's own saved values.
//
// NEVER PROJECTED (left at zero value; the security boundary):
//   - AgentIdentity, IdentityMode, CredentialLinkTimeout,
//     IdentityRecommendation, IdentityChoiceTimeout — no credential, ever.
//   - ToolBundles, MCPServers, SidecarToolboxes, WorkspaceSource,
//     CredentialExplanations, ToolGuard — no tool access, ever.
//   - Capabilities — no meta-tool capability grant.
//   - Authz — no slots, no start gate, no interact grant, no plan gate: a
//     stand-in carries no authorization policy of its own.
//   - Channels — a stand-in is never channel-attached.
//   - Model, Harness, ToolSessionLog — irrelevant to a rehearsal stand-in;
//     Model absent means the tier default applies, Harness absent means
//     ap-native.
//   - Budget — the tier default applies.
//   - Subagents, SubagentModes, BoundEntities (deprecated) — DROPPED. A
//     source class's own roster would almost certainly name classes this
//     workshop does not have, which the AgentClass reconciler marks
//     Valid=False for; recursively projecting a whole dependency graph is
//     out of scope for this one-class-at-a-time route. See this package's
//     own doc for the full rationale.
//   - CompletionRequirements, AgentUI — a stand-in never runs a session
//     and is never UI-callable.
//   - Config, ConfigSchema — the config plane feeds the CEL constraints of
//     bound toolspecs, which a stand-in never carries; dropped together so
//     the reconciler's config-against-schema validation sees neither
//     keys nor schema.
func projectStandin(source *spiceboxv1alpha1.AgentClass, workshopID, sourceNamespace, sourceClassName string, keptSkills []spiceboxv1alpha1.AgentSkill, droppedSkillRefs []string) *spiceboxv1alpha1.AgentClass {
	desc := fmt.Sprintf(
		"STAND-IN for %s/%s — holds no credentials, tools, or authorization; for delegation rehearsal only.",
		sourceNamespace, sourceClassName)
	if source.Spec.Description != "" {
		desc += " " + source.Spec.Description
	}
	if len(droppedSkillRefs) > 0 {
		// Name what was dropped so a builder reading the stand-in's
		// own description is not misled about why it behaves thinly — this is
		// not a bug report, it is an honest accounting of what a
		// credential-free rehearsal double can and cannot carry.
		desc += fmt.Sprintf(" (Skipped %d skill(s) that do not resolve in this workshop: %s.)",
			len(droppedSkillRefs), strings.Join(droppedSkillRefs, ", "))
	}

	return &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: workshopID,
			// Bare — exactly the source class's own name (Ruling C). NOT
			// "<workshopID>-<name>": that scheme was rejected because
			// nothing rewrites it back on export (see the package doc).
			Name: sourceClassName,
			Annotations: map[string]string{
				AnnotationStandinSource: sourceNamespace + "/" + sourceClassName,
			},
		},
		Spec: spiceboxv1alpha1.AgentClassSpec{
			DisplayName:     source.Spec.DisplayName,
			Description:     desc,
			SystemPrompt:    source.Spec.SystemPrompt,
			Skills:          append([]spiceboxv1alpha1.AgentSkill(nil), keptSkills...),
			UserPreferences: deepCopyUserPreferences(source.Spec.UserPreferences),
		},
	}
}

// deepCopyUserPreferences returns an independent copy of prefs for the
// stand-in's own spec. AgentSkill (Skills, above) is all scalar fields, so
// a plain append-into-a-nil-slice value copy is already a genuine deep
// copy; UserPreferenceSchema is not — it carries a pointer (Default) and a
// slice (Enum), so the same shallow copy would leave the stand-in aliasing
// the SOURCE class's own spec through those fields. Mirrors the per-item
// DeepCopyInto loop zz_generated.deepcopy.go uses for AgentClassSpec's own
// UserPreferences field.
func deepCopyUserPreferences(prefs []spiceboxv1alpha1.UserPreferenceSchema) []spiceboxv1alpha1.UserPreferenceSchema {
	if prefs == nil {
		return nil
	}
	out := make([]spiceboxv1alpha1.UserPreferenceSchema, len(prefs))
	for i := range prefs {
		prefs[i].DeepCopyInto(&out[i])
	}
	return out
}

// resolveProjectableSkills resolves each of skills' Ref against a
// materialized Skill (namespace workshopNamespace) or ClusterSkill that is
// itself Valid=True — the EXACT test pkg/controllers/agentclass's
// validateSkills runs when deciding whether an AgentClass may reach
// Valid=True, reproduced here against the WORKSHOP namespace rather than
// the source class's own.
//
// An AgentSkill.Ref is not a self-contained value: it is a canonical name
// that resolves against a Skill/ClusterSkill CR in the REFERENCING class's
// OWN namespace. A source class's skill resolves in the source's namespace
// — the workshop has no Skill CR of its own for it, so copying the Ref
// verbatim onto a stand-in projected into the workshop namespace would
// almost always leave validateSkills unable to resolve it there, parking
// the stand-in at Valid=False/AgentClassSkillMissing FOREVER (a namespace
// Skill cannot be conjured into existence by this route, and nothing here
// should try). classIsValid then refuses to start a session for it, so the
// rehearsal this whole route exists for never runs — the exact defect this
// function closes.
//
// Dropping an unresolvable ref is not a compromise: a stand-in exists to
// rehearse the HAND-OFF, not to do the real agent's actual work, so a
// thinner stand-in that reaches Valid=True and can actually be talked to
// serves that purpose better than a byte-identical copy that can never
// start at all. The dropped Refs are returned so the caller can name them
// on the stand-in's own Description (projectStandin), rather than silently
// thinning the skill set with no trace of what happened.
func resolveProjectableSkills(ctx context.Context, c client.Client, workshopNamespace string, skills []spiceboxv1alpha1.AgentSkill) (kept []spiceboxv1alpha1.AgentSkill, droppedRefs []string, err error) {
	if len(skills) == 0 {
		return nil, nil, nil
	}

	// Namespace Skills first — they shadow a ClusterSkill of the same
	// canonical name, mirroring validateSkills' own namespace-wins rule.
	var nsList spiceboxv1alpha1.SkillList
	if err := c.List(ctx, &nsList, client.InNamespace(workshopNamespace)); err != nil {
		return nil, nil, fmt.Errorf("listing Skills in workshop namespace %s: %w", workshopNamespace, err)
	}
	validByCanonical := make(map[string]bool, len(nsList.Items))
	seenByCanonical := make(map[string]bool, len(nsList.Items))
	for i := range nsList.Items {
		sk := &nsList.Items[i]
		seenByCanonical[sk.Spec.CanonicalName] = true
		validByCanonical[sk.Spec.CanonicalName] = conditions.IsTrue(sk.Status.Conditions, spiceboxv1alpha1.SkillConditionValid)
	}

	var clusterList spiceboxv1alpha1.ClusterSkillList
	if err := c.List(ctx, &clusterList); err != nil {
		return nil, nil, fmt.Errorf("listing ClusterSkills: %w", err)
	}
	for i := range clusterList.Items {
		csk := &clusterList.Items[i]
		if seenByCanonical[csk.Spec.CanonicalName] {
			continue // shadowed by a namespace Skill of the same canonical name
		}
		validByCanonical[csk.Spec.CanonicalName] = conditions.IsTrue(csk.Status.Conditions, spiceboxv1alpha1.SkillConditionValid)
	}

	for _, s := range skills {
		if validByCanonical[s.Ref] {
			kept = append(kept, s)
		} else {
			droppedRefs = append(droppedRefs, s.Ref)
		}
	}
	sort.Strings(droppedRefs) // deterministic Description text across calls
	return kept, droppedRefs, nil
}

// errCollision reports that a stand-in's bare name already names a
// DIFFERENT object in the workshop — one this workshop's builder authored
// itself, not a prior stand-in projection. Ruling D: never overwritten.
// serveHTTP translates this into 409 Conflict, naming what collided.
type errCollision struct {
	namespace, name string
}

func (e *errCollision) Error() string {
	return fmt.Sprintf(
		"%s/%s already names an AgentClass this workshop's builder authored — refusing to overwrite it with a stand-in",
		e.namespace, e.name)
}

// findStandin returns ws.Status.Standins' entry named name, or nil — the
// ONE place both createOrUpdateStandin and (indirectly, via
// pkg/tools/workshopmcp reading the same field off its own Get) the
// authored-class count consult. Never the stand-in AgentClass's own
// annotation (Ruling D).
func findStandin(ws *spiceboxv1alpha1.Workshop, name string) *spiceboxv1alpha1.WorkshopStandin {
	for i := range ws.Status.Standins {
		if ws.Status.Standins[i].Name == name {
			return &ws.Status.Standins[i]
		}
	}
	return nil
}

// pruneDeadStandins returns ws.Status.Standins with every entry removed
// whose own AgentClass is no longer among existingNames — e.g. deleted
// directly via workshop_delete. Left in place, a dead entry would (a) 404
// the update path forever, permanently blocking that bare name from ever
// being re-projected, and (b) once a builder authors a genuinely new class
// under the freed name, wrongly exclude it from
// pkg/tools/workshopmcp's soleAuthoredClassName authored-count — a status
// record must never outlive the object it describes. changed reports
// whether anything was actually removed, so the caller only pays for a
// Status().Update when pruning had something to do.
func pruneDeadStandins(standins []spiceboxv1alpha1.WorkshopStandin, existingNames map[string]bool) (pruned []spiceboxv1alpha1.WorkshopStandin, changed bool) {
	for _, st := range standins {
		if existingNames[st.Name] {
			pruned = append(pruned, st)
		} else {
			changed = true
		}
	}
	return pruned, changed
}

// createOrUpdateStandin creates standin, or — when its bare-name identity
// is already taken — decides what to do based on the WORKSHOP'S OWN
// authoritative registry, wsKey's Workshop CR status.standins (Ruling D),
// never the stand-in object's own annotation:
//
//   - status already records this bare name for the SAME
//     {sourceNamespace, sourceClassName} -> idempotent UPDATE in place
//     (retried once on a conflicting ResourceVersion), so re-projecting the
//     same source class converges rather than accumulating a second object
//     or failing.
//   - anything else — no entry at all, or one naming a DIFFERENT source —
//     -> refused with *errCollision. Never overwritten, regardless of what
//     annotation the object occupying that identity happens to carry.
//
// Before either branch runs, prunes any status.standins entry whose own
// AgentClass has been deleted (pruneDeadStandins). A fresh projection
// additionally enforces the per-kind object cap against every AgentClass
// actually in the workshop namespace — drafts and stand-ins together,
// matching the admission webhook's own per-KIND checkLimits semantics —
// before minting anything, and records the new entry onto status once the
// AgentClass itself is created, retrying on a conflicting write and
// compensating with a delete if every retry is exhausted (log is
// used only on that compensating path, so a nil logr.Logger from a caller
// that never hits it is never dereferenced).
func createOrUpdateStandin(ctx context.Context, c client.Client, wsKey types.NamespacedName, standin *spiceboxv1alpha1.AgentClass, sourceNamespace, sourceClassName string, log logr.Logger) error {
	var ws spiceboxv1alpha1.Workshop
	if err := c.Get(ctx, wsKey, &ws); err != nil {
		return fmt.Errorf("get Workshop %s: %w", wsKey, err)
	}

	// List every AgentClass actually living in the workshop namespace once:
	// both the dead-entry prune and the per-kind object cap need
	// this same live count/membership.
	var acList spiceboxv1alpha1.AgentClassList
	if err := c.List(ctx, &acList, client.InNamespace(standin.Namespace)); err != nil {
		return fmt.Errorf("listing AgentClass objects in workshop %s: %w", standin.Namespace, err)
	}
	existingNames := make(map[string]bool, len(acList.Items))
	for i := range acList.Items {
		existingNames[acList.Items[i].Name] = true
	}

	if pruned, changed := pruneDeadStandins(ws.Status.Standins, existingNames); changed {
		ws.Status.Standins = pruned
		if err := c.Status().Update(ctx, &ws); err != nil {
			return fmt.Errorf("pruning dead stand-in entries from workshop status: %w", err)
		}
	}

	if prior := findStandin(&ws, standin.Name); prior != nil {
		if prior.SourceNamespace != sourceNamespace || prior.SourceName != sourceClassName {
			// status names a DIFFERENT source for this bare name: refused, and
			// the AgentClass at that identity (which belongs to THAT source) is
			// never touched.
			return &errCollision{namespace: standin.Namespace, name: standin.Name}
		}
		// Idempotent re-projection of the SAME recorded source: converge the
		// AgentClass in place. No field of a recorded entry can ever differ
		// between two projections of the same source, so status itself does
		// not change on this path — nothing to write.
		return updateStandinWithRetry(ctx, c, standin)
	}

	// No status entry names this bare name at all. Enforce the per-kind
	// object cap before minting a new one: this route writes as the operator
	// and so bypasses the admission webhook's checkLimits, which only ever
	// fires for a write attributed to the sidecar's own SA. Counted against
	// EVERY AgentClass in the namespace, not len(status.standins) alone
	// — the webhook counts by kind, not by whether an object is a stand-in.
	if int32(len(acList.Items)) >= ws.Spec.Limits.MaxObjectsPerKind {
		return fmt.Errorf(
			"workshop already has %d AgentClass object(s) (drafts and stand-ins combined), at its per-kind object limit of %d; remove or export one before projecting another",
			len(acList.Items), ws.Spec.Limits.MaxObjectsPerKind)
	}

	if err := c.Create(ctx, standin.DeepCopy()); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return err
		}
		// Something already occupies this identity, and status records NO
		// stand-in there at all: the workshop builder's own authored work (or
		// an object carrying a FORGED AnnotationStandinSource — see this
		// package's own doc, Ruling D). Never overwritten, whatever it
		// carries.
		return &errCollision{namespace: standin.Namespace, name: standin.Name}
	}

	if err := recordStandinStatus(ctx, c, wsKey, standin.Name, sourceNamespace, sourceClassName); err != nil {
		// The AgentClass landed, but the status record that makes it
		// discoverable as a stand-in did not — every retry (recordStandinStatus)
		// was exhausted. Left as-is, this object would be an ORPHAN: absent from
		// the registry, so soleAuthoredClassName counts it as builder-authored
		// (export_draft would ship it) and, with no draft yet authored,
		// export_draft/request_install would offer an admin a credential-free
		// copy of it under the REAL agent's own name. Compensate by deleting it
		// rather than leaving that behind.
		if delErr := c.Delete(ctx, standin); delErr != nil && !apierrors.IsNotFound(delErr) {
			log.Info("workshopprojectsrv: recording stand-in status failed AND the compensating delete also failed; this AgentClass is now an orphan requiring manual cleanup",
				"namespace", standin.Namespace, "name", standin.Name,
				"sourceNamespace", sourceNamespace, "sourceName", sourceClassName,
				"statusErr", err.Error(), "deleteErr", delErr.Error())
			return fmt.Errorf("record stand-in %s in workshop status: %w (compensating delete ALSO failed: %v)", standin.Name, err, delErr)
		}
		log.Info("workshopprojectsrv: recording stand-in status failed; deleted the just-created AgentClass to avoid an unregistered orphan",
			"namespace", standin.Namespace, "name", standin.Name,
			"sourceNamespace", sourceNamespace, "sourceName", sourceClassName, "err", err.Error())
		return fmt.Errorf("record stand-in %s in workshop status: %w (the just-created AgentClass was deleted to prevent an orphan)", standin.Name, err)
	}
	return nil
}

// recordStandinStatus appends {name, sourceNamespace, sourceName} onto
// wsKey's Workshop.status.standins, retrying on a conflicting write with a
// FRESH Get + re-find + re-append each attempt rather than
// blindly repeating the same stale Update — five other writers touch this
// same Workshop's status, so an ordinary conflict is the expected case, not
// an edge case. Idempotent against its own retries: if a prior attempt's
// Update actually landed before a transient error was observed, the
// re-Get's findStandin already shows the entry recorded and this returns
// nil without appending a duplicate.
func recordStandinStatus(ctx context.Context, c client.Client, wsKey types.NamespacedName, standinName, sourceNamespace, sourceClassName string) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var ws spiceboxv1alpha1.Workshop
		if err := c.Get(ctx, wsKey, &ws); err != nil {
			return err
		}
		if e := findStandin(&ws, standinName); e != nil {
			if e.SourceNamespace == sourceNamespace && e.SourceName == sourceClassName {
				return nil // already recorded — a retry after a prior Update actually landed
			}
			// Unreachable in practice: this identity was just Create()'d
			// successfully under the SAME collision check createOrUpdateStandin
			// just ran, so no other entry should be able to name it here.
			// Surfaced rather than silently overwriting a foreign record.
			return fmt.Errorf("workshop status already names %s for a different source (%s/%s); refusing to overwrite", standinName, e.SourceNamespace, e.SourceName)
		}
		ws.Status.Standins = append(ws.Status.Standins, spiceboxv1alpha1.WorkshopStandin{
			Name: standinName, SourceNamespace: sourceNamespace, SourceName: sourceClassName,
		})
		return c.Status().Update(ctx, &ws)
	})
}

// updateStandinWithRetry converges an existing stand-in AgentClass onto
// standin's own annotations/spec, retrying once on a conflicting
// ResourceVersion: a stale read surfaced as a raw 500 before this fix,
// defeating this route's own claim that re-projecting the same source
// converges rather than merely usually converging. Re-reads the existing
// object on the retry rather than blindly repeating the same write, since a
// stale copy would only conflict again.
//
// Annotations are MERGED, not replaced wholesale: a bare `existing.
// Annotations = standin.Annotations` would silently discard any annotation
// this object carries that this route did not itself stamp, which is not
// this route's call to make.
//
// Falls back to a plain Create when the Get 404s: the recorded status
// entry named this identity, but the AgentClass itself was deleted out from
// under it (e.g. workshop_delete) — defense in depth alongside
// createOrUpdateStandin's own proactive prune, for the race between that
// prune's List and this function's own Get.
func updateStandinWithRetry(ctx context.Context, c client.Client, standin *spiceboxv1alpha1.AgentClass) error {
	const maxAttempts = 2
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		var existing spiceboxv1alpha1.AgentClass
		err := c.Get(ctx, types.NamespacedName{Namespace: standin.Namespace, Name: standin.Name}, &existing)
		if apierrors.IsNotFound(err) {
			return c.Create(ctx, standin.DeepCopy())
		}
		if err != nil {
			return fmt.Errorf("get existing stand-in: %w", err)
		}
		if existing.Annotations == nil {
			existing.Annotations = make(map[string]string, len(standin.Annotations))
		}
		for k, v := range standin.Annotations {
			existing.Annotations[k] = v
		}
		existing.Spec = standin.Spec
		lastErr = c.Update(ctx, &existing)
		if lastErr == nil || !apierrors.IsConflict(lastErr) {
			return lastErr
		}
	}
	return lastErr
}
