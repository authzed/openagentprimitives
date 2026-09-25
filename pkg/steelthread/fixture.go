package steelthread

import (
	"bytes"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/yaml"

	"github.com/authzed/openagentprimitives/pkg/agent/tool/sidecartoolbox"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	channelkindsregistry "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/credkind"
	credkindregistry "github.com/authzed/openagentprimitives/pkg/platform/identity/credkind/registry"

	// Blank-imported at LIBRARY level, not from a binary's main. RewriteFixture
	// must be able to emit a placeholder Secret for whichever credential type a
	// captured AgentIdentity happens to carry, unconditionally — the same
	// reasoning pkg/platform/identity/broker/inproc documents for blank-
	// importing this exact bundle itself (see AGENTS.md's "Identity credential
	// types are a deliberate exception to 'the blank import goes in the
	// binaries'"). The type set is closed by the CRD's Enum marker, so there is
	// no "install only the static kind" case a caller of RewriteFixture could
	// opt out of.
	_ "github.com/authzed/openagentprimitives/pkg/platform/identity/credkind/imports"
)

// Fixed rewrite values. Every one is a constant, never derived from the live
// input, so re-running RewriteFixture over the SAME session produces
// byte-identical output — a capture that varied would make its own diff
// unreadable, the failure mode BRONZE_UPDATE_GOLDEN goldens exist to avoid.
const (
	// fixtureNamespace replaces every live namespace: envtest does not
	// auto-create namespaces, so every emitted object must agree on one that
	// the harness applies into.
	fixtureNamespace = "default"

	// fixtureModelProvider and fixtureModelName replace the live model: only
	// the e2e harness's reconciler admits provider=test, and a captured
	// session must never re-invoke a real model on replay.
	fixtureModelProvider = "test"
	fixtureModelName     = "scripted"

	// mcpURLSentinel replaces every live MCPServer endpoint. The harness
	// substitutes it for the httptest server's own URL before apply, so the CR
	// stays diffable in git while still addressing a live in-process server
	// during the run.
	mcpURLSentinel = "{{MCP_URL}}"

	// fakeChannelKind is what every Channel other than the trigger's own input
	// Channel is rewritten to.
	fakeChannelKind = "fake"

	// genericSecretKey names the single data key a placeholder Secret gets
	// when nothing more specific is known — either because the referencing
	// object's kind needs no particular key (fake), or because the kind isn't
	// registered in this binary (RewriteFixture asks the channelkinds registry
	// rather than hardcoding a kind's key list; an unregistered kind answers
	// the same way an unknown one does everywhere else in this codebase: a
	// safe, generic fallback, not an error).
	genericSecretKey = "placeholder"

	// genericPlaceholderPrefix begins every substituted credential value that
	// has no declared shape to satisfy. A named constant rather than a repeated
	// format string because the structural credential scan has to RECOGNIZE
	// what this rewrite writes — a value substituted here that the scan does
	// not know is a substitution refuses the capture as though it were a live
	// credential. See placeholderMarkers.
	genericPlaceholderPrefix = "fixture-placeholder-"

	// unusedByTestProviderValue and unusedByFakeChannelValue match the wording
	// the hand-authored fixtures already use (test/e2e/testdata/
	// agent-centerdot-companies/00-secret.yaml): both are literally true here,
	// because provider=test never reads the model API key and the fake kind
	// never reads Channel credentials — only the CRD's "field must be set"
	// validation needs either Secret to exist at all.
	unusedByTestProviderValue = "unused-by-the-test-provider"
	unusedByFakeChannelValue  = "unused-by-the-fake-channel"
)

// FixtureInput is one live agent's manifests, gathered by an earlier capture
// step: the AgentClass, its referenced MCPServers, its AgentIdentity, and
// every Channel bound to it. RewriteFixture never sees a live Secret's VALUE —
// only these CRs' references to one — matching pkg/platform/oap/source's
// cluster exporter, which carries the same guarantee for the same reason: a
// bug that copied a live credential forward would be committed to a repo.
type FixtureInput struct {
	Class      *spiceboxv1alpha1.AgentClass
	MCPServers []*spiceboxv1alpha1.MCPServer
	Identity   *spiceboxv1alpha1.AgentIdentity
	Channels   []*spiceboxv1alpha1.Channel

	// SidecarToolboxes are the session's RESOLVED sidecar toolboxes, read from
	// AgentSession.status.resolvedSidecarToolboxes — NOT from the class's
	// spec.sidecarToolboxes, and NOT from the live SidecarToolbox CRs the class
	// refers to.
	//
	// The status is the only input that says what the runner ACTUALLY ran. Each
	// entry carries `Spec`, a snapshot the operator took at session start, so a
	// SidecarToolbox CR edited after the session began still yields the tool
	// allowlist the transcript was produced against. The class spec names only a
	// ref; following it reads today's CR, which is the same class of mistake
	// PlanGateActive taught — the spec value can be stale or unclamped, and the
	// resolved status is what the run consulted.
	//
	// Both halves of the capture read this: declaredTools takes each entry's
	// `Name` as an LLM-facing prefix (matching sidecartoolbox.Synthesize, which
	// builds its synthetic MCPServer with ObjectMeta.Name = rt.Name) and its
	// `Spec.Tools[].Name` as the server-side names, and rewriteSidecarToolboxes
	// emits the CR the replayed AgentClass's own ref validation requires.
	SidecarToolboxes []spiceboxv1alpha1.ResolvedSidecarToolbox

	// SandboxClasses, Toolspecs and Toolkits are the three CRs a SANDBOX tool is
	// synthesized from, gathered by following the class's spec.toolBundles.
	//
	// All three are required for a captured sandbox tool to EXIST at replay, and
	// each supplies a different half of it:
	//
	//   - SpiceboxClass carries spec.tools, the catalog naming each tool and the
	//     argv it runs. The tool's LLM-facing name is "<bundle>_<class tool>",
	//     and the driver's fake exec responder dispatches on that argv.
	//   - SpiceboxToolspec carries the narrowing — allowSubcommands, constraints,
	//     writesRelationships — which is what the authorization behavior a
	//     captured session exercised actually comes from.
	//   - SpiceboxToolkit carries the permission model per subcommand. It is not
	//     optional garnish: synthesis TOLERATES a missing toolkit and falls back
	//     to a passthrough permission, so a fixture without it replays the same
	//     calls through a different gate, silently.
	//
	// Empty for a session with no toolBundles, which is every MCP-only capture.
	SandboxClasses []*spiceboxv1alpha1.SpiceboxClass
	Toolspecs      []*spiceboxv1alpha1.SpiceboxToolspec
	Toolkits       []*spiceboxv1alpha1.SpiceboxToolkit

	// SessionUserIdentity is the session's own projection of the STARTER's
	// credential catalog, present exactly when the session resolved to
	// identityMode userPassthrough. Nil for every agent-identity session, which
	// is what a fixture with no UserIdentity in it means.
	//
	// The per-SESSION projection, not the live user's UserIdentity, for the
	// same reason SidecarToolboxes reads the resolved status: the catalog holds
	// whatever that person has ever linked, while this holds what THIS run
	// resolved. See rewriteUserIdentity.
	SessionUserIdentity *spiceboxv1alpha1.SessionUserIdentity

	// Skills and SkillSources are the CRs behind the class's spec.skills, and
	// they are gathered and emitted as a PAIR because neither is sufficient
	// alone — see rewriteSkills for the provenance gate that makes the pairing
	// mandatory rather than tidy.
	//
	// Read from the LIVE CRs rather than from any session status, and that is
	// the opposite choice to SidecarToolboxes above. There is no per-session
	// snapshot of a skill to prefer: AgentSession.status.resolvedSkillBundles
	// records only which bundles were STAGED (a digest, a mount name, a
	// ConfigMap name) and carries none of the spec a replayed Skill has to
	// present — no canonical name to match the class's ref against, no body, no
	// frontmatter name the staging path checks. The CR is the only place that
	// content exists.
	//
	// Empty for every class that opts into no skill, which is every capture
	// taken before this and all but one AgentClass on the cluster it was
	// developed against.
	Skills       []*spiceboxv1alpha1.Skill
	SkillSources []*spiceboxv1alpha1.SkillSource

	// TriggerChannel is the metadata.name of the input Channel a triggered
	// session opened on, or empty for a session a person typed into.
	//
	// THE one exception to every other rewrite here: bt.Trigger's contract is
	// that the Channel's OWN spec.kind selects the delivery mechanics, and the
	// driver signs the payload with that Channel's own credentials Secret, so
	// HMAC verification, event filtering and channel-key derivation all run
	// for real. Rewriting this Channel to fake would delete the only thing a
	// trigger bundle exists to exercise. Every OTHER Channel — conversational
	// and output alike — becomes kind=fake.
	TriggerChannel string
}

// FixtureFile is one numbered YAML file RewriteFixture emits, named to match
// the convention the hand-authored fixtures already use
// (test/e2e/testdata/agent-centerdot-companies): 00-secret.yaml,
// 01-identity.yaml, 02-mcpserver.yaml, 03-agent.yaml.
type FixtureFile struct {
	Name string
	YAML []byte
}

// RewriteResult is one fixture rewrite's output: the files, and what the
// rewrite CHANGED about the session that the bundle has to know.
type RewriteResult struct {
	// Files are the numbered YAML manifests, in apply order.
	Files []FixtureFile

	// UngatedTools are the LLM-facing names of tools this rewrite made
	// reachable from the FIRST turn, by dropping a gate no fixture can satisfy.
	//
	// Today that is exactly one rewrite: rewriteSidecarToolboxes dropping
	// spec.secretInputs, which in a live session holds a sidecar's tools back
	// until a producer tool publishes the gating Secret. A fixture has no
	// producer, so the gate could never open and the sidecar would answer
	// nothing; dropping it is what makes those tools exist at all, and the
	// price is that they exist from turn zero rather than from the turn they
	// really arrived.
	//
	// The replay needs this to tell that difference — which IT introduced —
	// apart from a gate that genuinely stopped withholding, which is a
	// regression. It is REPORTED here rather than restated anywhere else
	// precisely so the two cannot disagree: the function that drops the gate is
	// the function that names what dropping it let through, and a rewrite that
	// stops ungating something stops excusing it in the same commit.
	//
	// Sorted, so a re-capture of one session is byte-identical.
	UngatedTools []string

	// ShadowedSecrets are the metadata.names of emitted placeholder Secrets
	// whose NAME was taken from a live reference — so each one stands in for
	// real credential material that exists in the cluster the capture read.
	//
	// This is what makes the leak scan's gate precise instead of coarse. The
	// gate used to ask "does the fixture emit ANY Secret?", which reads a
	// wholly INVENTED placeholder — the one rewriteClass mints when the class
	// declares no apiKey at all, because its credential is resolved by the
	// settings tiers — as proof that live material existed to be read. It does
	// not: no live Secret by that name exists, there is no value to scan for,
	// and the capture was refused for failing to supply one that cannot be had.
	//
	// A name listed here is a different claim, and a checkable one: this Secret
	// stands in for a live one, so the caller must have read that live one's
	// value and passed it in SelfCheckInput.LiveSecrets. Reported by the
	// rewrite rather than re-derived by the checker, for the reason
	// UngatedTools gives — the function that substitutes the value is the
	// function that says whose value it substituted.
	//
	// Sorted and deduplicated, so a re-capture of one session is byte-identical.
	ShadowedSecrets []string

	// EmittedSkills are the canonical names of the Skills this rewrite actually
	// wrote, sorted and deduplicated.
	//
	// Reported by the rewrite rather than re-derived by the checker, for the
	// reason UngatedTools and ShadowedSecrets are: the class's spec.skills says
	// what the session opted INTO, and only this function knows which of those
	// a Skill CR was found and emitted for. A checker that re-derived the answer
	// from the class would say every skill was captured the moment the class
	// named it. See CodeSkillsNotCaptured.
	EmittedSkills []string

	// SkillsWithoutBundle are the canonical names whose spec.bundle ref this
	// rewrite dropped — the skills whose scripts and assets a replay will not
	// stage. See rewriteSkills and CodeSkillBundleNotStaged.
	SkillsWithoutBundle []string

	// MintedCredentials are the emitted credentials whose credkind reports
	// Minted() — one entry per credential, spelled "<identity>/<credential>
	// (<type>)".
	//
	// A MINTED credential is not read from a Secret; each resolve calls an
	// external minter for a fresh short-lived token, and the Deps field that
	// minter arrives in is nil wherever nobody configured one. A fixture can
	// substitute a stored value, which is what every placeholder Secret here
	// does; it cannot substitute a minter. So the placeholder this rewrite
	// emits alongside a minted credential satisfies the AgentIdentity's own
	// Secret-exists check and satisfies nothing at resolve time.
	//
	// Reported by the rewrite rather than re-derived by the checker, for the
	// reason UngatedTools and ShadowedSecrets are: the function that emits a
	// credential is the function that says what emitting it could not carry.
	// Sorted and deduplicated, so a re-capture of one session is
	// byte-identical. See CodeCredentialNotMintable.
	//
	// It reports every minted credential, INCLUDING the types a replay harness
	// stands a minter up for. Whether the harness can serve one is a separate
	// fact, held in bt.StandInMintedCredentialTypes and applied by the check;
	// filtering here would fold the two together and leave the rewrite unable
	// to say what it actually emitted.
	MintedCredentials []MintedCredential

	// DefaultUser is the channel identity the replay must send as, set exactly
	// when this rewrite emitted a UserIdentity for a userPassthrough session.
	// Empty otherwise, which leaves the harness's own default in place.
	//
	// Stated rather than assumed, for the reason UngatedTools and
	// ShadowedSecrets are: the emitted UserIdentity is NAMED after this user's
	// canonical subject, so a bundle sending as anybody else would resolve no
	// catalog and park in AwaitingCredentials with nothing pointing back here.
	// The function that derives the name is the function that says who it
	// derived it for.
	DefaultUser string
}

// fixtureSecret is one placeholder Secret a rewrite emits, together with the
// single fact the leak scan needs about it: whether its NAME came from a LIVE
// reference. See RewriteResult.ShadowedSecrets.
type fixtureSecret struct {
	Secret      *corev1.Secret
	ShadowsLive bool
}

// RewriteFixture turns one live agent's manifests into a bootable envtest
// fixture: a fixed rewrite table (model provider/name, MCPServer url,
// namespace, server-side metadata) plus fresh placeholder Secrets carrying
// FIXED dummy values, never the live ones. See FixtureInput.TriggerChannel
// for the one field this table does not touch.
//
// Every emitted object's metadata is rebuilt from just its name and
// fixtureNamespace — never a live object's ObjectMeta copied forward. That is
// deliberate, not merely a side effect of the fields this function happens to
// rewrite: a live CR can carry arbitrary annotations and labels an operator
// added for their own reasons (a debug note, an internal tracking id), and
// none of the rewrite rules below say anything about them. Preserving them
// anyway would mean whatever a live object accumulated over its lifetime
// rides into a file this function writes into a repo. Rebuilding metadata
// from an explicit allowlist (name + namespace, nothing else) is what makes
// TestRewriteFixture_NoLiveSecretMaterialSurvives true by construction rather
// than by remembering to strip the right field every time a new one appears.
//
// It returns what it CHANGED as well as what it wrote — see
// RewriteResult.UngatedTools. A rewrite that removes a gate alters what the
// replayed run can reach, and the bundle has to carry that fact from the
// function that performed the rewrite rather than restate it somewhere else.
func RewriteFixture(in FixtureInput) (RewriteResult, error) {
	if in.Class == nil {
		return RewriteResult{}, fmt.Errorf("steelthread: RewriteFixture: FixtureInput.Class is required")
	}
	if in.TriggerChannel != "" && !hasChannelNamed(in.Channels, in.TriggerChannel) {
		return RewriteResult{}, fmt.Errorf("steelthread: RewriteFixture: TriggerChannel %q names no Channel in FixtureInput.Channels", in.TriggerChannel)
	}

	var files []FixtureFile
	// What a rewrite let through, gathered as the rewrites run. See
	// RewriteResult.UngatedTools.
	var ungated []string

	miscSecrets, class, err := rewriteClass(in.Class)
	if err != nil {
		return RewriteResult{}, err
	}

	channelSecrets, channels, err := rewriteChannels(in.Channels, in.TriggerChannel)
	if err != nil {
		return RewriteResult{}, err
	}
	miscSecrets = append(miscSecrets, channelSecrets...)

	if len(miscSecrets) > 0 {
		doc, err := marshalDocs(secretDocs(miscSecrets))
		if err != nil {
			return RewriteResult{}, err
		}
		files = append(files, FixtureFile{Name: "00-secret.yaml", YAML: doc})
	}

	// Every emitted Secret, from every rewriter, so ShadowedSecrets is derived
	// from the same values secretDocs marshals rather than recomputed from the
	// inputs a second time.
	allSecrets := slices.Clone(miscSecrets)

	// The starter's own catalog, for a userPassthrough session. Its Secrets are
	// deliberately NOT in allSecrets: every name it emits is re-derived from
	// the fixture user, so none of them shadows a live one and none is a value
	// the leak scan's gate could require somebody to have read.
	// Gathered as the rewrites run, from every rewriter that emits a
	// credential. See RewriteResult.MintedCredentials.
	var minted []MintedCredential

	var defaultUser string
	if in.SessionUserIdentity != nil {
		userFile, userMinted, err := rewriteUserIdentity(in.SessionUserIdentity)
		if err != nil {
			return RewriteResult{}, err
		}
		minted = append(minted, userMinted...)
		defaultUser = fixtureUserEmail
		files = append(files, userFile)
	}

	if in.Identity != nil {
		identitySecrets, identity, identityMinted, err := rewriteIdentity(in.Identity)
		if err != nil {
			return RewriteResult{}, err
		}
		minted = append(minted, identityMinted...)
		allSecrets = append(allSecrets, identitySecrets...)
		docs := secretDocs(identitySecrets)
		docs = append(docs, identity)
		doc, err := marshalDocs(docs)
		if err != nil {
			return RewriteResult{}, err
		}
		files = append(files, FixtureFile{Name: "01-identity.yaml", YAML: doc})
	}

	if len(in.MCPServers) > 0 {
		mcpServers, err := rewriteMCPServers(in.MCPServers)
		if err != nil {
			return RewriteResult{}, err
		}
		doc, err := marshalDocs(mcpServers)
		if err != nil {
			return RewriteResult{}, err
		}
		files = append(files, FixtureFile{Name: "02-mcpserver.yaml", YAML: doc})
	}

	if len(in.SidecarToolboxes) > 0 {
		toolboxes, sidecarUngated, err := rewriteSidecarToolboxes(in.SidecarToolboxes)
		if err != nil {
			return RewriteResult{}, err
		}
		ungated = append(ungated, sidecarUngated...)
		doc, err := marshalDocs(toolboxes)
		if err != nil {
			return RewriteResult{}, err
		}
		// 02a, not 04: the AgentClass in 03-agent.yaml validates every
		// sidecarToolboxes[].ref against an existing SidecarToolbox, and the
		// harness applies a directory's *.yaml in sorted order. Emitting this
		// after the class would apply the class against a ref that does not
		// resolve yet. The suffix keeps 03-agent.yaml's name stable rather than
		// renumbering every file after it.
		files = append(files, FixtureFile{Name: "02a-sidecartoolbox.yaml", YAML: doc})
	}

	// The three sandbox CRs, in the order they reference each other: a toolspec
	// names a toolkit, a class names its toolspecs. Numbered 02b/02c/02d for the
	// reason 02a is: everything the AgentClass in 03-agent.yaml refers to has to
	// be applied before it.
	sandboxFiles, err := rewriteSandboxDocs(in)
	if err != nil {
		return RewriteResult{}, err
	}
	files = append(files, sandboxFiles...)

	// 02e for the reason 02a is: the AgentClass in 03-agent.yaml refuses to go
	// Valid=True until every skills[].ref resolves to a Skill that is itself
	// Valid, and the harness applies a directory's *.yaml in sorted order.
	skillDocs, emittedSkills, droppedSkillBundles, err := rewriteSkills(in.Skills, in.SkillSources)
	if err != nil {
		return RewriteResult{}, err
	}
	if len(skillDocs) > 0 {
		doc, err := marshalDocs(skillDocs)
		if err != nil {
			return RewriteResult{}, err
		}
		files = append(files, FixtureFile{Name: "02e-skill.yaml", YAML: doc})
	}

	agentDocs := []any{class}
	for _, ch := range channels {
		agentDocs = append(agentDocs, ch)
	}
	doc, err := marshalDocs(agentDocs)
	if err != nil {
		return RewriteResult{}, err
	}
	files = append(files, FixtureFile{Name: "03-agent.yaml", YAML: doc})

	sort.Strings(ungated)
	slices.SortFunc(minted, func(a, b MintedCredential) int { return strings.Compare(a.Label, b.Label) })
	return RewriteResult{
		Files:               files,
		UngatedTools:        ungated,
		ShadowedSecrets:     shadowedNames(allSecrets),
		DefaultUser:         defaultUser,
		EmittedSkills:       emittedSkills,
		SkillsWithoutBundle: droppedSkillBundles,
		MintedCredentials:   slices.Compact(minted),
	}, nil
}

// ReplayChannel returns the Channel CR and the credentials Secret RewriteFixture
// will emit for the live Channel named name.
//
// Exported because a caller has to know what the REPLAY's channel is BEFORE the
// capture runs: the meta tools a session is offered are assembled against the
// bound Channel's KIND, and that kind is exactly what this rewrite replaces. A
// caller that wants to know which of those tools survive has to assemble a
// second time against this, and cannot do it from the emitted YAML.
//
// It runs the same rewriteChannels the emission runs rather than restating its
// rule, so what a caller assembles against and what lands in 03-agent.yaml
// cannot disagree — the failure that would otherwise be invisible is a check
// that passes against a channel the fixture never writes.
func ReplayChannel(in FixtureInput, name string) (*spiceboxv1alpha1.Channel, *corev1.Secret, error) {
	secrets, channels, err := rewriteChannels(in.Channels, in.TriggerChannel)
	if err != nil {
		return nil, nil, err
	}
	// rewriteChannels appends to both slices in one loop, so index i of each
	// describes the same Channel.
	for i, ch := range channels {
		if ch.Name == name {
			return ch, secrets[i].Secret, nil
		}
	}
	return nil, nil, fmt.Errorf("steelthread: ReplayChannel: no Channel named %q in FixtureInput.Channels", name)
}

func hasChannelNamed(channels []*spiceboxv1alpha1.Channel, name string) bool {
	for _, ch := range channels {
		if ch != nil && ch.Name == name {
			return true
		}
	}
	return false
}

// rewriteClass rewrites the AgentClass's model to the fixed test provider and
// returns the placeholder Secret its apiKey ref now needs.
//
// The returned Secret SHADOWS a live one only when the class named one. A class
// whose spec.model is absent entirely — every class whose model and credential
// come from the settings tiers — gets a name this function INVENTS, which
// stands in for nothing that exists. See RewriteResult.ShadowedSecrets.
//
// # A DERIVED interact permission is written into the emitted SPEC
//
// A class with a userless input Channel (a webhook: nobody typed) is refused
// outright unless its EFFECTIVE session interact permission is non-empty —
// declared on the spec, or derived by its own reconciler onto
// status.derivedSessionInteractPermission from the single role=output Channel's
// membership (a Slack channel's members, say). Most trigger-opened classes take
// the derived route and declare nothing.
//
// rewriteChannels turns every non-trigger Channel into kind=fake, which is
// correct and is also exactly the input that derivation reads: a fake Channel
// names no membership, so the replayed reconciler derives nothing and parks the
// class at Valid=False/UserLessChannelMissingAuthz — at the readiness barrier,
// having exercised nothing. So the value the live reconciler already computed
// is carried onto the emitted spec, where the rewrite cannot destroy it.
//
// This is a carry-forward, not an invention, and the difference is the whole
// justification. EffectiveSessionInteractPermission prefers the declared value
// and falls back to the derived one, so the replayed class enforces the SAME
// subject-set string the captured session ran under; every consumer reads it
// through that accessor and none reads the derived field directly. Writing a
// permissive value instead — or dropping the requirement — would be a
// weakening, and this is neither: with the fixture's Slack channel gone, the
// subject-set matches nobody, which is stricter than the live cluster, not
// looser. It is the fourth time on this branch that the answer was "read the
// resolved status, not the spec".
func rewriteClass(live *spiceboxv1alpha1.AgentClass) ([]fixtureSecret, *spiceboxv1alpha1.AgentClass, error) {
	spec := live.Spec.DeepCopy()

	if spec.GetAuthz().GetSession().InteractPermission == "" && live.Status.DerivedSessionInteractPermission != "" {
		if spec.Authz == nil {
			spec.Authz = &spiceboxv1alpha1.AuthzBlock{}
		}
		if spec.Authz.Session == nil {
			spec.Authz.Session = &spiceboxv1alpha1.SessionAuthz{}
		}
		spec.Authz.Session.InteractPermission = live.Status.DerivedSessionInteractPermission
	}

	model := spec.Model
	if model == nil {
		model = &spiceboxv1alpha1.ModelConfig{}
		spec.Model = model
	}
	model.Provider = fixtureModelProvider
	model.Name = fixtureModelName
	model.FromCatalog = "" // provider/name above are the inline form; catalog and inline are mutually exclusive.

	shadowsLive := model.APIKey.Name != ""
	if model.APIKey.Name == "" {
		model.APIKey.Name = live.Name + "-placeholder"
	}
	if model.APIKey.Key == "" {
		model.APIKey.Key = "api-key"
	}

	out := &spiceboxv1alpha1.AgentClass{
		TypeMeta:   typeMeta("AgentClass"),
		ObjectMeta: fixtureMeta(live.Name),
		Spec:       *spec,
	}

	secret := newSecret(model.APIKey.Name, map[string]string{model.APIKey.Key: unusedByTestProviderValue})
	return []fixtureSecret{{Secret: secret, ShadowsLive: shadowsLive}}, out, nil
}

// rewriteChannels applies the one exception (FixtureInput.TriggerChannel
// keeps its real kind) plus the blanket rule (every other Channel becomes
// kind=fake), and returns the placeholder credentials Secret each needs.
//
// Channels are processed in NAME order rather than FixtureInput's given
// order: a caller that gathered them off a List() call has no ordering
// guarantee, and RewriteFixture's own determinism promise must not depend on
// one.
func rewriteChannels(live []*spiceboxv1alpha1.Channel, triggerChannel string) ([]fixtureSecret, []*spiceboxv1alpha1.Channel, error) {
	sorted := slices.Clone(live)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })

	var secrets []fixtureSecret
	var channels []*spiceboxv1alpha1.Channel
	for _, ch := range sorted {
		spec := ch.Spec.DeepCopy()
		isTrigger := ch.Name == triggerChannel

		var secretData map[string]string
		if isTrigger {
			// Kind, and every kind-specific config block, ride through
			// unchanged: the driver signs with THIS Channel's own kind and
			// credentials, so nothing about how it delivers may be rewritten.
			keys := requiredSecretKeysFor(spec.Kind, ch)
			secretData = make(map[string]string, len(keys))
			for _, k := range keys {
				secretData[k] = genericPlaceholderPrefix + k
			}
		} else {
			spec.Kind = fakeChannelKind
			spec.Slack = nil
			spec.GitHub = nil
			spec.Bento = nil
			spec.Fake = &spiceboxv1alpha1.FakeChannelConfig{Echo: false}
			secretData = map[string]string{genericSecretKey: unusedByFakeChannelValue}
		}

		out := &spiceboxv1alpha1.Channel{
			TypeMeta:   typeMeta("Channel"),
			ObjectMeta: fixtureMeta(ch.Name),
			Spec:       *spec,
		}
		channels = append(channels, out)

		secretName := spec.CredentialsRef.SecretName
		shadowsLive := secretName != ""
		if secretName == "" {
			secretName = ch.Name + "-creds"
			out.Spec.CredentialsRef.SecretName = secretName
		}
		secrets = append(secrets, fixtureSecret{
			Secret:      newSecret(secretName, secretData),
			ShadowsLive: shadowsLive,
		})
	}
	return secrets, channels, nil
}

// requiredSecretKeysFor asks the channelkinds registry which Secret data
// keys kindName's Channel.RequiredSecretKeys declares, so a REGISTERED kind
// (github, slack, ...) gets its real key names — "put it under the same key
// the live Secret used so the kind reads it unchanged" — without this
// package ever branching on a kind by name.
//
// An unregistered kind (this package links no channel kind, and a test may
// use a made-up one entirely) answers the same way every other
// registry-backed question about an unlinked kind does elsewhere in this
// codebase (channelkindsregistry.IsBrowserSurface, .NeedsWebhook): a safe
// generic fallback, not an error.
func requiredSecretKeysFor(kindName string, ch *spiceboxv1alpha1.Channel) []string {
	k, ok := channelkindsregistry.Get(kindName)
	if !ok {
		return []string{genericSecretKey}
	}
	keys := k.RequiredSecretKeys(ch)
	if len(keys) == 0 {
		return []string{genericSecretKey}
	}
	return keys
}

// MintedCredential is one emitted credential whose value is minted per resolve
// rather than read from a Secret.
//
// Type is carried beside the label because the two answer different questions.
// The label is for a human reading a finding; the TYPE is what decides whether
// a replay harness can serve it at all (bt.StandInMintedCredentialTypes), and
// re-deriving that by parsing the label would make a finding's prose
// load-bearing.
type MintedCredential struct {
	// Label names the credential for a reader: "<identity>/<credential>
	// (<type>)".
	Label string
	// Type is the credkind type name, exactly as the registry spells it.
	Type string
}

// mintedCredentialFor describes one emitted credential for
// RewriteResult.MintedCredentials, or returns the zero value when the type
// stores its value rather than minting it.
//
// Asked of the registered Kind, never of cred.Type: which types mint is the
// credkind registry's own answer (credkind.Kind.Minted), so a fifth type that
// mints is caught here the day it is registered, with nothing in this package
// edited. See RewriteResult.MintedCredentials.
func mintedCredentialFor(k credkind.Kind, owner string, cred spiceboxv1alpha1.AgentCredential) MintedCredential {
	if !k.Minted() {
		return MintedCredential{}
	}
	return MintedCredential{
		Label: fmt.Sprintf("%s/%s (%s)", owner, cred.Name, k.Type()),
		Type:  k.Type(),
	}
}

// rewriteIdentity deep-copies the AgentIdentity's spec (credential names and
// secret refs are kept — they are references, not secret material) and
// returns one placeholder Secret per credential, dispatched through the
// credkind registry rather than switched on cred.Type.
//
// The third return is the credentials a placeholder cannot stand in for; see
// RewriteResult.MintedCredentials.
func rewriteIdentity(live *spiceboxv1alpha1.AgentIdentity) ([]fixtureSecret, *spiceboxv1alpha1.AgentIdentity, []MintedCredential, error) {
	spec := live.Spec.DeepCopy()

	var secrets []fixtureSecret
	var minted []MintedCredential
	for _, cred := range spec.Credentials {
		k, err := credkindregistry.Get(cred.Type)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("steelthread: RewriteFixture: AgentIdentity %q credential %q: %w", live.Name, cred.Name, err)
		}
		if mc := mintedCredentialFor(k, live.Name, cred); mc.Label != "" {
			minted = append(minted, mc)
		}
		ref := k.SecretRef(cred)
		if ref == nil {
			// e.g. federated: no backing Secret to placeholder. AgentIdentity's
			// own XValidation already rejects type=federated, but this function
			// stays defensive rather than assuming that rule never relaxes.
			continue
		}
		keys := k.RequiredSecretKeys(cred)
		if len(keys) == 0 {
			keys = []string{genericSecretKey}
		}
		data := make(map[string]string, len(keys))
		for _, key := range keys {
			// Shape-aware, because the AgentIdentity reconciler validates a
			// stored value against the token format its provider declares —
			// see placeholderCredentialValue.
			val, err := placeholderCredentialValue(cred.Name, key)
			if err != nil {
				return nil, nil, nil, err
			}
			data[key] = val
		}
		// Always shadows a live Secret: ref.Name comes from the credential's
		// own SecretRef, which the live AgentIdentity declared.
		secrets = append(secrets, fixtureSecret{Secret: newSecret(ref.Name, data), ShadowsLive: true})
	}

	out := &spiceboxv1alpha1.AgentIdentity{
		TypeMeta:   typeMeta("AgentIdentity"),
		ObjectMeta: fixtureMeta(live.Name),
		Spec:       *spec,
	}
	return secrets, out, minted, nil
}

// rewriteMCPServers deep-copies every MCPServer's spec and replaces its
// endpoint with the sentinel the harness substitutes before apply.
func rewriteMCPServers(live []*spiceboxv1alpha1.MCPServer) ([]any, error) {
	sorted := slices.Clone(live)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })

	docs := make([]any, 0, len(sorted))
	for _, m := range sorted {
		spec := m.Spec.DeepCopy()
		spec.Server.URL = mcpURLSentinel
		docs = append(docs, &spiceboxv1alpha1.MCPServer{
			TypeMeta:   typeMeta("MCPServer"),
			ObjectMeta: fixtureMeta(m.Name),
			Spec:       *spec,
		})
	}
	return docs, nil
}

// rewriteSidecarToolboxes emits one SidecarToolbox CR per resolved entry, so
// the replayed AgentClass's sidecarToolboxes[].ref validation resolves.
//
// The spec comes from the resolved entry's SNAPSHOT, not from the live CR: that
// snapshot is what the session actually ran against. See
// FixtureInput.SidecarToolboxes.
//
// # spec.secretInputs is DROPPED
//
// It is the same rewrite as replacing an MCPServer's real auth with a
// placeholder Secret, and for the same reason: at replay the sidecar IS the
// fake MCP stub, reached through SetSidecarProbeURL, and it needs no upstream
// credential to answer. What secretInputs does in production is hold the
// sidecar's tools back until a PRODUCER tool publishes the per-session
// secret-output Secret — a gate the operator enforces at pod-create and the
// in-process refresher enforces again. Carried into a fixture, that gate can
// never open: a bundle has no producer and no secret-output field, so the
// sidecar's tools are never synthesized and every call to one fails as an
// unknown tool.
//
// This is a FIXTURE REWRITE, not a runtime fabrication. The distinction is
// exactly the one every other rewrite here turns on: the driver is not made to
// manufacture a Secret behind a gate that is still declared — which would mask a
// regression in the gate — the fixture simply does not declare a gate it has no
// way to satisfy. The agent-side behavior replays faithfully because the
// transcript never depended on the credential, only on the tools the credential
// unlocked, and those are served by the stub either way.
//
// Nothing ELSE in the spec is rewritten, and the two fields that look like they
// should be are deliberately left alone:
//
//   - source.image names an image no fixture ever pulls. The harness creates no
//     sidecar pod at all — MarkSidecarReady stamps the resolved status directly
//     and SetSidecarProbeURL points both probe and dispatch at the MCP stub — so
//     the image is inert, and substituting one would be inventing a fact.
//   - transport.path rides through because sidecartoolbox.EndpointPath appends
//     it to the DISPATCH url, and the harness stub is an httptest.NewServer over
//     a bare http.HandlerFunc, which answers every path. Rewriting it would make
//     the fixture disagree with the run for no gain.
//
// sandbox.class is likewise not emitted as a SpiceboxClass: the AgentSession
// reconciler treats a NotFound SpiceboxClass as recoverable and falls back to
// the toolbox CR's own network values (controller.go's sidecar resolution),
// so the reference dangling is the tolerated case, not a broken fixture.
// The second return is the LLM-facing names of the tools dropping secretInputs
// let through — see RewriteResult.UngatedTools. Reported from HERE, beside the
// line that drops the gate, so the exemption a bundle carries can never outlive
// the rewrite that earned it. Derived through the real synthesizer's own naming
// (sidecartoolbox.LLMToolNames), never spelled out here: a hand-joined prefix
// would drift from what the runner actually offers, and the symptom of that
// drift is a tool the replay reports as an unexplained extra.
func rewriteSidecarToolboxes(live []spiceboxv1alpha1.ResolvedSidecarToolbox) ([]any, []string, error) {
	// By Ref, which is the emitted CR's metadata.name: a caller that gathered
	// these off a status slice has whatever order the operator wrote, and
	// RewriteFixture's determinism promise must not depend on one.
	sorted := slices.Clone(live)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Ref < sorted[j].Ref })

	docs := make([]any, 0, len(sorted))
	var ungated []string
	for _, rt := range sorted {
		if rt.Ref == "" {
			return nil, nil, fmt.Errorf("steelthread: RewriteFixture: resolved sidecar toolbox %q has no ref, "+
				"so the AgentClass's sidecarToolboxes[].ref cannot resolve to an emitted CR", rt.Name)
		}
		spec := rt.Spec.DeepCopy()
		// A toolbox that declared NO secretInputs was never gated, so nothing
		// about it changes and it excuses nothing. Only a real drop is
		// reported.
		if len(spec.SecretInputs) > 0 {
			ungated = append(ungated, sidecartoolbox.LLMToolNames(rt)...)
		}
		spec.SecretInputs = nil // see the doc comment above
		docs = append(docs, &spiceboxv1alpha1.SidecarToolbox{
			TypeMeta:   typeMeta("SidecarToolbox"),
			ObjectMeta: fixtureMeta(rt.Ref),
			Spec:       *spec,
		})
	}
	return docs, ungated, nil
}

func typeMeta(kind string) metav1.TypeMeta {
	return metav1.TypeMeta{APIVersion: spiceboxv1alpha1.SchemeGroupVersion.String(), Kind: kind}
}

// fixtureMeta is the ENTIRE allowlist of live ObjectMeta this package ever
// carries forward: name and fixtureNamespace. See RewriteFixture's doc
// comment for why nothing else — not annotations, not labels — rides
// through.
func fixtureMeta(name string) metav1.ObjectMeta {
	return metav1.ObjectMeta{Name: name, Namespace: fixtureNamespace}
}

func newSecret(name string, data map[string]string) *corev1.Secret {
	return &corev1.Secret{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: fixtureMeta(name),
		Type:       corev1.SecretTypeOpaque,
		StringData: data,
	}
}

func secretDocs(secrets []fixtureSecret) []any {
	docs := make([]any, 0, len(secrets))
	for _, s := range secrets {
		docs = append(docs, s.Secret)
	}
	return docs
}

// shadowedNames returns the names of the Secrets that stand in for live
// credential material, sorted and deduplicated. See
// RewriteResult.ShadowedSecrets.
func shadowedNames(secrets []fixtureSecret) []string {
	seen := map[string]bool{}
	for _, s := range secrets {
		if s.ShadowsLive {
			seen[s.Secret.Name] = true
		}
	}
	if len(seen) == 0 {
		return nil
	}
	return slices.Sorted(maps.Keys(seen))
}

// marshalDocs renders docs as a deterministic multi-document YAML stream,
// one "---\n"-separated document per entry.
//
// Each entry is round-tripped through unstructured first, purely to delete
// its "status" key. encoding/json's omitempty never omits a non-pointer
// struct field — every one of these types embeds Status by value — so a
// plain yaml.Marshal would render a literal "status: {}" on every object.
// That is not merely cosmetic: the rewrite table calls status STRIPPED, and
// a captured object never had a controller run against it, so "status: {}"
// claims an observation that never happened. The same unstructured-then-
// delete shape pkg/platform/oap/source.Sanitize uses for the same field.
func marshalDocs(docs []any) ([]byte, error) {
	var buf bytes.Buffer
	for i, d := range docs {
		if i > 0 {
			buf.WriteString("---\n")
		}
		m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(d)
		if err != nil {
			return nil, fmt.Errorf("steelthread: RewriteFixture: convert %T to unstructured: %w", d, err)
		}
		delete(m, "status")
		b, err := yaml.Marshal(m)
		if err != nil {
			return nil, fmt.Errorf("steelthread: RewriteFixture: marshal %T: %w", d, err)
		}
		buf.Write(b)
	}
	return buf.Bytes(), nil
}
