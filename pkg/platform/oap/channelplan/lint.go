// Package channelplan is everything about a bundle's DECLARED channels
// (oap.Requires.Channels) that needs the channel-kind registry: linting the
// declaration today, and planning the install-time wizard runs it drives next.
//
// It is a subpackage of oap rather than part of it because
// pkg/channels/channelkinds imports pkg/platform/oap (a kind's Wizard answers
// in oap.Questions), so oap cannot import the kind registry back without a
// cycle. A subpackage may import both — pkg/platform/oap/questionscreen sits
// in the same position for the same reason.
package channelplan

import (
	"fmt"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/validation"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"
	credregistry "github.com/authzed/openagentprimitives/pkg/platform/identity/credkind/registry"
	"github.com/authzed/openagentprimitives/pkg/platform/oap"

	// The credential-type registry every SecretNameFor call below dispatches
	// through. Blank-imported at LIBRARY level, which is the documented
	// exception for credkinds (AGENTS.md): the set is closed by
	// AgentCredential.Type's own CRD enum, so there is no "link only some of
	// them" case an importing binary could want, and an empty registry would
	// turn every credential in every bundle into an error.
	_ "github.com/authzed/openagentprimitives/pkg/platform/identity/credkind/imports"
)

// credsSecretSuffix is how a channel wizard names the Secret it writes:
// "<channel-name>-creds". Every kind that writes one follows it — github
// (credsSecretName in its wizard.go), slack (the same helper in its own), and
// bento — and RequiredChannel.Name's own doc states it as the reason the name
// is declared in the manifest rather than prompted for at install time.
//
// It is spelled here, and not read off the kinds, because there is no seam to
// read it from: a wizard produces its Secret only from a full set of answers,
// which a lint does not have.
const credsSecretSuffix = "-creds"

// manifestResource is the RequirementFinding.Resource for a finding about the
// declaration itself rather than about a bundled CR. The manifest is that file
// inside a source folder, and the config blob of that name inside a packed .oap.
const manifestResource = "oap.yaml"

// LintRequiredChannels checks a bundle's requires.channels declaration, and
// cross-checks it against the credentials the bundle's own AgentIdentities
// reference.
//
// THE CROSS-CHECK IS THE POINT. A channel wizard names the Secret it writes
// "<channel-name>-creds", and a bundled AgentIdentity names that Secret
// directly — so the two strings have to agree, and until now nothing made
// them. A bundle whose AgentIdentity says "demo-agent-github-creds" while its
// declaration says "demo-agent-gh" lints clean, installs clean, and fails on
// the first webhook with no token to clone with. The finding names BOTH
// strings, because which of the two is wrong is the author's call, not this
// lint's. Declaring a channel is what opts a bundle into that guarantee — see
// crossCheckCredsReferences for why a bundle declaring none is left alone.
//
// The declaration itself is checked for: a kind the channel-kind registry
// knows, a role that is a ChannelSpec.Role value, that is not "monitoring"
// (B-R12 — a monitoring Channel binds to no agent, so it is never one the
// installed agent needs), and that the kind serves, a name Kubernetes can
// accept as an object name, and no two entries claiming the same name.
//
// The role check asks the kind, through channelkinds.Kind.SupportedRoles, and
// keeps no list of its own. It deliberately does NOT call ValidateSpec: that
// judges a whole Channel, and every kind needing a kind-specific spec block
// refuses a block-less one for THAT reason (github wants spec.github, slack
// spec.slack, bento spec.bento.generate) — so its verdict on a declaration
// says nothing about the role, and inferring the legal roles from the shape of
// its error strings would be a switch on the kind in disguise. ValidateSpec
// stays the enforcement path at reconcile time, reading the same per-kind list
// this does, so the two cannot disagree.
//
// A nil bundle, an absent manifest, an undecodable manifest stream, or an
// unregistered credential type is a returned error, never a skip that reads as
// clean.
//
// AN INSTALL CLIENT CALLS CheckDeclared, NOT THIS. This is the lint half
// alone, which is the right entry point for `oap agent lint` — an AUTHOR
// judging a bundle, where there is no install and so no --name to refuse. An
// installer that reaches for this directly has half a gate, and a client
// stopping at the half it happened to remember is how this endpoint shipped
// without the credentials cross-check; CheckDeclared is the whole one.
func LintRequiredChannels(b *oap.Bundle) ([]oap.RequirementFinding, error) {
	if b == nil {
		return nil, fmt.Errorf("oap: LintRequiredChannels: nil bundle")
	}
	if b.Manifest == nil {
		return nil, fmt.Errorf("oap: LintRequiredChannels: bundle has no manifest")
	}
	// An empty registry means the linking binary forgot its channel-kind blank
	// imports, not that the author invented every kind they named. Fail as the
	// wiring bug it is rather than emitting a page of false accusations.
	if len(registry.All()) == 0 {
		return nil, fmt.Errorf("oap: LintRequiredChannels: channel-kind registry is empty — this binary is missing its channelkinds blank imports")
	}
	crs, err := b.CRs()
	if err != nil {
		return nil, fmt.Errorf("oap: LintRequiredChannels: decode bundled manifests: %w", err)
	}

	declared := b.Manifest.Requires.Channels
	findings := lintDeclarations(declared)
	findings = append(findings, lintDeliveryTarget(declared)...)

	crossFindings, err := crossCheckCredsReferences(b.Manifest, crs, declared)
	if err != nil {
		return nil, err
	}
	return append(findings, crossFindings...), nil
}

// DeclarationError is a bundle whose requires.channels declaration cannot be
// installed, carrying EVERY finding rather than only the first — an author
// fixing one name and re-running to discover the next is the experience a
// findings list exists to avoid.
//
// It is a type rather than a rendered string because its two clients render it
// differently — a CLI error an operator reads in a terminal, a 400 body a
// browser renders — while the VERDICT must be one function's. Error() is the
// client-neutral rendering, and carries no advice about any one command.
type DeclarationError struct {
	// Findings is what CheckDeclared found wrong, in the order
	// LintRequiredChannels reports them: the declarations first, in
	// declaration order, then the credential cross-check.
	Findings []oap.RequirementFinding
}

func (e *DeclarationError) Error() string {
	var b strings.Builder
	b.WriteString("this bundle's requires.channels declaration cannot be installed:")
	for _, f := range e.Findings {
		b.WriteString("\n")
		b.WriteString(f.String())
	}
	return b.String()
}

// CheckDeclared is the ONE gate every install client runs over a bundle's
// requires.channels BEFORE it touches the cluster: the --name refusal, then
// the declaration lint.
//
// IT IS ONE FUNCTION BECAUSE IT IS ONE DECISION, and the alternative has
// already shipped a defect. `oap agent install` called RefuseNamedInstall and
// LintRequiredChannels; admind's install endpoint called RefuseNamedInstall
// and nothing else — so a bundle whose AgentIdentity read
// "demo-agent-github-creds" while its declaration said "demo-agent-gh"
// installed cleanly from the console, rendered a form, wired the operator's
// answers, and left the agent reading a Secret nothing produces. Every CR
// healthy; the first inbound with no token. That is the very defect the
// cross-check exists to catch, reached on the one path that did not run it.
// Two clients each remembering to call two things is what produced it, so
// there is now one thing to call.
//
// PRESENTATION IS DELIBERATELY NOT SHARED. A caller that only tests `err !=
// nil` is correct by construction — the findings arrive AS an error, never as
// a second return value it could forget to look at — and a caller that wants
// to render them itself reaches them with errors.As on *DeclarationError.
//
// The order matters and is not alphabetical: --name is refused first because
// it is a refusal about the COMBINATION rather than about the bundle, so a
// bundle that is also malformed should still be told the simpler thing.
//
// A bundle that declares no channel raises no finding from either check, so
// this changes nothing for every bundle written before requires.channels
// existed. "Untouched" would overstate it: LintRequiredChannels still decodes
// every bundled CR to run its credentials cross-check, and a bundle whose
// manifests do not decode is an error from here.
func CheckDeclared(b *oap.Bundle, instanceName string) error {
	if b == nil || b.Manifest == nil {
		return fmt.Errorf("oap: CheckDeclared: bundle has no manifest")
	}
	if err := RefuseNamedInstall(instanceName, b.Manifest.Requires.Channels); err != nil {
		return err
	}
	findings, err := LintRequiredChannels(b)
	if err != nil {
		return fmt.Errorf("inspect the channels this bundle declares: %w", err)
	}
	if len(findings) == 0 {
		return nil
	}
	return &DeclarationError{Findings: findings}
}

// DeclaredNameWarning reports a Channel that did not take the name the bundle
// declared, or "" when it did (or when this run created no Channel to compare).
//
// It is the RUNTIME half of the cross-check LintRequiredChannels makes
// statically, and it exists because the static half cannot see this: the lint
// proves the bundle's own credentials and its declared channel names agree,
// and this catches the case where the Channel that actually got created is not
// the one the bundle was written against. A channel wizard names the Secret it
// writes "<channel-name>-creds" and a bundled AgentIdentity names that Secret
// directly, so a Channel created under another name leaves the agent with no
// credential — the exact failure the declaration removes, arriving silently.
//
// Every kind that ASKS for a name honours the seeded one; a kind whose
// manifests fix their own name does not, and that is worth a sentence rather
// than nothing.
//
// A warning and not a refusal, in both clients: by the time a name can be
// compared the Channel is already applied, and tearing it down would leave the
// operator worse off than telling them what happened.
//
// It takes the declaration and the manifest rather than three strings on
// purpose: kind, declared name and created name are all strings, so every
// transposition of them would compile.
func DeclaredNameWarning(rc oap.RequiredChannel, created *spiceboxv1alpha1.Channel) string {
	declared := strings.TrimSpace(rc.Name)
	if created == nil || declared == "" || created.Name == declared {
		return ""
	}
	return fmt.Sprintf(
		"the %s wizard named this Channel %q, not the declared %q — anything in this bundle that reads %q will not find it",
		rc.Kind, created.Name, declared, declared+credsSecretSuffix)
}

// lintDeliveryTarget reports a bundle that declares somewhere for work to
// ARRIVE and nowhere for it to GO (B-R14).
//
// THIS IS A PROPERTY OF THE SET, which is why it is not part of
// lintDeclarations: no single entry is wrong, and an author reading a
// per-entry finding would go looking for a typo in a line that is fine.
//
// WHAT IT CATCHES, and why it is worth a rule of its own. The Channel
// controller resolves a role=input Channel's delivery target at reconcile time
// — pkg/controllers/channel/controller.go step 6, "that target must resolve
// now, not at 09:00 on a Monday when the cron fires" — through
// outputbind.Resolve, which matches role=output ALONE. An agent declaring an
// input channel and no output one therefore installs cleanly, reports every CR
// applied, and leaves the input Channel Valid=False with reason
// ChannelOutputBindingUnresolvable. That is the exact "installs cleanly and
// does nothing" outcome requires.channels exists to make impossible, and
// declaring the channels in the manifest is what finally makes it checkable
// BEFORE the install.
//
// ROLE=BOTH DOES NOT SATISFY IT, and that is the half an author gets wrong:
// "both" reads like "does everything". outputbind refuses it deliberately (see
// its Resolve doc) because a both Channel is self-contained — origin and
// destination — so counting it as an output target would conflate "serves
// itself" with "serves someone else's input", and a cron digest would land in
// an interactive channel. The lint has to make the same call as the resolver
// or it would bless a bundle the cluster then refuses.
//
// IT FIRES ONLY ON THE COMBINATION. A bundle declaring no channel at all is
// untouched (as is every bundle written before requires.channels existed), and
// so is one declaring only an output — an agent that delivers and is triggered
// some other way is a real shape, and a Channel that is only an output asserts
// nothing about inputs. monitoring cannot appear in either half: B-R12 refuses
// it as a declared role outright.
//
// It reports against the FIRST input declaration rather than the manifest as a
// whole, because that entry is the one with nowhere to deliver — a concrete
// line to look at, with the set-level rule explained in the reason.
func lintDeliveryTarget(declared []oap.RequiredChannel) []oap.RequirementFinding {
	firstInput := -1
	hasOutput := false
	for i, rc := range declared {
		switch effectiveRole(rc) {
		case spiceboxv1alpha1.ChannelRoleInput:
			if firstInput < 0 {
				firstInput = i
			}
		case spiceboxv1alpha1.ChannelRoleOutput:
			hasOutput = true
		}
	}
	if firstInput < 0 || hasOutput {
		return nil
	}
	return []oap.RequirementFinding{declarationFinding(
		fmt.Sprintf("requires.channels[%d].role", firstInput),
		fmt.Sprintf(
			"%q declares role %q but this bundle declares no role %q channel, so the agent has somewhere for work to arrive and nowhere to deliver it. "+
				"The Channel controller resolves an input channel's delivery target when the Channel is created, and matches role %q alone — so this Channel would go Valid=False with reason %s on a cluster where everything else installed fine. "+
				"A role %q channel does NOT satisfy it: it is both origin and destination, so counting it as a delivery target would conflate serving itself with serving someone else's input (see pkg/channels/channelkinds/outputbind). "+
				"Declared here: %s. Give the channel that delivers this agent's work role %q, or drop the input declaration.",
			declared[firstInput].Name, spiceboxv1alpha1.ChannelRoleInput, spiceboxv1alpha1.ChannelRoleOutput,
			spiceboxv1alpha1.ChannelRoleOutput, spiceboxv1alpha1.ReasonChannelOutputBindingUnresolvable,
			spiceboxv1alpha1.ChannelRoleBoth, describeDeclaredRoles(declared), spiceboxv1alpha1.ChannelRoleOutput))}
}

// describeDeclaredRoles renders what the bundle actually declared, so the
// author can see the set the rule above judged rather than re-deriving it. The
// role shown is the EFFECTIVE one, because that is what was judged: a bundle
// that omitted the field entirely would otherwise be told off for a role it
// cannot find in its own manifest.
func describeDeclaredRoles(declared []oap.RequiredChannel) string {
	out := make([]string, 0, len(declared))
	for _, rc := range declared {
		out = append(out, fmt.Sprintf("%q is role %s", rc.Name, effectiveRole(rc)))
	}
	return strings.Join(out, ", ")
}

// effectiveRole is the role the Channel would actually GET: the declared one,
// or ChannelSpec.Role's own +kubebuilder:default when the field is omitted.
//
// One helper rather than the same `if role == "" ` in each check, because two
// checks disagreeing about what an omitted role means is a bundle that passes
// one and fails the other for the same field.
func effectiveRole(rc oap.RequiredChannel) string {
	if rc.Role == "" {
		return spiceboxv1alpha1.ChannelRoleBoth
	}
	return rc.Role
}

// lintDeclarations checks each requires.channels entry on its own, in
// declaration order, so the first finding an author reads is about the first
// entry that has one.
func lintDeclarations(declared []oap.RequiredChannel) []oap.RequirementFinding {
	var findings []oap.RequirementFinding
	firstByName := map[string]int{}

	for i, rc := range declared {
		path := fmt.Sprintf("requires.channels[%d]", i)

		switch {
		case rc.Name == "":
			findings = append(findings, declarationFinding(path+".name",
				"name is required: it is the Channel CR's own name, and the Secret its wizard writes is derived from it"))
		default:
			if errs := validation.IsDNS1123Subdomain(rc.Name); len(errs) > 0 {
				// Subdomain, not label: this becomes a CR's metadata.name, and
				// the apiserver's rule for one is the subdomain rule. Linting
				// the stricter label rule would reject a dotted name the
				// cluster accepts.
				findings = append(findings, declarationFinding(path+".name", fmt.Sprintf(
					"%q is not a usable Channel name: %s", rc.Name, strings.Join(errs, "; "))))
			} else if first, dup := firstByName[rc.Name]; dup {
				findings = append(findings, declarationFinding(path+".name", fmt.Sprintf(
					"%q is already declared by requires.channels[%d]; two Channels cannot share a name, and the credential Secrets derived from it would collide too",
					rc.Name, first)))
			} else {
				firstByName[rc.Name] = i
			}
		}

		kind, kindRegistered := registry.Get(rc.Kind)
		if !kindRegistered {
			findings = append(findings, declarationFinding(path+".kind", fmt.Sprintf(
				"%q is not a registered channel kind; this build has %s",
				rc.Kind, strings.Join(registry.Names(), ", "))))
		}

		// An omitted role is not a shape error: ChannelSpec.Role carries
		// +kubebuilder:default=both, so the apiserver would read the same
		// default. Judge the value the Channel would actually get — through
		// the same helper lintDeliveryTarget judges with, so the two checks
		// cannot come to disagree about what an omitted field means.
		role := effectiveRole(rc)
		legalRoles := spiceboxv1alpha1.AllChannelRoles()
		if !slices.Contains(legalRoles, role) {
			findings = append(findings, declarationFinding(path+".role", fmt.Sprintf(
				"%q is not one of %s", rc.Role, strings.Join(legalRoles, "|"))))
			continue
		}
		// B-R12. "monitoring" is a legal ChannelSpec.Role and is NOT a legal
		// DECLARED one, and the two are different questions. A monitoring
		// Channel posts framework events and binds to no AgentClass — the
		// channel reconciler skips the binding for one entirely — so it is by
		// definition not "a Channel the installed agent needs to function",
		// which is the whole meaning of requires.channels.
		//
		// Refused here rather than caught downstream because the downstream
		// failure is silent. Several kinds report monitoring among their
		// SupportedRoles (slack serves all four), so the role check below
		// accepts it; install then drives that kind's AGENT flow, whose
		// manifests carry no role, and stamps the declared "monitoring" onto
		// the result. The outcome is a Channel nothing routes to the agent, or
		// one the kind's own ValidateSpec rejects for missing a monitoring
		// destination it was never asked for. Making the combination
		// unrepresentable is a far smaller rule than teaching install to
		// reconcile a kind's own manifest role against a declared one.
		if role == spiceboxv1alpha1.ChannelRoleMonitoring {
			findings = append(findings, declarationFinding(path+".role", fmt.Sprintf(
				"%q cannot be declared in requires.channels: a monitoring Channel binds to no agent, so it is never a channel the installed agent needs. "+
					"Declare the role the agent actually uses (%s), and create the monitoring channel separately with `oap channel create --monitoring`",
				role, strings.Join(spiceboxv1alpha1.AgentChannelRoles(), "|"))))
			continue
		}
		// registry.Get returns a nil INTERFACE for an unregistered name, so
		// this guard is load-bearing: without it the SupportedRoles call below
		// dereferences nil and takes the whole lint down. The finding above
		// already told the author what is wrong.
		if !kindRegistered {
			continue
		}
		if served := kind.SupportedRoles(); !slices.Contains(served, role) {
			findings = append(findings, declarationFinding(path+".role", fmt.Sprintf(
				"kind %q does not serve role %q; it serves %s",
				rc.Kind, role, strings.Join(served, "|"))))
		}
	}
	return findings
}

// crossCheckCredsReferences finds every bundled AgentIdentity credential whose
// Secret follows the "<channel-name>-creds" convention but names a channel
// this bundle never declares.
//
// The Secret's name comes from credregistry.SecretNameFor, the registry's own
// dispatch, rather than from a `case cred.Static != nil:` chain here — that
// helper exists so callers stop enumerating credential types, and a new type
// must not need an edit in this file. Its three outcomes are distinct and are
// kept distinct: an unregistered type is an error (a bundle naming a type no
// build has), "" means the type stores nothing at all (federated mints per
// resolve), and anything else is a Secret name.
//
// A Secret the bundle ALREADY accounts for is never a finding, whatever it is
// called: a secret question's createSecret names one install materializes from
// an answer. Without that exemption an ordinary "<vendor>-creds" static token
// collected by a question would be reported as a dangling channel reference.
// requires.secrets is NOT such a signal — it is derived from these very
// credentials, so exempting it would silence the check (see
// accountedSecretNames).
//
// A bundle that declares NO channel is not checked at all. The defect this
// finds is two strings disagreeing, and a bundle with no declaration has only
// one of them: its Channel is created out of band with `oap channel create`,
// where the operator types the name and the bundle's README is what tells them
// which one. Reporting that as a dangling reference would not be a
// disagreement detected, it would be this lint demanding an optional
// declaration — and would fail every bundle written before requires.channels
// existed. Declaring a channel is what opts a bundle into the guarantee.
func crossCheckCredsReferences(m *oap.Manifest, crs []*unstructured.Unstructured, declared []oap.RequiredChannel) ([]oap.RequirementFinding, error) {
	if len(declared) == 0 {
		return nil, nil
	}
	accounted := accountedSecretNames(m)

	// Nameless entries are skipped: a channel with no name produces no
	// "<name>-creds" Secret, so it gives a credential nothing to agree with.
	// isDeclared therefore holds every NAMED declaration, including one whose
	// name lintDeclarations has already rejected as unusable — a declared
	// "Demo_Agent" still accounts for "Demo_Agent-creds" here. That is
	// deliberate: the author is told once, about the name, rather than a
	// second time about a Secret whose only problem is that same name.
	declaredNames := make([]string, 0, len(declared))
	isDeclared := make(map[string]bool, len(declared))
	for _, rc := range declared {
		if rc.Name == "" || isDeclared[rc.Name] {
			continue
		}
		isDeclared[rc.Name] = true
		declaredNames = append(declaredNames, rc.Name)
	}
	// Every declaration was nameless. The guard above only established that
	// the SLICE is non-empty, and skipping the nameless entries can still
	// leave nothing to compare against — which was not merely a vacuous
	// cross-check but a crash, since danglingCredsReason quotes
	// declaredNames[0]. lintDeclarations has already reported each nameless
	// entry; that is the finding the author needs, and it is the one the panic
	// used to eat before it could be printed.
	if len(declaredNames) == 0 {
		return nil, nil
	}

	var findings []oap.RequirementFinding
	for _, cr := range crs {
		if cr.GetKind() != "AgentIdentity" {
			continue
		}
		var ai spiceboxv1alpha1.AgentIdentity
		if err := oap.DecodeBundledCR(cr, &ai); err != nil {
			return nil, fmt.Errorf("oap: LintRequiredChannels: AgentIdentity %q: %w", cr.GetName(), err)
		}
		for i, cred := range ai.Spec.Credentials {
			secretName, err := credregistry.SecretNameFor(cred)
			if err != nil {
				return nil, fmt.Errorf("oap: LintRequiredChannels: AgentIdentity %q credential %q: %w",
					cr.GetName(), cred.Name, err)
			}
			if secretName == "" || accounted[secretName] {
				continue
			}
			channelName, followsConvention := strings.CutSuffix(secretName, credsSecretSuffix)
			if !followsConvention || channelName == "" || isDeclared[channelName] {
				continue
			}
			findings = append(findings, oap.RequirementFinding{
				Resource: "AgentIdentity/" + cr.GetName(),
				// The block's key is the type discriminator itself
				// (AgentCredential.Type's enum values are the JSON names of the
				// blocks they select), so the path stays exact without this
				// file knowing any one type.
				Path:   fmt.Sprintf("spec.credentials[%d].%s.secretRef.name", i, cred.Type),
				Reason: danglingCredsReason(cred.Name, secretName, channelName, declaredNames),
			})
		}
	}
	return findings, nil
}

// danglingCredsReason names BOTH strings that disagree — the Secret the
// credential reads and the channel names the manifest declares — because
// either one could be the typo and only the author knows which.
//
// declaredNames is non-empty, and the caller is what makes that true:
// crossCheckCredsReferences returns before reaching here both when the bundle
// declares no channel and when every channel it declares is nameless. Do not
// weaken either guard — declaredNames[0] below is why the second one exists.
func danglingCredsReason(credName, secretName, impliedChannel string, declaredNames []string) string {
	return fmt.Sprintf(
		"credential %q reads Secret %q, which nothing in this bundle produces. "+
			"A channel wizard names the Secret it writes %q, so that Secret belongs to a Channel named %q "+
			"— and requires.channels declares %s. "+
			"Make the two agree: rename the declared channel to %q, or point the secretRef at that channel's own %q.",
		credName, secretName, "<channel-name>"+credsSecretSuffix, impliedChannel,
		quotedList(declaredNames), impliedChannel, declaredNames[0]+credsSecretSuffix)
}

// accountedSecretNames is every Secret name the bundle itself explains: the
// ones install materializes from a secret question's answer.
//
// requires.secrets is DELIBERATELY not a source here. It is now DERIVED from the
// bundled AgentIdentity credentials (see oap.DeriveInherited), so it mirrors the
// exact set of credential Secrets crossCheckCredsReferences walks — exempting it
// would make every credential Secret account for itself and silence the dangling
// check on the FromFolder path the check exists to protect. A credential Secret
// is legitimately explained only by matching a declared channel (the caller's
// isDeclared) or by a secret question install answers; a "<x>-creds" Secret that
// is neither is the dangling reference this reports.
func accountedSecretNames(m *oap.Manifest) map[string]bool {
	accounted := make(map[string]bool, len(m.Questions))
	for _, q := range m.Questions {
		if q.Secret != nil && q.Secret.CreateSecret != nil && q.Secret.CreateSecret.Name != "" {
			accounted[q.Secret.CreateSecret.Name] = true
		}
	}
	return accounted
}

// declarationFinding is a finding about the manifest's own requires.channels,
// which belongs to no CR.
func declarationFinding(path, reason string) oap.RequirementFinding {
	return oap.RequirementFinding{Resource: manifestResource, Path: path, Reason: reason}
}

// quotedList renders names as `"a", "b"` — quoted so a name that is a prefix
// of another (or of a Secret named after it) is still distinguishable in the
// rendered message.
func quotedList(names []string) string {
	quoted := make([]string, 0, len(names))
	for _, n := range names {
		quoted = append(quoted, fmt.Sprintf("%q", n))
	}
	return strings.Join(quoted, ", ")
}
