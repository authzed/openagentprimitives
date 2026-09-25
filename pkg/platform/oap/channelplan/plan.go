// The install-time half of this package: given a bundle, a cluster and a
// namespace, decide for each DECLARED channel what still needs doing and what
// is already known, so neither client has to work it out.
//
// It does no rendering and no prompting. `oap agent install` and admind's
// install endpoint answer the same questions — is this Channel already there?
// which answers can be filled in without asking? — and a second
// implementation of that would drift on exactly the seams that matter.
package channelplan

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/wizardkeys"
	"github.com/authzed/openagentprimitives/pkg/platform/oap"
	"github.com/authzed/openagentprimitives/pkg/x/externalurl"
)

// ChannelPlan is one declared channel's install-time verdict: whether it is
// already wired, and which of its wizard's answers this install can supply
// without asking.
type ChannelPlan struct {
	// Required is the declaration verbatim, including the role and the
	// operator-facing Purpose. The role is NOT a seeded answer — no kind's
	// wizard asks for one — it reaches the Channel from here when the caller
	// applies the manifests.
	Required oap.RequiredChannel

	// AlreadyWired is true only when a Channel of this name was SEEN in the
	// namespace AND it is OURS — same kind, same AgentClass. It is never true
	// on an offline plan, never true because a lookup failed, and never true
	// for a Channel that merely holds the name (see Conflict).
	//
	// A name match alone is deliberately NOT enough. A bundle upgrade that
	// changes a declared channel's kind, or an operator who hand-created a
	// Channel of that name for another agent, would otherwise get "already
	// wired", nothing created, every CR healthy — and an unreachable agent.
	// That is the same silent cross-binding RefuseNamedInstall exists to
	// prevent, reached without --name.
	AlreadyWired bool

	// Conflict is non-nil when a Channel of this name exists and is NOT ours.
	// AlreadyWired is false alongside it, deliberately: a consumer that reads
	// only AlreadyWired then tries to CREATE and is refused by the apiserver on
	// the name, which is loud. The opposite default would have it skip, which
	// is the silent cross-binding itself.
	//
	// The planner does not decide what to do about it — Task 4 refuses. This
	// only reports what is there.
	Conflict *ChannelConflict

	// WiringKnown says whether AlreadyWired and Conflict are observations or
	// defaults. False means this plan never got to look — a nil client, an
	// empty namespace, or a declaration with no name to look one up by — so
	// AlreadyWired is false for want of an answer rather than because the
	// namespace was checked and found empty.
	//
	// AlreadyWired implies WiringKnown, and so does a non-nil Conflict; the
	// reverse does not hold. The four representable states are exactly: not
	// looked, absent, ours, and someone else's.
	//
	// A caller that is about to WRITE must read this: acting on
	// !AlreadyWired when !WiringKnown means creating a Channel that may
	// already exist. A caller that only PREVIEWS may treat it as "would
	// create" — which is the truth for a run that applies nothing. No such
	// caller exists yet; see PlanChannels on the offline arm.
	WiringKnown bool

	// Seeded is answer key → value for the questions this install can answer
	// on the operator's behalf. Never nil; empty when nothing is known.
	//
	// A key appears ONLY with a non-empty value. Seeding "" would answer a
	// question with nothing — the driver's seeded map suppresses a question by
	// presence, not by content — which is strictly worse than asking it.
	Seeded map[string]string

	// SeededFrom is answer key → where the value came from, in prose, for
	// EVERY key in Seeded and no others. Never nil.
	//
	// It is not decoration. Pre-seeding must not be silent: a value taken from
	// a ConfigMap the operator has never seen, applied without a word, is
	// impossible to debug when it is wrong. The clients print this.
	SeededFrom map[string]string

	// NotSeeded is answer key → why this install could not pre-fill it, for
	// the keys it TRIED to fill and could not. Never nil, and it never
	// overlaps Seeded or SeededFrom.
	//
	// It is a separate map rather than more entries in SeededFrom because
	// "consult a second map before you can interpret this one" is the join
	// shape this branch keeps producing defects from: a renderer that iterated
	// SeededFrom without first testing Seeded would print "external base URL
	// came from: could not read ConfigMap … forbidden", labelling a failure as
	// a provenance note. Two maps make that mistake shaped wrong instead of
	// merely wrong, and neither client has to be told the rule.
	//
	// This is where a failed read is REPORTED rather than swallowed. The plan
	// degrades — the question simply gets asked — so this prose is the whole
	// of what reaches the operator, and it distinguishes "there is no such
	// ConfigMap" from "I was not allowed to look", which send someone to fix
	// different things.
	//
	// A key nobody tried to fill — an agentClass the caller did not resolve —
	// appears in neither map: there is no failure to report, and prose about
	// it would be noise in every ordinary run.
	NotSeeded map[string]string
}

// ChannelConflict is a Channel occupying a declared name that this install
// cannot adopt, and what is different about it.
//
// The observed values are carried because the object was already in hand when
// the difference was found: re-fetching it to build a message would be a
// second read, and asking the operator to go and look is worse than telling
// them.
type ChannelConflict struct {
	// Kind and AgentClass are what the EXISTING Channel declares.
	Kind       string
	AgentClass string
	// Reason names exactly what differs, in prose a client can print without
	// re-deriving it. It is a description, not a verdict — the planner states
	// what it found and the caller decides.
	Reason string
}

// PlanChannels decides, for each channel the bundle declares, what install
// still has to do and what it can already answer — in DECLARATION ORDER,
// which is the order install processes them.
//
// c may be nil. That is an offline plan: every declaration is still planned,
// nothing is reported wired, and nothing is seeded from the cluster. It is
// deliberately not an error, and deliberately not the same as "checked and
// found nothing" — see ChannelPlan.WiringKnown.
//
// NO PRODUCTION CALLER TAKES THAT ARM TODAY, and it is worth saying so rather
// than leaving a reader to infer a mode that does not exist. `oap agent
// install` registers no --apply flag (only `oap channel create` does), and
// wireDeclaredChannels' own check() refuses a missing cluster connection
// BEFORE it plans; admind always has a client. The arm is exercised by tests,
// and it is kept because "no cluster" has to be answerable as an honest
// unknown rather than as a panic or a false "absent" — which is what
// WiringKnown is for.
//
// A TYPED-nil pointer is not a nil interface and will panic here, not take the
// offline path: a caller with no cluster must declare its variable as
// client.Client and leave it unassigned, never as a *someClient it forgot to
// set. This repo has had a production outage from exactly that.
//
// agentClass is the name the AgentClass CR will have IN THE CLUSTER, which the
// caller resolves and this function does not derive: install renames every
// bundled CR with an "<instanceName>-" prefix, so the bundle's own agent.name
// is the right answer only when no rename is in play. An empty agentClass
// seeds nothing rather than seeding "", and the wizard's shared AgentClass
// question then lists the namespace as it always did.
//
// instanceName is the install's --name value (install.InstallOpts.Name), empty
// when there is none. A NAMED INSTALL OF A CHANNEL-DECLARING BUNDLE IS REFUSED
// — see RefuseNamedInstall for the defect that refusal replaces, and for why
// it lives here rather than in each caller. It is tested with a bare `!= ""`,
// matching install's own gate on InstallOpts.Name exactly, so a `--name "  "`
// that install would honour as a prefix cannot slip past this refusal: two
// predicates for one fact is how the combination comes back.
//
// The three trailing strings are adjacent and same-typed, so ANY of their
// THREE pairwise transpositions compiles. Two are caught here: an agentClass
// in instanceName refuses any channel-declaring bundle outright, and a
// namespace in instanceName does the same. The THIRD — namespace and
// agentClass swapped — is NOT caught in this function: the lookup runs in a
// namespace that does not exist and honestly reports absent. It is caught
// downstream, by wizardkeys.AgentClassQuestion verifying the seeded class
// against its namespace's real listing, so no silent bad install results —
// but do not read this paragraph as a claim that the planner catches all
// three.
//
// It does NOT re-lint the declaration. LintRequiredChannels is the single
// verdict on whether a declaration is well-formed — an unregistered kind, an
// unusable name, a role the kind does not serve — and a planner that also
// ruled on it would be a second source of truth for the same fact. Callers
// lint first.
func PlanChannels(ctx context.Context, c client.Client, b *oap.Bundle, namespace, agentClass, instanceName string) ([]ChannelPlan, error) {
	if b == nil {
		return nil, fmt.Errorf("oap: PlanChannels: nil bundle")
	}
	if b.Manifest == nil {
		return nil, fmt.Errorf("oap: PlanChannels: bundle has no manifest")
	}
	declared := b.Manifest.Requires.Channels
	if len(declared) == 0 {
		// A bundle declaring no channel is unaffected by --name: there is no
		// unrenamed declaration to collide, so the refusal below must not fire.
		return nil, nil
	}
	// Before any cluster read: there is no point planning a combination that
	// cannot be installed.
	if err := RefuseNamedInstall(instanceName, declared); err != nil {
		return nil, err
	}

	// Read once for the whole plan rather than once per channel: it is one
	// cluster-wide fact, and N declarations must not become N identical Gets.
	externalBaseURL, noExternalBaseURL := readExternalBaseURL(ctx, c)

	plans := make([]ChannelPlan, 0, len(declared))
	for _, rc := range declared {
		p, err := planOne(ctx, c, rc, namespace, agentClass, externalBaseURL, noExternalBaseURL)
		if err != nil {
			return nil, err
		}
		plans = append(plans, p)
	}
	return plans, nil
}

// PlanOne is PlanChannels for a SINGLE declaration, for a caller that already
// knows which channel it is acting on and does not have the bundle in hand.
//
// admind's install form renders the whole bundle's channels in one response
// and then completes them ONE AT A TIME, on later requests that carry a token
// rather than the .oap file. Re-deriving that channel's verdict through this
// function rather than trusting what the earlier response said is what makes
// the collision check honest: a Channel of that name can have appeared, or
// been bound to another agent, in between the two requests.
//
// It re-reads the external base URL, which PlanChannels reads once for a whole
// bundle. One declaration is one read either way, so there is nothing to
// amortise here.
//
// It deliberately takes NO instanceName. RefuseNamedInstall is about a BUNDLE
// (a named install prefixes every bundled CR but not a declared channel name),
// and a caller that has already installed and is now wiring one channel has no
// bundle left to refuse — the refusal belongs at the install, which is where
// both callers make it.
func PlanOne(ctx context.Context, c client.Client, rc oap.RequiredChannel, namespace, agentClass string) (ChannelPlan, error) {
	externalBaseURL, noExternalBaseURL := readExternalBaseURL(ctx, c)
	return planOne(ctx, c, rc, namespace, agentClass, externalBaseURL, noExternalBaseURL)
}

// planOne is the body both entry points share: one declaration's seeds and one
// declaration's wiring verdict. The external base URL arrives as a parameter
// because PlanChannels reads it ONCE for a whole bundle — N declarations must
// not become N identical Gets.
func planOne(
	ctx context.Context,
	c client.Client,
	rc oap.RequiredChannel,
	namespace, agentClass, externalBaseURL, noExternalBaseURL string,
) (ChannelPlan, error) {
	// Trimmed ONCE, here, so the name that is seeded and the name that is
	// looked up cannot differ: `name: "demo-agent-gh "` used to seed the
	// trimmed spelling and search for the untrimmed one, which reports absent
	// for a Channel that is right there.
	name := strings.TrimSpace(rc.Name)

	p := ChannelPlan{
		Required:   rc,
		Seeded:     map[string]string{},
		SeededFrom: map[string]string{},
		NotSeeded:  map[string]string{},
	}
	// The bundle declares the name precisely so the operator cannot get it
	// wrong: other bundled CRs already reference "<name>-creds".
	p.seed(wizardkeys.KeyChannelName, name,
		"this bundle's requires.channels declaration")
	p.seed(wizardkeys.KeyAgentClass, agentClass,
		"the AgentClass this bundle installs")
	p.seed(wizardkeys.KeyExternalBaseURL, externalBaseURL, fmt.Sprintf(
		"ConfigMap %s/%s, key %q, published by webd",
		externalurl.Namespace, spiceboxv1alpha1.WebdExternalURLConfigMap, spiceboxv1alpha1.WebdTrustedURLKey))
	p.note(wizardkeys.KeyExternalBaseURL, noExternalBaseURL)

	wired, conflict, known, err := lookupWiring(ctx, c, strings.TrimSpace(namespace), name, rc, agentClass)
	if err != nil {
		return ChannelPlan{}, err
	}
	p.AlreadyWired, p.Conflict, p.WiringKnown = wired, conflict, known
	return p, nil
}

// RefuseNamedInstall reports that `--name` and a channel-declaring bundle
// cannot be combined, naming the flag value, every declared channel, and the
// route that does work. An empty name, or a bundle declaring no channel, is
// nothing to refuse.
//
// It is exported because PlanChannels is not the only moment the combination
// can be caught, and it is not the best one: a CLI that reaches this refusal
// only through the planner has already applied every CR by the time it fires,
// since the plan is made after the install lands. `oap agent install` calls
// this itself, before it writes anything. Both callers share ONE wording,
// which is the reason this is a function and not a rule each of them states.
//
// PlanChannels keeps its own call regardless: a caller that forgets this one
// must still not get a plan for a combination that cannot work.
//
// WHAT IT REPLACES, which is worse than an inconvenience. `instance.Rename`
// prefixes every bundled CR, but a declared channel is not a CR — a Channel is
// never bundled — so `requires.channels[].name` comes through untouched. Two
// named installs therefore declare the SAME channel name. The second one's
// plan finds the first one's Channel, reports AlreadyWired, and installs an
// agent wired to nothing: a Channel binds to exactly one AgentClass, so
// instance B's work is delivered to instance A. Nothing errors, nothing logs,
// and the operator sees two healthy installs. A github Channel's webhook path
// is per-channel, so one Channel cannot serve two agents even in principle —
// two agents need two URLs and therefore two Apps.
//
// WHY REFUSING RATHER THAN RENAMING. Prefixing the declared name alone would
// manufacture a different failure, not fix this one: the bundled
// AgentIdentity's credential reads "<declared-name>-creds", and Rename leaves
// that ref alone because the Secret is written by the channel wizard and is
// not in the bundle (see TestRenameLeavesAChannelCredsSecretRefAlone). Move
// one half without the other and the agent has no token. The correct fix
// prefixes BOTH halves atomically, which needs the "-creds" derivation in a
// third place or the kind registry inside `instance` — real design scope for a
// capability nobody has asked for yet. Until then a loud refusal beats a
// silent wrong outcome, which is the trade this repo makes everywhere else.
//
// WHY THE PLANNER STILL REFUSES TOO. Both `oap agent install` and admind's
// install endpoint call this planner, and one caller being missed is precisely
// how this class of defect survives — it has already happened on this branch.
// A caller that refuses EARLIER (the CLI does, before it writes anything) is
// strictly better for its operator; a caller that refuses only here still
// refuses. The wording is this function's either way, so the two can never
// disagree about what is wrong or about which routes work.
func RefuseNamedInstall(instanceName string, declared []oap.RequiredChannel) error {
	if instanceName == "" || len(declared) == 0 {
		return nil
	}
	names := make([]string, 0, len(declared))
	for _, rc := range declared {
		if rc.Name != "" {
			names = append(names, fmt.Sprintf("%q", rc.Name))
		}
	}
	which := "the channel(s) it declares"
	if len(names) > 0 {
		which = strings.Join(names, ", ")
	}
	// Exactly two routes, and both are things the operator can actually do.
	// A third — "keep --name here and wire the Channel by hand" — was written
	// and removed: the declared name is already held by the first instance's
	// Channel in this namespace, and the bundle's credentials read the Secret
	// derived from that same name, so there is no name a hand-created Channel
	// could take that both frees the collision and satisfies the credential.
	return fmt.Errorf(
		"oap: PlanChannels: --name %s cannot be combined with a bundle that declares channels (%s): "+
			"a named install prefixes every bundled resource but NOT a declared channel name, so a second "+
			"named install would find this same Channel already wired and bind to it — and a Channel serves "+
			"exactly one agent, so the second agent would receive nothing while the first answered its work. "+
			"Install without --name, or install this instance in its own namespace — where its Channel must "+
			`still keep the declared name, because the bundle's own credentials read the Secret derived `+
			"from it. To wire a Channel of a different name by hand, see \"oap channel create\", but the "+
			"bundle's AgentIdentity will not find its credentials under that name",
		instanceName, which)
}

// seed records key → value, and where the value came from, unless the value is
// empty. One helper rather than three copies of the same `if v != ""`, so the
// "no key without a value, no value without a source" invariant holds by
// construction instead of by three people remembering it.
func (p *ChannelPlan) seed(key, value, source string) {
	value = strings.TrimSpace(value)
	if value == "" {
		return
	}
	p.Seeded[key] = value
	p.SeededFrom[key] = source
	// A value supersedes any reason recorded for its absence. Today
	// readExternalBaseURL returns exactly one of the two so this cannot fire,
	// but a future seed-with-fallback would otherwise leave a key in both maps
	// and break B-R10's disjointness silently — the guarantee the split exists
	// to give, gone without a symptom.
	delete(p.NotSeeded, key)
}

// note records WHY a key this install tried to pre-fill was not seeded, so the
// failure reaches the operator as a sentence next to the question they are
// about to be asked instead of vanishing.
//
// Together with seed's own delete, the two are order-independent IN BOTH
// DIRECTIONS: a key that ends up with a value carries only its source, and a
// key that does not carries only the reason. An empty why is nothing to say —
// an offline plan attempted no read — and records nothing.
func (p *ChannelPlan) note(key, why string) {
	if why == "" {
		return
	}
	if _, seeded := p.Seeded[key]; seeded {
		return
	}
	p.NotSeeded[key] = why
}

// lookupWiring reports what the namespace holds for this declared channel:
// nothing, a Channel that is ours, or a Channel that merely holds the name.
//
// A NAME MATCH IS NOT ENOUGH (B-R11). The existing Channel's spec.kind and
// spec.agentClass are compared against what this install declares, because a
// Channel of the right name bound to another agent — or of another kind — is
// not one this install can adopt. Reporting it as wired would leave every CR
// healthy and the agent unreachable, which is the silent cross-binding
// RefuseNamedInstall argues at length to refuse; reaching the same outcome
// without a name prefix would make that refusal arbitrary.
//
// A NotFound is a real answer: the namespace was checked and there is nothing
// there. ANY OTHER ERROR IS NOT, and is returned rather than folded into
// "absent" — reporting a Channel we could not read as absent is what has
// install try to create one that may already exist.
//
// An empty namespace reads as UNKNOWN rather than absent. A namespaced Get
// with no namespace segment 404s against a real apiserver, so IsNotFound would
// otherwise fire and report an observation nobody made — the one path that
// escaped this tri-state before.
func lookupWiring(ctx context.Context, c client.Client, namespace, name string, rc oap.RequiredChannel, agentClass string) (wired bool, conflict *ChannelConflict, known bool, err error) {
	// A nameless declaration is an authoring error the lint reports; there is
	// nothing to look up, so the honest answer is "unknown", not "absent".
	if c == nil || namespace == "" || name == "" {
		return false, nil, false, nil
	}
	var existing spiceboxv1alpha1.Channel
	switch err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &existing); {
	case err == nil:
		if got := conflictWith(&existing, rc, agentClass); got != nil {
			return false, got, true, nil
		}
		return true, nil, true, nil
	case apierrors.IsNotFound(err):
		return false, nil, true, nil
	default:
		return false, nil, false, fmt.Errorf(
			"oap: PlanChannels: check for an existing Channel %q in namespace %q: %w", name, namespace, err)
	}
}

// conflictWith reports why an existing Channel of the declared name is not one
// this install can adopt, or nil when it is.
//
// AgentClass is compared only when the caller resolved one: an install that has
// not named its AgentClass cannot judge the binding, and inventing a mismatch
// from an empty string would report a conflict against every Channel in the
// namespace. The kind is always comparable, because the declaration carries it.
//
// An existing monitoring Channel binds to no agent, so its empty agentClass
// against a declared one IS a difference — it is not this install's Channel,
// and the message says what it found instead.
func conflictWith(existing *spiceboxv1alpha1.Channel, rc oap.RequiredChannel, agentClass string) *ChannelConflict {
	report := func(reason string) *ChannelConflict {
		return &ChannelConflict{
			Kind:       existing.Spec.Kind,
			AgentClass: existing.Spec.AgentClass,
			Reason:     reason,
		}
	}
	if rc.Kind != "" && existing.Spec.Kind != rc.Kind {
		return report(fmt.Sprintf(
			"a Channel named %q already exists in this namespace but is a %q channel, and this bundle declares a %q one",
			existing.Name, existing.Spec.Kind, rc.Kind))
	}
	if agentClass != "" && existing.Spec.AgentClass != agentClass {
		bound := fmt.Sprintf("agent %q", existing.Spec.AgentClass)
		if existing.Spec.AgentClass == "" {
			bound = "no agent (it is a monitoring channel)"
		}
		return report(fmt.Sprintf(
			"a Channel named %q already exists in this namespace but is bound to %s, and this install wants it bound to %q",
			existing.Name, bound, agentClass))
	}
	return nil
}

// readExternalBaseURL returns the origin an external service reaches this
// cluster on, as webd published it — or, when there is none, prose saying why,
// for the operator who is about to be asked for it by hand.
//
// A FAILED READ IS NOT FATAL TO THE PLAN, and that is a deliberate departure
// from this repo's fail-closed default. The URL is a seed: an optimisation
// whose absence is already handled, because the question simply gets asked.
// Failing the whole plan on it would stop an operator with namespace-scoped
// RBAC — who cannot get configmaps in the platform namespace — from installing
// a bundle whose channels never wanted the URL at all. Degrading and saying so
// satisfies the no-silent-errors rule by SURFACING, which is what that rule
// asks for when the failure affects something the user is waiting for.
//
// The four "no URL" cases are kept distinct in the prose because they are
// different things to tell an operator: nothing was attempted (offline), there
// is no such ConfigMap (the cluster has not published one yet), the ConfigMap
// is there but unpopulated, and I was not allowed to look.
//
// The value is trimmed of trailing slashes, exactly as the CLI's own reader of
// this key does: callers concatenate a path onto it, and a base URL ending in
// "/" yields a webhook URL with "//" in it that the receiving router answers
// with a 404 rather than a delivery.
func readExternalBaseURL(ctx context.Context, c client.Client) (url, why string) {
	where := externalurl.Namespace + "/" + spiceboxv1alpha1.WebdExternalURLConfigMap
	if c == nil {
		return "", fmt.Sprintf("this plan was made offline, with no cluster to read ConfigMap %s from", where)
	}
	var cm corev1.ConfigMap
	key := client.ObjectKey{Namespace: externalurl.Namespace, Name: spiceboxv1alpha1.WebdExternalURLConfigMap}
	if err := c.Get(ctx, key, &cm); err != nil {
		if apierrors.IsNotFound(err) {
			return "", fmt.Sprintf("there is no ConfigMap %s, so this cluster has not published an external URL yet", where)
		}
		// The underlying message ("... is forbidden: ...") is the actionable
		// half and is carried verbatim; a generic "could not read it" would
		// leave the operator no better off than silence.
		return "", fmt.Sprintf("could not read ConfigMap %s: %v", where, err)
	}
	url = strings.TrimRight(strings.TrimSpace(cm.Data[spiceboxv1alpha1.WebdTrustedURLKey]), "/")
	if url == "" {
		return "", fmt.Sprintf("ConfigMap %s carries no %q value yet", where, spiceboxv1alpha1.WebdTrustedURLKey)
	}
	return url, ""
}
