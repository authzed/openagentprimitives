// Package workshop holds the admission webhook that is the heart of the
// agent-builder lockdown (spec layer 1.4): it fires on every write the
// workshop sidecar's ServiceAccount makes to the workshop-authored kinds,
// attributes the SA to a session, walks that session's workshop and its
// SpiceDB `workshop#build` tuple, then enforces the per-kind content rules
// ordinary RBAC cannot express — no fragment schema, allowlisted sidecar
// images only, no nested builder, no cross-namespace reference, prefixed
// cluster-scoped tool CRs only, and per-kind object limits.
//
// RBAC alone gets a workshop namespace's Role to grant create/update/patch on
// the seven namespaced kinds and a shared ClusterRole to grant create on the
// two cluster-scoped tool kinds (pkg/controllers/workshop/rbac.go) — but RBAC
// cannot pin `create` by resource NAME, cannot inspect an object's fields, and
// cannot count "objects belonging to THIS workshop". Every one of those gaps
// is this webhook's job.
package workshop

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/apimage"
	"github.com/authzed/openagentprimitives/pkg/tools/apiadapter"
	"github.com/authzed/openagentprimitives/pkg/tools/skills/canonical"
	"github.com/authzed/openagentprimitives/pkg/x/safehttp"
)

// PathWorkshopObject is the webhook's HTTP path. It must match
// config/manager/webhook.yaml.
const PathWorkshopObject = "/validate-workshop-object"

// workshopSASuffix and saUsernamePrefix mirror
// pkg/controllers/webhooks/subagentrequest's runnerSASuffix pattern for a
// different principal: the workshop sidecar's ServiceAccount, named
// "<session>-workshop-sa" by spiceboxv1alpha1.WorkshopServiceAccountName.
// Duplicated rather than shared deliberately — see that package's own note on
// why a shared constant would couple two independently-configured admission
// gates. Both must stay in sync with WorkshopServiceAccountName; the RBAC
// sufficiency harness covers that name.
const (
	workshopSASuffix = "-workshop-sa"
	saUsernamePrefix = "system:serviceaccount:"
)

// agentBuilderCapabilityKey is the Capabilities map key
// pkg/agent/tool/meta/capability/agentbuilder.go registers under
// (agentBuilderCapability.Name()). Duplicated as a local string rather than
// imported: the capability package is the declaration half of the sanction
// and has no business depending on (or being depended on by) the admission
// layer that enforces it.
const agentBuilderCapabilityKey = "agent_builder"

// imageAPIAdapter is the one first-party sidecar image plan 7b's config
// carrier rule applies to. Derived from apimage.APIAdapter.Name — the single
// source of truth for the image's real name — rather than a second literal,
// so checkSidecarImage's allowlist and checkSidecarConfig's per-image branch
// can never silently drift apart from what oap actually builds and pins.
var imageAPIAdapter = apimage.APIAdapter.Name

// allowedWorkshopSidecarImages is the first-party allowlist for a
// workshop-authored SidecarToolbox's spec.source.image (spec §1.4 / plan
// 3-7): the image NAMES a builder session may hand its own sidecars. Built
// from apimage.WorkshopSidecarImages — see that var's own doc for why it is
// the single source of truth this allowlist and the workshop sidecar's
// `inventory` tool both derive from, so the two can never silently drift
// apart. This is necessary but not sufficient — checkSidecarImage also pins
// the registry the image was pulled from, since a bare name match alone would
// admit "attacker.example/anything/ap-workshop:tag" (arbitrary code in the
// workshop namespace, from a registry nobody vetted) just as readily as the
// real image.
var allowedWorkshopSidecarImages = func() map[string]bool {
	m := make(map[string]bool, len(apimage.WorkshopSidecarImages))
	for _, im := range apimage.WorkshopSidecarImages {
		m[im.Name] = true
	}
	return m
}()

// allowedWorkshopSubagentModes is the delegation-surface ceiling for a
// SubagentRequest a workshop sidecar creates. spiceboxv1alpha1.SubagentModesAll
// lists four modes; this set names the three that a locked-down build space
// may grant itself: single_turn (headless, no channel at all), task (a
// channel that may only ASK, never be driven), and attended (a channel for a
// full back-and-forth, but with the HUMAN running the workshop, not with
// another agent). chat is excluded: it hands the child a full two-way
// conversational channel to the parent AGENT, which is exactly the unbounded
// agent-to-agent surface a locked-down build space must not be able to grant
// itself — attended's conversation is with the person driving the build, so
// admitting it does not hand the workshop that same autonomy.
var allowedWorkshopSubagentModes = map[string]bool{
	spiceboxv1alpha1.SubagentModeSingleTurn: true,
	spiceboxv1alpha1.SubagentModeTask:       true,
	spiceboxv1alpha1.SubagentModeAttended:   true,
}

// allWorkshopKinds is every req.Kind.Kind value this webhook governs: the
// seven namespaced kinds workshopKindResources
// (pkg/controllers/workshop/rbac.go) grants RBAC create on, plus the two
// cluster-scoped tool kinds spicebox-workshop-toolwriter grants. Used both to
// dispatch content rules and to sum the workshop's total object count for the
// maxObjects limit.
var allWorkshopKinds = []string{
	"AgentClass", "MCPServer", "SidecarToolbox", "AgentIdentity",
	"Skill", "AgentUI", "SubagentRequest",
	"SpiceboxToolspec", "SpiceboxToolkit",
}

func isClusterScopedWorkshopKind(kind string) bool {
	return kind == "SpiceboxToolspec" || kind == "SpiceboxToolkit"
}

// WorkshopBuildChecker answers workshop:<workshopID>#build for a session —
// satisfied by *pkg/authz/spicedb.Client.CheckWorkshopBuild.
type WorkshopBuildChecker interface {
	CheckWorkshopBuild(ctx context.Context, workshopID, sessNS, sessName string) (bool, error)
}

// Webhook validates admission requests for every workshop-authored object.
type Webhook struct {
	client               client.Reader
	tuples               WorkshopBuildChecker
	trustedImageRegistry string
	decoder              admission.Decoder
}

// New constructs the webhook. client reads Workshop CRs, the workshop
// namespace, ClusterAgentSettings and SpiceboxClass/SpiceboxToolspec
// resources this webhook cross-checks against; tuples answers the SpiceDB
// standing check. trustedImageRegistry is the operator's OWN first-party
// image registry (the "<registry>" half of apimage.Image.RegistryRef, e.g.
// "ghcr.io/example" for a remote install, or "" for a local/dev cluster
// running bare :dev images) — see checkSidecarImage's doc for why a
// SidecarToolbox image name match alone is not enough. decoder must be built
// from the manager's scheme.
func New(c client.Reader, tuples WorkshopBuildChecker, trustedImageRegistry string, d admission.Decoder) *Webhook {
	return &Webhook{client: c, tuples: tuples, trustedImageRegistry: trustedImageRegistry, decoder: d}
}

// Handle implements the ordered gate described in the package doc: attribute
// the SA (1), resolve the caller's own workshop and confirm the object
// belongs to it (2), confirm the SpiceDB standing tuple (3), enforce the
// per-kind content rules (4), then the per-kind and total object limits on
// CREATE (5). It denies, verbatim, at the first failure.
func (w *Webhook) Handle(ctx context.Context, req admission.Request) admission.Response {
	// 1. Attribute the SA. A non-workshop-SA principal is governed by
	// ordinary RBAC (this branch is normally unreachable: matchConditions in
	// config/manager/webhook.yaml is expected to narrow dispatch to
	// "*-workshop-sa" principals only — this is defense in depth against that
	// registration drifting). A principal that LOOKS like a workshop SA but
	// does not parse into a session is fail-closed: a guard whose
	// unrecognized direction is permissive is not a guard.
	sessNS, session, isWorkshopSA := workshopSession(req.UserInfo.Username)
	if !isWorkshopSA {
		return admission.Allowed("requester is not a workshop sidecar ServiceAccount")
	}
	if sessNS == "" || session == "" {
		return admission.Denied("the requesting ServiceAccount " + req.UserInfo.Username +
			" cannot be attributed to a session, so its workshop cannot be verified")
	}

	// DELETE (and any other operation) carries nothing this gate inspects;
	// config/manager/webhook.yaml only ever dials this webhook for CREATE and
	// UPDATE, but the check is defensive in exactly the same spirit as step 1.
	if req.Operation != admissionv1.Create && req.Operation != admissionv1.Update {
		return admission.Allowed("")
	}

	// 2. Resolve the caller's own workshop.
	var ws spiceboxv1alpha1.Workshop
	if err := w.client.Get(ctx, types.NamespacedName{Namespace: sessNS, Name: spiceboxv1alpha1.WorkshopName(session)}, &ws); err != nil {
		return admission.Denied(fmt.Sprintf(
			"session %s/%s has no workshop this write can be attributed to: %v", sessNS, session, err))
	}
	if ws.Status.Namespace == "" {
		return admission.Denied(fmt.Sprintf(
			"the workshop for session %s/%s is not yet provisioned", sessNS, session))
	}
	workshopID := ws.Status.Namespace

	if !isClusterScopedWorkshopKind(req.Kind.Kind) {
		if req.Namespace != workshopID {
			return admission.Denied(fmt.Sprintf(
				"objects may only be written into your own workshop namespace %q, not %q", workshopID, req.Namespace))
		}
		var objNS corev1.Namespace
		if err := w.client.Get(ctx, types.NamespacedName{Name: req.Namespace}, &objNS); err != nil {
			return admission.Denied(fmt.Sprintf("could not confirm workshop namespace %q: %v", req.Namespace, err))
		}
		if objNS.Labels[spiceboxv1alpha1.LabelWorkshopSessionNamespace] != sessNS ||
			objNS.Labels[spiceboxv1alpha1.LabelWorkshopSessionName] != session {
			return admission.Denied(fmt.Sprintf(
				"namespace %q is not labeled for session %s/%s: this workshop does not own it", req.Namespace, sessNS, session))
		}
	}
	// A cluster-scoped toolspec/toolkit's own LabelWorkshopNamespace is
	// checked as part of its content rule below (checkClusterToolObject),
	// which needs the decoded object's labels anyway — see that function's
	// doc for why this is the same check the brief describes as step 2 for
	// the cluster-scoped case.

	// 3. The SpiceDB standing tuple. FullyConsistent per
	// (*spicedb.Client).CheckWorkshopBuild — this is a privilege gate asking
	// moments after every provisioning write. An error is fail-closed too: an
	// unconfirmable tuple is a no, not a maybe.
	ok, err := w.tuples.CheckWorkshopBuild(ctx, workshopID, sessNS, session)
	if err != nil {
		return admission.Denied(fmt.Sprintf(
			"could not confirm workshop %s#build for session %s/%s: %v", workshopID, sessNS, session, err))
	}
	if !ok {
		return admission.Denied(fmt.Sprintf(
			"session %s/%s does not hold build permission on workshop %s", sessNS, session, workshopID))
	}

	// 4. Per-kind content rules.
	if resp := w.checkContent(ctx, req, &ws, workshopID); !resp.Allowed {
		return resp
	}

	// 5. Per-kind and total object limits — CREATE only.
	if req.Operation == admissionv1.Create {
		if resp := w.checkLimits(ctx, req, &ws, workshopID); !resp.Allowed {
			return resp
		}
	}

	return admission.Allowed("")
}

// workshopSession splits a per-workshop sidecar ServiceAccount's
// authenticated username into the namespace and session it speaks for. See
// pkg/controllers/webhooks/subagentrequest.runnerSession's doc for why the
// prefix and suffix are checked against the WHOLE username before any
// namespace/name split — the same fail-closed shape applies here.
func workshopSession(username string) (ns, session string, isWorkshopSA bool) {
	if !strings.HasPrefix(username, saUsernamePrefix) {
		return "", "", false
	}
	rest := strings.TrimPrefix(username, saUsernamePrefix)
	if !strings.HasSuffix(rest, workshopSASuffix) {
		return "", "", false
	}
	nsPart, saName, ok := strings.Cut(rest, ":")
	if !ok {
		return "", "", true
	}
	return nsPart, strings.TrimSuffix(saName, workshopSASuffix), true
}

// checkContent decodes req's object into its concrete type and runs that
// kind's content rules. Every kind also gets the two "ANY object" checks the
// brief names: SubagentRequest.spec.parent is the one field, across all nine
// kinds, that can name a namespace at all (every other cross-object
// reference in these types is structurally same-namespace or, for the two
// cluster-scoped kinds, carries no namespace concept), so that rule is
// enforced entirely inside checkSubagentRequest. The "no cluster-scoped
// reference except the prefix+label-bound toolspec/toolkit" rule has TWO
// homes, both inside checkAgentClass: spec.toolBundles (a SpiceboxClass ref
// via .class, a SpiceboxToolspec ref via .toolspecs[]) and spec.skills[].ref
// (a canonical skill name that, unless it names the reserved "local"
// authority, resolves against a git fetch or a cluster-scoped ClusterSkill).
// Fix round 1 added the skills half after a review found it unchecked — see
// the "Fix round 1" section of this package's task report.
func (w *Webhook) checkContent(ctx context.Context, req admission.Request, ws *spiceboxv1alpha1.Workshop, workshopID string) admission.Response {
	switch req.Kind.Kind {
	case "AgentClass":
		var obj spiceboxv1alpha1.AgentClass
		if err := w.decoder.Decode(req, &obj); err != nil {
			return admission.Errored(http.StatusBadRequest, err)
		}
		if reason := w.checkAgentClass(ctx, &obj, workshopID); reason != "" {
			return admission.Denied(reason)
		}
	case "MCPServer":
		var obj spiceboxv1alpha1.MCPServer
		if err := w.decoder.Decode(req, &obj); err != nil {
			return admission.Errored(http.StatusBadRequest, err)
		}
		if reason := checkMCPServer(&obj); reason != "" {
			return admission.Denied(reason)
		}
	case "SidecarToolbox":
		var obj spiceboxv1alpha1.SidecarToolbox
		if err := w.decoder.Decode(req, &obj); err != nil {
			return admission.Errored(http.StatusBadRequest, err)
		}
		if reason := w.checkSidecarToolbox(ctx, &obj); reason != "" {
			return admission.Denied(reason)
		}
	case "AgentIdentity", "Skill", "AgentUI":
		// No kind-specific content rule beyond the SA/namespace/tuple checks
		// already run: none of these three types carries a namespace-bearing
		// or cluster-scoped-reference field (AgentIdentity's credential Secret
		// refs are structurally same-namespace-only — see SecretKeyRef/
		// SecretRef's own doc comments — and AgentUI's Tools/Actions name
		// tools, not objects).
	case "SubagentRequest":
		var obj spiceboxv1alpha1.SubagentRequest
		if err := w.decoder.Decode(req, &obj); err != nil {
			return admission.Errored(http.StatusBadRequest, err)
		}
		if reason := checkSubagentRequest(&obj, ws); reason != "" {
			return admission.Denied(reason)
		}
	case "SpiceboxToolspec":
		var obj spiceboxv1alpha1.SpiceboxToolspec
		if err := w.decoder.Decode(req, &obj); err != nil {
			return admission.Errored(http.StatusBadRequest, err)
		}
		if reason := checkClusterToolObject(obj.ObjectMeta, workshopID); reason != "" {
			return admission.Denied(reason)
		}
	case "SpiceboxToolkit":
		var obj spiceboxv1alpha1.SpiceboxToolkit
		if err := w.decoder.Decode(req, &obj); err != nil {
			return admission.Errored(http.StatusBadRequest, err)
		}
		if reason := checkClusterToolObject(obj.ObjectMeta, workshopID); reason != "" {
			return admission.Denied(reason)
		}
	default:
		return admission.Denied(fmt.Sprintf(
			"kind %q is not one of the workshop-authored kinds this gate governs", req.Kind.Kind))
	}
	return admission.Allowed("")
}

// checkAgentClass enforces: an inline system prompt only (no ConfigMap
// fragment injected into a compiled prompt out of band); a roster naming
// only bare class names (Subagents is documented as "in its own namespace" —
// no field on the type can express a foreign namespace — but a qualified-
// looking entry, e.g. containing "/" or ":", is refused rather than trusted,
// since plan 4 is expected to add an outbound exception through a NEW field
// and this one must not silently grow that meaning first); no agent_builder
// capability (no nested builder); an empty start gate and interact grant (no
// class-declared authz policy); and the ANY-object cluster-scoped-reference
// rule, which for AgentClass is entirely about spec.toolBundles — the only
// place any of the nine workshop-authored kinds can name a cluster-scoped
// object at all. A bundle's Class names a SpiceboxClass directly (not a
// toolspec/toolkit) and so is refused outright; a bundle's Toolspecs must
// each resolve to a SpiceboxToolspec this SAME workshop owns.
func (w *Webhook) checkAgentClass(ctx context.Context, ac *spiceboxv1alpha1.AgentClass, workshopID string) string {
	if ac.Spec.SystemPrompt.ConfigMapRef != nil {
		return "AgentClass.spec.systemPrompt must be inline: a workshop-authored class may not source its prompt from a ConfigMap"
	}
	for _, subagent := range ac.Spec.Subagents {
		if strings.ContainsAny(subagent, "/:") {
			return fmt.Sprintf(
				"AgentClass.spec.subagents entry %q may only name a class in the workshop namespace, not a qualified reference", subagent)
		}
	}
	if _, granted := ac.Spec.Capabilities[agentBuilderCapabilityKey]; granted {
		return "AgentClass.spec.capabilities must not grant agent_builder: a workshop-authored class may not be a nested builder"
	}
	session := ac.Spec.GetAuthz().GetSession()
	if len(session.AllowedStarters) > 0 {
		return "AgentClass.spec.authz.session.allowedStarters must be empty: a workshop-authored class may not declare its own start gate"
	}
	if session.InteractPermission != "" {
		return "AgentClass.spec.authz.session.interactPermission must be empty: a workshop-authored class may not declare its own interact grant"
	}
	// ANY-object cluster-ref rule, third instance: spec.skills[].ref is a
	// canonical skill name that, unless it names the reserved "local"
	// authority, resolves against either a git-fetched skill (an external
	// fetch this build space must not trigger) or a cluster-scoped
	// ClusterSkill (a cluster-scoped reference exactly like an unbound
	// SpiceboxClass). Per spec §1, a workshop AgentClass may reference only
	// skills it authored itself, in its own namespace — canonical.Parse's
	// IsLocal is the actual predicate the rest of the codebase (skill
	// resolution, SkillSource) uses to draw this same line, so it is used
	// here rather than a naive "local//" string match.
	for _, sk := range ac.Spec.Skills {
		parsed, err := canonical.Parse(sk.Ref)
		if err != nil || !parsed.IsLocal {
			return fmt.Sprintf(
				"AgentClass.spec.skills[%s]: this agent references a skill from outside its build space; a build space may only use skills authored in it", sk.Name)
		}
	}
	for _, tb := range ac.Spec.ToolBundles {
		if tb.Class != "" {
			return fmt.Sprintf(
				"AgentClass.spec.toolBundles[%s].class %q is a cluster-scoped reference a workshop-authored class may not make", tb.Name, tb.Class)
		}
		for _, ts := range tb.Toolspecs {
			if reason := w.checkReferencedToolspec(ctx, ts, workshopID); reason != "" {
				return fmt.Sprintf("AgentClass.spec.toolBundles[%s].toolspecs: %s", tb.Name, reason)
			}
		}
	}
	return ""
}

// checkReferencedToolspec confirms toolspecName resolves to a
// SpiceboxToolspec this workshop itself owns — the same name-prefix and
// LabelWorkshopNamespace proof checkClusterToolObject requires when the
// toolspec is CREATED, applied here to a REFERENCE instead. A reference that
// cannot be resolved is denied fail-closed, the same direction as an
// unconfirmable SpiceDB tuple.
func (w *Webhook) checkReferencedToolspec(ctx context.Context, toolspecName, workshopID string) string {
	var ts spiceboxv1alpha1.SpiceboxToolspec
	if err := w.client.Get(ctx, types.NamespacedName{Name: toolspecName}, &ts); err != nil {
		return fmt.Sprintf("references SpiceboxToolspec %q, which could not be confirmed as this workshop's own: %v", toolspecName, err)
	}
	if reason := checkClusterToolObject(ts.ObjectMeta, workshopID); reason != "" {
		return fmt.Sprintf("references SpiceboxToolspec %q: %s", toolspecName, reason)
	}
	return ""
}

// checkMCPServer enforces: no SpiceDB schema fragment (a workshop-authored
// server may not extend the cluster-wide composed schema), and a server URL
// whose host passes safehttp.GuardHost (no metadata/link-local/loopback/
// cluster-internal destination).
func checkMCPServer(m *spiceboxv1alpha1.MCPServer) string {
	if m.Spec.SpiceDBSchema != nil {
		return "MCPServer.spec.spicedbSchema must be absent: a workshop-authored server may not extend the cluster-wide composed schema"
	}
	u, err := url.Parse(m.Spec.Server.URL)
	if err != nil || u.Hostname() == "" {
		return fmt.Sprintf("MCPServer.spec.server.url %q could not be parsed into a host", m.Spec.Server.URL)
	}
	if err := safehttp.GuardHost(u.Hostname()); err != nil {
		return fmt.Sprintf("MCPServer.spec.server.url %q is refused: %v", m.Spec.Server.URL, err)
	}
	return ""
}

// checkSidecarToolbox enforces: a pinned first-party image FROM the trusted
// registry only (NOT an inline/adapter source), no secret-gated inputs (a gate
// no fixture can satisfy, per the brief), a sandbox class resolving to an
// allowed backend kind, and — once every prior gate has admitted a first-party
// image — that spec.config is consumed by that SPECIFIC image and, when it
// is, that it is well-formed.
func (w *Webhook) checkSidecarToolbox(ctx context.Context, st *spiceboxv1alpha1.SidecarToolbox) string {
	// The inline/adapter tier lets a SidecarToolbox name an arbitrary
	// spec.source.inline.baseImage and layer a script onto it. A workshop
	// sidecar must run a pinned, allowlisted first-party image and nothing else,
	// so the inline source is refused EXPLICITLY here — ahead of the image check
	// — rather than left to fail incidentally on an empty spec.source.image. A
	// future change that made checkSidecarImage tolerate an empty ref would
	// otherwise silently reopen an arbitrary-base-image path.
	if st.Spec.Source.Inline != nil {
		return "SidecarToolbox.spec.source.inline is not permitted: a workshop-authored toolbox must use a pinned first-party image, not an inline/adapter base image"
	}
	imageName, reason := checkSidecarImage(w.trustedImageRegistry, st.Spec.Source.Image)
	if reason != "" {
		return reason
	}
	if len(st.Spec.SecretInputs) > 0 {
		return "SidecarToolbox.spec.secretInputs must be empty: a workshop-authored toolbox may not gate on a secret handle"
	}
	if reason := w.checkSandboxClassAllowed(ctx, st.Spec.Sandbox.Class); reason != "" {
		return reason
	}
	// Every check above has already admitted a specific, allowlisted
	// first-party image (imageName) — so it is safe to branch on which one.
	return checkSidecarConfig(imageName, st)
}

// checkSidecarConfig enforces the plan 7b "config carrier" contract:
// spec.config is opaque to the platform (SidecarToolboxSpec.Config's own
// doc), so admissibility is decided by whichever image actually consumes it.
// Only ap-api-adapter does today. For that image, the SAME parser the
// sidecar binary boots with (apiadapter.Parse) decides whether a config is
// admissible — a config this webhook admits is therefore one the sidecar
// will actually serve, and every denial below returns the parser's own words
// verbatim rather than a re-derived summary. For any other allowlisted
// image, a non-empty config is refused outright: nothing else reads it, and
// admitting it silently would let an author believe it does something.
func checkSidecarConfig(imageName string, st *spiceboxv1alpha1.SidecarToolbox) string {
	if imageName != imageAPIAdapter {
		if st.Spec.Config != "" {
			return fmt.Sprintf("SidecarToolbox.spec.config is not consumed by image %s; only %s reads a config", imageName, imageAPIAdapter)
		}
		return ""
	}

	if st.Spec.Config == "" {
		return fmt.Sprintf("SidecarToolbox.spec.config is required for image %s: the adapter has no tools without it", imageAPIAdapter)
	}
	cfg, err := apiadapter.Parse([]byte(st.Spec.Config))
	if err != nil {
		return fmt.Sprintf("SidecarToolbox.spec.config is not a valid adapter config: %v", err)
	}
	// UpstreamEnvVarMismatch is the SAME check pkg/tools/kinds/sidecartoolbox's
	// ValidateFile runs client-side: a config that authenticates but whose
	// auth.envVar disagrees with spec.upstreamAuth.envVar would otherwise pass
	// both this admission check and validate_spec, only to fail when the
	// sidecar actually tries to authenticate at boot.
	if msg := cfg.UpstreamEnvVarMismatch(st.Spec.UpstreamAuth.EnvVar); msg != "" {
		return msg
	}
	declared := make([]string, 0, len(st.Spec.Tools))
	for _, t := range st.Spec.Tools {
		declared = append(declared, t.Name)
	}
	// ToolNamesMatch is the SAME check pkg/tools/kinds/sidecartoolbox's
	// ValidateFile runs client-side (dedup included), so an author sees the
	// identical refusal locally that admission would give.
	return cfg.ToolNamesMatch(declared)
}

// checkSidecarImage enforces BOTH halves of "first-party image": the
// repository's last path segment must be an allowlisted NAME, AND the
// reference must actually come from this cluster's trusted registry — a
// name match alone would admit "attacker.example/anything/ap-workshop:tag"
// exactly as readily as the real image, since nothing downstream
// (pkg/controllers/sidecartoolbox, pkg/agent/tool/sidecartoolbox) re-checks
// the registry the sidecar image was actually pulled from.
//
// Returns the allowlisted image NAME it validated (imageName, e.g.
// "ap-api-adapter") alongside the usual denial reason ("" on success) —
// checkSidecarToolbox branches its spec.config rule on that same name rather
// than re-splitting the ref by hand.
//
// Uses go-containerregistry's name package (already a go.mod dependency,
// used the same way in cmd/oap/internal/agentcmd/install_env.go) rather than
// hand-rolling image-ref parsing — registry/repository/tag/digest splitting
// has enough edge cases (implicit "library/" namespacing, the docker.io vs
// index.docker.io alias, scoped names) that a hand-rolled basename split is
// exactly the class of bug CLAUDE.md's "prefer a library" rule warns about,
// and IS the bug this fix round exists to close.
//
// trustedImageRegistry is the "<registry>" apimage.Image.RegistryRef(reg)
// composes with — "" on a local/dev cluster (apimage.Image.LocalRef, a bare
// "<name>:<tag>" with no registry at all), otherwise the exact registry (and
// any org/path prefix, e.g. "ghcr.io/example") the operator's own first-party
// images were pushed to and pulled from. A ref is admitted iff:
//
//   - its repository's last path segment is an allowlisted name, AND
//   - EITHER it carries no explicit registry at all (the bare local-dev
//     form, a SINGLE repository path segment) and trustedImageRegistry is
//     itself "" (a local cluster, where bare refs are how every first-party
//     image is named — including the operator's own), OR it carries an
//     explicit registry+path prefix that matches trustedImageRegistry
//     exactly.
//
// Any other combination — a foreign registry, an implicit-Docker-Hub org ref
// ("authzed/ap-workshop", no registry segment but more than one path
// segment), or a bare name on a cluster that DOES have a configured trusted
// registry — is refused.
//
// FIX ROUND 2: "no explicit registry" must be decided from the RAW,
// un-parsed ref (refShape below), never from a parsed field. Fix round 1
// used `repo.RegistryStr() == name.DefaultRegistry` ("index.docker.io") as
// its "bare ref" test — but go-containerregistry's own alias normalization
// (name.NewRegistry: an unqualified ref defaults to index.docker.io, AND an
// explicit "docker.io/…" is rewritten to the identical value) makes a
// genuinely bare "ap-workshop:dev" and an EXPLICIT
// "docker.io/attacker/ap-workshop:v1" parse to the exact same RegistryStr().
// Under an empty trustedImageRegistry (every local/desktop install, and the
// custom-`--sandbox-image` fallback this package's checkSidecarImage doc
// already flagged), that collapse reopened the arbitrary-registry
// vulnerability this whole function exists to close, just spelled
// "docker.io" instead of an arbitrary host. Deciding from the raw string,
// before that normalization ever runs, is what closes it for good.
func checkSidecarImage(trustedImageRegistry, ref string) (imageName, reason string) {
	parsed, err := name.ParseReference(ref)
	if err != nil {
		return "", fmt.Sprintf("SidecarToolbox.spec.source.image %q could not be parsed: %v", ref, err)
	}
	repo := parsed.Context()
	repoStr := repo.RepositoryStr()
	imageName = repoStr
	registryPrefix := repo.RegistryStr()
	if i := strings.LastIndex(repoStr, "/"); i >= 0 {
		imageName = repoStr[i+1:]
		registryPrefix = repo.RegistryStr() + "/" + repoStr[:i]
	}

	if !allowedWorkshopSidecarImages[imageName] {
		return imageName, fmt.Sprintf("SidecarToolbox.spec.source.image %q is not one of the first-party workshop images", ref)
	}

	hasSlash, explicitRegistry := refShape(ref)
	if explicitRegistry == "" {
		// No explicit registry. Trusted only when this is the genuine bare
		// local-dev form — a SINGLE repository path segment ("ap-workshop:dev",
		// never "authzed/ap-workshop:dev": that second shape is an IMPLICIT
		// Docker Hub org reference, not a bare name, and must be refused
		// exactly like an explicit docker.io/… one) — and only on a cluster
		// with no configured trusted registry. hasSlash, not repoStr, is what
		// decides "single segment": RepositoryStr() injects an implicit
		// "library/" prefix for a bare Docker-Hub-default name
		// (hasImplicitNamespace in go-containerregistry), which would make
		// even the genuine bare form look multi-segment if repoStr were
		// tested instead.
		if trustedImageRegistry == "" && !hasSlash {
			return imageName, ""
		}
		return imageName, fmt.Sprintf(
			"SidecarToolbox.spec.source.image %q names no registry; this cluster's trusted first-party registry is %q",
			ref, trustedImageRegistry)
	}

	if trustedImageRegistry != "" && registryPrefix == trustedImageRegistry {
		return imageName, ""
	}
	return imageName, fmt.Sprintf(
		"SidecarToolbox.spec.source.image %q is not from this cluster's trusted first-party registry", ref)
}

// refShape reports, from the RAW (unparsed) image reference ref, whether it
// carries any "/"-delimited path structure at all (hasSlash) and, when it
// does, whether the first segment is registry-shaped (explicitRegistry,
// "" when not). Mirrors go-containerregistry's own heuristic exactly
// (pkg/name/repository.go, NewRepository): the first segment names a
// registry iff it contains "." or ":" or equals "localhost" — the same test
// name.ParseReference itself uses to decide whether to split a registry off
// the front of a reference, reproduced here because the package exposes no
// public helper for it. A "/" can never appear inside a tag or digest (Docker
// tag/digest grammar forbids it), so this is safe to compute directly on the
// full raw reference — tag or digest suffix, and all — without stripping it
// first.
func refShape(ref string) (hasSlash bool, explicitRegistry string) {
	first, _, found := strings.Cut(ref, "/")
	if !found {
		return false, ""
	}
	if first == "localhost" || strings.ContainsAny(first, ".:") {
		return true, first
	}
	return true, ""
}

// checkSandboxClassAllowed resolves className to its backend kind (via the
// SpiceboxClass CR's spec.sandbox.kind, defaulting like SandboxBackend.
// ResolvedKind does) and confirms that kind is allowed: the cluster's own
// SettingsLimits.AllowedSandboxKinds ceiling when the admin has set one
// (ClusterAgentSettings "cluster" — the singleton read/write every other
// settings webhook uses), else the built-in default set of just
// spiceboxv1alpha1.DefaultSandboxKind ("pod") — the brief's "AllowedSandboxKinds
// shape — read from settings, else the built-in default set". A missing
// ClusterAgentSettings singleton is not an error (most installs never create
// one); any OTHER read error is fail-closed.
func (w *Webhook) checkSandboxClassAllowed(ctx context.Context, className string) string {
	var class spiceboxv1alpha1.SpiceboxClass
	if err := w.client.Get(ctx, types.NamespacedName{Name: className}, &class); err != nil {
		return fmt.Sprintf("SidecarToolbox.spec.sandbox.class %q could not be resolved: %v", className, err)
	}
	kind := class.Spec.Sandbox.ResolvedKind()

	allowed := []string{spiceboxv1alpha1.DefaultSandboxKind}
	var cas spiceboxv1alpha1.ClusterAgentSettings
	err := w.client.Get(ctx, types.NamespacedName{Name: spiceboxv1alpha1.ClusterAgentSettingsName}, &cas)
	switch {
	case err == nil:
		if cas.Spec.Limits != nil && cas.Spec.Limits.AllowedSandboxKinds != nil {
			allowed = *cas.Spec.Limits.AllowedSandboxKinds
		}
	case apierrors.IsNotFound(err):
		// No cluster-tier settings at all: fall through with the built-in
		// default set.
	default:
		return fmt.Sprintf("could not read the cluster sandbox-kind allowlist: %v", err)
	}
	for _, k := range allowed {
		if k == kind {
			return ""
		}
	}
	return fmt.Sprintf(
		"SidecarToolbox.spec.sandbox.class %q resolves to sandbox kind %q, which is not in the allowed set %v",
		className, kind, allowed)
}

// checkSubagentRequest enforces: spec.parent names exactly the workshop's own
// session (the one field, across all nine workshop-authored kinds, that can
// name a foreign namespace at all — see checkContent's doc), and spec.mode is
// one of the bounded delegation surfaces this build space may grant.
func checkSubagentRequest(sr *spiceboxv1alpha1.SubagentRequest, ws *spiceboxv1alpha1.Workshop) string {
	if sr.Spec.Parent.Namespace != ws.Spec.Session.Namespace || sr.Spec.Parent.Name != ws.Spec.Session.Name {
		return fmt.Sprintf(
			"SubagentRequest.spec.parent must be the workshop's own session %s/%s, not %s/%s",
			ws.Spec.Session.Namespace, ws.Spec.Session.Name, sr.Spec.Parent.Namespace, sr.Spec.Parent.Name)
	}
	if !allowedWorkshopSubagentModes[sr.EffectiveMode()] {
		return fmt.Sprintf(
			"SubagentRequest.spec.mode %q is not permitted from a workshop; only %s, %s, or %s",
			sr.Spec.Mode, spiceboxv1alpha1.SubagentModeSingleTurn, spiceboxv1alpha1.SubagentModeTask,
			spiceboxv1alpha1.SubagentModeAttended)
	}
	return ""
}

// checkClusterToolObject enforces the CREATE-time content rule for the two
// cluster-scoped kinds a workshop may author: metadata.name is prefixed
// "<workshopID>-" (workshopID already carries the leading "ws-", so this
// reads e.g. "ws-abc123456789-mytool") AND metadata.labels carries
// LabelWorkshopNamespace equal to workshopID. This is ALSO the resolution
// step for the cluster-scoped case the brief's step 2 describes ("read
// LabelWorkshopNamespace off the OBJECT... mismatch -> Denied") — the same
// proof serves both purposes, since a toolspec/toolkit that fails this check
// is refused either way.
func checkClusterToolObject(meta metav1.ObjectMeta, workshopID string) string {
	wantPrefix := workshopID + "-"
	if !strings.HasPrefix(meta.Name, wantPrefix) {
		return fmt.Sprintf("metadata.name %q must be prefixed %q for this workshop", meta.Name, wantPrefix)
	}
	if meta.Labels[spiceboxv1alpha1.LabelWorkshopNamespace] != workshopID {
		return fmt.Sprintf(
			"metadata.labels[%s] must equal %q, the calling session's own workshop namespace",
			spiceboxv1alpha1.LabelWorkshopNamespace, workshopID)
	}
	return ""
}

// checkLimits enforces WorkshopLimits.MaxObjectsPerKind and MaxObjects on
// CREATE: a workshop's build space cannot be turned into an unbounded object
// mint. Counting happens against the CACHED manager client (client.Reader),
// so nine List calls per create is cheap, in-memory work, not nine live API
// round-trips.
func (w *Webhook) checkLimits(ctx context.Context, req admission.Request, ws *spiceboxv1alpha1.Workshop, workshopID string) admission.Response {
	counts := make(map[string]int, len(allWorkshopKinds))
	total := 0
	for _, k := range allWorkshopKinds {
		n, err := w.countKind(ctx, k, workshopID)
		if err != nil {
			return admission.Denied(fmt.Sprintf("could not count existing %s objects for the workshop object limits: %v", k, err))
		}
		counts[k] = n
		total += n
	}

	perKind := counts[req.Kind.Kind]
	if int32(perKind) >= ws.Spec.Limits.MaxObjectsPerKind {
		return admission.Denied(fmt.Sprintf(
			"workshop %s already has %d %s object(s), at its per-kind limit of %d",
			workshopID, perKind, req.Kind.Kind, ws.Spec.Limits.MaxObjectsPerKind))
	}
	if int32(total) >= ws.Spec.Limits.MaxObjects {
		return admission.Denied(fmt.Sprintf(
			"workshop %s already has %d object(s) across every kind, at its total limit of %d",
			workshopID, total, ws.Spec.Limits.MaxObjects))
	}
	return admission.Allowed("")
}

// countKind lists every existing object of kind belonging to workshopID: for
// the seven namespaced kinds, everything in the workshop namespace; for the
// two cluster-scoped tool kinds, everything carrying this workshop's
// LabelWorkshopNamespace (a cluster-scoped List has no namespace to scope by).
func (w *Webhook) countKind(ctx context.Context, kind, workshopID string) (int, error) {
	if isClusterScopedWorkshopKind(kind) {
		opt := client.MatchingLabels{spiceboxv1alpha1.LabelWorkshopNamespace: workshopID}
		switch kind {
		case "SpiceboxToolspec":
			var l spiceboxv1alpha1.SpiceboxToolspecList
			if err := w.client.List(ctx, &l, opt); err != nil {
				return 0, err
			}
			return len(l.Items), nil
		case "SpiceboxToolkit":
			var l spiceboxv1alpha1.SpiceboxToolkitList
			if err := w.client.List(ctx, &l, opt); err != nil {
				return 0, err
			}
			return len(l.Items), nil
		}
	}

	ns := client.InNamespace(workshopID)
	switch kind {
	case "AgentClass":
		var l spiceboxv1alpha1.AgentClassList
		if err := w.client.List(ctx, &l, ns); err != nil {
			return 0, err
		}
		return len(l.Items), nil
	case "MCPServer":
		var l spiceboxv1alpha1.MCPServerList
		if err := w.client.List(ctx, &l, ns); err != nil {
			return 0, err
		}
		return len(l.Items), nil
	case "SidecarToolbox":
		var l spiceboxv1alpha1.SidecarToolboxList
		if err := w.client.List(ctx, &l, ns); err != nil {
			return 0, err
		}
		return len(l.Items), nil
	case "AgentIdentity":
		var l spiceboxv1alpha1.AgentIdentityList
		if err := w.client.List(ctx, &l, ns); err != nil {
			return 0, err
		}
		return len(l.Items), nil
	case "Skill":
		var l spiceboxv1alpha1.SkillList
		if err := w.client.List(ctx, &l, ns); err != nil {
			return 0, err
		}
		return len(l.Items), nil
	case "AgentUI":
		var l spiceboxv1alpha1.AgentUIList
		if err := w.client.List(ctx, &l, ns); err != nil {
			return 0, err
		}
		return len(l.Items), nil
	case "SubagentRequest":
		var l spiceboxv1alpha1.SubagentRequestList
		if err := w.client.List(ctx, &l, ns); err != nil {
			return 0, err
		}
		return len(l.Items), nil
	}
	return 0, fmt.Errorf("unrecognized workshop-authored kind %q", kind)
}
