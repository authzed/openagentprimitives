package channelplan_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/fake"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"
	"github.com/authzed/openagentprimitives/pkg/platform/oap"
	"github.com/authzed/openagentprimitives/pkg/platform/oap/channelplan"

	// The kinds the declarations below name. This is a TEST binary; the `oap`
	// binary gets them from cmd/oap/internal/agentcmd's own blank imports.
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/bento"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/github"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/slack"
)

// roleStrictKind is a test-only channel kind serving a role set no registered
// kind has: input and monitoring, but not output or both. It exists so the
// role check is proved to read whatever the kind ANSWERS rather than any
// property of github (the only narrow kind it could otherwise be tested
// against, and input-only, so "input-only" and "what the kind said" would be
// indistinguishable).
//
// It models the contract the Kind interface states: ValidateSpec derives its
// role refusal from SupportedRoles, so the declaration a lint reads and the
// rule reconcile enforces are one list. It embeds fake.Kind for the interface
// methods it does not care about.
type roleStrictKind struct{ fake.Kind }

func (roleStrictKind) Name() string { return "rolestrict" }

func (roleStrictKind) SupportedRoles() []string {
	return []string{spiceboxv1alpha1.ChannelRoleInput, spiceboxv1alpha1.ChannelRoleMonitoring}
}

func (k roleStrictKind) ValidateSpec(ch *spiceboxv1alpha1.Channel) error {
	if !slices.Contains(k.SupportedRoles(), ch.Spec.Role) {
		return fmt.Errorf("rolestrict channels must declare role=%s; got %q",
			strings.Join(k.SupportedRoles(), "|"), ch.Spec.Role)
	}
	return nil
}

var _ channelkinds.Kind = roleStrictKind{}

func init() { registry.Register(roleStrictKind{}) }

// WHY SO MANY FIXTURES BELOW CARRY A SPARE `{kind: slack, role: output, name:
// demo-agent-out}`. B-R14 refuses a bundle that declares a role=input channel
// and no role=output one, because the Channel controller resolves an input
// channel's delivery target when the Channel is created and matches
// role=output alone — so such a bundle installs cleanly and delivers nothing.
// Every fixture whose SUBJECT is something else (the credentials cross-check, a
// shape error, a kind's own role set) carries that line so it raises exactly
// the one finding it is about. The fixtures for B-R14 itself deliberately do
// not, and say so.

// bundleWith builds a Bundle from a `requires:`/`questions:` fragment plus any
// number of CR documents. The fragment goes through oap.ParseManifest rather
// than being hand-built as a struct, so each case also pins that the YAML
// spelling of requires.channels is the one the manifest type accepts.
func bundleWith(t *testing.T, manifestFragment string, crs ...string) *oap.Bundle {
	t.Helper()
	m, err := oap.ParseManifest([]byte(
		"oapFormatVersion: \"1\"\nagent:\n  name: demo-agent\n  version: \"1.0.0\"\n" + manifestFragment))
	require.NoError(t, err, "the test's own manifest fragment must parse")
	return &oap.Bundle{Manifest: m, Manifests: []byte(strings.Join(crs, "\n---\n"))}
}

// agentIdentityReferencingSecret is a bundled AgentIdentity with one githubApp
// credential pointing at secretName — the shape whose Secret a channel wizard,
// not the bundle, produces.
func agentIdentityReferencingSecret(t *testing.T, secretName string) string {
	t.Helper()
	return fmt.Sprintf(`apiVersion: agentprimitives.authzed.com/v1alpha1
kind: AgentIdentity
metadata:
  name: demo-agent-id
spec:
  credentials:
    - name: github-app
      type: githubApp
      githubApp:
        secretRef:
          name: %s
`, secretName)
}

func TestLintRequiredChannels_DanglingCredsReferenceIsCaught(t *testing.T) {
	// An AgentIdentity naming <name>-creds, with no channel declaring <name>.
	b := bundleWith(t, `
requires:
  channels:
    - {kind: github, role: input, name: demo-agent-gh}
    - {kind: slack, role: output, name: demo-agent-out}
`, agentIdentityReferencingSecret(t, "demo-agent-github-creds"))

	findings, err := channelplan.LintRequiredChannels(b)
	require.NoError(t, err)
	require.Len(t, findings, 1, "a credential pointing at a Secret nothing produces must not lint clean")
	assert.Contains(t, findings[0].Reason, `"demo-agent-github-creds"`,
		"the finding must name the dangling reference")
	// The closing quote is load-bearing: "demo-agent-gh" is a PREFIX of
	// "demo-agent-github-creds", so a bare Contains would pass on the dangling
	// name alone and say nothing about whether the declared name was reported.
	assert.Contains(t, findings[0].Reason, `"demo-agent-gh"`,
		"and the declared channel, so the operator can see which of the two to change")
	assert.Equal(t, "AgentIdentity/demo-agent-id", findings[0].Resource,
		"the offending CR is the AgentIdentity that holds the reference")
	assert.Equal(t, "spec.credentials[0].githubApp.secretRef.name", findings[0].Path)
}

func TestLintRequiredChannels_MatchingNameIsClean(t *testing.T) {
	b := bundleWith(t, `
requires:
  channels:
    - {kind: github, role: input, name: demo-agent-gh}
    - {kind: slack, role: output, name: demo-agent-out}
`, agentIdentityReferencingSecret(t, "demo-agent-gh-creds"))

	findings, err := channelplan.LintRequiredChannels(b)
	require.NoError(t, err)
	assert.Empty(t, findings)
}

// A -creds Secret install materializes from a secret question is NOT a
// channel's. Without this exemption, an operator-supplied "<something>-creds"
// token collected by a question would be accused of a dangling channel
// reference.
//
// requires.secrets is deliberately NOT such an exemption: it is derived from the
// bundled credentials (oap.DeriveInherited), so it mirrors the very Secrets this
// check walks — exempting it would silence the check on the FromFolder path it
// exists to protect (see accountedSecretNames).
func TestLintRequiredChannels_ACredsSecretMaterializedByAQuestionIsClean(t *testing.T) {
	b := bundleWith(t, `
requires:
  channels:
    - {kind: github, role: input, name: demo-agent-gh}
    - {kind: slack, role: output, name: demo-agent-out}
questions:
  - name: vendorToken
    type: secret
    prompt: "Vendor token"
    secret:
      createSecret: {name: demo-agent-vendor-creds, key: token}
`, agentIdentityReferencingSecret(t, "demo-agent-vendor-creds"))
	findings, err := channelplan.LintRequiredChannels(b)
	require.NoError(t, err)
	assert.Empty(t, findings)
}

// A bundle that declares no channel opts out of the cross-check: its Channel
// is created out of band with `oap channel create`, so there is no second
// string to disagree with, and treating the credential as dangling would fail
// every bundle written before requires.channels existed. Pinned so the scope
// is a decision, not an accident — and so the opposite (making the optional
// declaration effectively mandatory) cannot be introduced unnoticed.
func TestLintRequiredChannels_MakesNoCredsClaimWhenNoChannelIsDeclared(t *testing.T) {
	b := bundleWith(t, "", agentIdentityReferencingSecret(t, "demo-agent-gh-creds"))
	findings, err := channelplan.LintRequiredChannels(b)
	require.NoError(t, err)
	assert.Empty(t, findings)

	// The same credential, once ONE channel is declared, is checked — so the
	// clean run above is the absence of a declaration and not the absence of
	// the check.
	declared := bundleWith(t, `
requires:
  channels:
    - {kind: github, role: input, name: demo-agent-other}
    - {kind: slack, role: output, name: demo-agent-out}
`, agentIdentityReferencingSecret(t, "demo-agent-gh-creds"))
	findings, err = channelplan.LintRequiredChannels(declared)
	require.NoError(t, err)
	require.Len(t, findings, 1)
	assert.Contains(t, findings[0].Reason, `"demo-agent-gh-creds"`)
}

func TestLintRequiredChannels_RejectsTheShapeErrors(t *testing.T) {
	cases := []struct {
		name, fragment, wantPath, wantReason string
	}{
		{
			name:       "unregistered kind: rejected naming it",
			fragment:   `requires: {channels: [{kind: nosuch, role: input, name: demo-agent-x}, {kind: slack, role: output, name: demo-agent-out}]}`,
			wantPath:   "requires.channels[0].kind",
			wantReason: `"nosuch" is not a registered channel kind`,
		},
		{
			name:       "role is not a ChannelSpec.Role value: rejected naming it",
			fragment:   `requires: {channels: [{kind: slack, role: sideways, name: demo-agent-x}]}`,
			wantPath:   "requires.channels[0].role",
			wantReason: `"sideways" is not one of input|output|both|monitoring`,
		},
		{
			name:       "name is not a DNS-1123 subdomain: rejected",
			fragment:   `requires: {channels: [{kind: slack, role: output, name: Demo_Agent}]}`,
			wantPath:   "requires.channels[0].name",
			wantReason: `"Demo_Agent" is not a usable Channel name`,
		},
		{
			name:       "no name at all: rejected saying what the name is for",
			fragment:   `requires: {channels: [{kind: github, role: input}, {kind: slack, role: output, name: demo-agent-out}]}`,
			wantPath:   "requires.channels[0].name",
			wantReason: "name is required",
		},
		{
			name:       "duplicate names: rejected naming the earlier entry",
			fragment:   `requires: {channels: [{kind: slack, role: output, name: dup}, {kind: bento, role: input, name: dup}]}`,
			wantPath:   "requires.channels[1].name",
			wantReason: `"dup" is already declared by requires.channels[0]`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			findings, err := channelplan.LintRequiredChannels(bundleWith(t, tc.fragment))
			require.NoError(t, err)
			require.Len(t, findings, 1)
			assert.Equal(t, tc.wantPath, findings[0].Path)
			assert.Contains(t, findings[0].Reason, tc.wantReason)
			assert.Equal(t, "oap.yaml", findings[0].Resource,
				"a declaration finding is about the manifest, not about any CR")
		})
	}
}

// A role a REAL kind does not serve is rejected. github is input-only
// (github/kind.go's SupportedRoles), so `{kind: github, role: output}` — which
// installs cleanly and then never receives a webhook — must not lint clean.
//
// The kind is asked; nothing here knows github is input-only. Every role in
// the CRD enum is covered, so the check cannot pass by rejecting one spelling
// and missing the rest.
// "monitoring" is deliberately NOT in the table below any more. It is refused
// for every kind by B-R12, before the kind's own role set is consulted at all,
// so asserting github's "does not serve" wording on it would be asserting a
// message the author never sees. The case did not go away — it is
// TestLintRequiredChannels_MonitoringIsNeverDeclarable, which pins it on a kind
// that DOES serve monitoring and is therefore the stronger placement.
func TestLintRequiredChannels_RoleTheKindDoesNotServeIsRejected(t *testing.T) {
	for _, role := range []string{"output", "both"} {
		t.Run(role+": rejected naming the kind and the role", func(t *testing.T) {
			findings, err := channelplan.LintRequiredChannels(bundleWith(t,
				fmt.Sprintf(`requires: {channels: [{kind: github, role: %s, name: demo-agent-gh}]}`, role)))
			require.NoError(t, err)
			require.Len(t, findings, 1)
			assert.Equal(t, "requires.channels[0].role", findings[0].Path)
			assert.Contains(t, findings[0].Reason,
				fmt.Sprintf(`kind "github" does not serve role %q`, role))
			assert.Contains(t, findings[0].Reason, "it serves input",
				"and says what it does serve, so the author can act without reading the kind's source")
		})
	}

	t.Run("input: the role github does serve is clean", func(t *testing.T) {
		findings, err := channelplan.LintRequiredChannels(bundleWith(t,
			`requires: {channels: [{kind: github, role: input, name: demo-agent-gh}, {kind: slack, role: output, name: demo-agent-out}]}`))
		require.NoError(t, err)
		assert.Empty(t, findings, "otherwise the cases above would pass for a check that rejects every role")
	})
}

// The verdict is the KIND's, not a rule about github. rolestrict serves
// {input, monitoring} — a set no registered kind has — and the lint must
// follow it in both directions.
func TestLintRequiredChannels_ReadsEachKindsOwnRoleSet(t *testing.T) {
	findings, err := channelplan.LintRequiredChannels(bundleWith(t,
		`requires: {channels: [{kind: rolestrict, role: output, name: demo-agent-x}]}`))
	require.NoError(t, err)
	require.Len(t, findings, 1)
	assert.Contains(t, findings[0].Reason, `kind "rolestrict" does not serve role "output"`)
	// The reason quotes THIS kind's own set — "input|monitoring", which no
	// registered kind has — so a hardcoded rule, or one copied from any real
	// kind, cannot produce this sentence.
	assert.Contains(t, findings[0].Reason, "it serves input|monitoring")

	// The one role this kind serves that a bundle may also declare.
	clean, err := channelplan.LintRequiredChannels(bundleWith(t,
		`requires: {channels: [{kind: rolestrict, role: input, name: demo-agent-x}, {kind: slack, role: output, name: demo-agent-out}]}`))
	require.NoError(t, err)
	assert.Empty(t, clean)
}

// TestLintRequiredChannels_MonitoringIsNeverDeclarable pins B-R12, and pins it
// on the kind that makes the ruling non-obvious: rolestrict SERVES monitoring,
// so this refusal is not the kind's own role set talking. It is the separate
// rule that a monitoring Channel binds to no agent and therefore can never be
// "a Channel the installed agent needs", whatever the kind can deliver.
//
// Without it the combination is accepted and fails silently downstream: several
// registered kinds report monitoring among their SupportedRoles (slack serves
// all four), install drives that kind's AGENT flow, and the declared role is
// stamped onto manifests built for something else — leaving a Channel nothing
// routes to the agent.
func TestLintRequiredChannels_MonitoringIsNeverDeclarable(t *testing.T) {
	findings, err := channelplan.LintRequiredChannels(bundleWith(t,
		`requires: {channels: [{kind: rolestrict, role: monitoring, name: demo-agent-x}]}`))
	require.NoError(t, err)
	require.Len(t, findings, 1)
	assert.Equal(t, "requires.channels[0].role", findings[0].Path)
	assert.Contains(t, findings[0].Reason, "binds to no agent",
		"the message must say WHY monitoring is excluded, so the next author does not read it as an oversight")
	assert.Contains(t, findings[0].Reason, "input|output|both",
		"and must name the roles that ARE declarable")
	assert.Contains(t, findings[0].Reason, "oap channel create --monitoring",
		"a monitoring channel is still creatable — just not by declaring it")
}

// TestLintRequiredChannels_AnInputWithNoDeliveryTargetIsRejected pins B-R14.
//
// The Channel controller resolves a role=input Channel's delivery target when
// the Channel is created (controller.go step 6) through outputbind.Resolve,
// which matches role=output ALONE. So an agent declaring an input channel and
// no output one installs cleanly, applies every CR, and leaves the input
// Channel Valid=False with reason OutputBindingUnresolvable — the exact
// "installs cleanly and does nothing" outcome requires.channels exists to
// remove, and now catchable before the install because the bundle declares
// what it needs.
//
// The finding must be reported against the INPUT declaration, because that is
// the entry with nowhere to deliver.
func TestLintRequiredChannels_AnInputWithNoDeliveryTargetIsRejected(t *testing.T) {
	findings, err := channelplan.LintRequiredChannels(bundleWith(t,
		`requires: {channels: [{kind: github, role: input, name: demo-agent-gh}]}`))
	require.NoError(t, err)
	require.Len(t, findings, 1)
	assert.Equal(t, "oap.yaml", findings[0].Resource,
		"it is a property of the manifest's declaration set, not of any CR")
	assert.Equal(t, "requires.channels[0].role", findings[0].Path,
		"the entry with nowhere to deliver is the one to point at")
	assert.Contains(t, findings[0].Reason, "nowhere to deliver it",
		"the message must say what actually goes wrong")
	assert.Contains(t, findings[0].Reason, "OutputBindingUnresolvable",
		"and name the condition the operator would otherwise have to go and find on the Channel")
	assert.Contains(t, findings[0].Reason, "outputbind",
		"and point at the code that makes the ruling, so the next reader can check it")
}

// TestLintRequiredChannels_RoleBothIsNotADeliveryTarget is the half an author
// gets wrong, and the reason B-R14 is worth a rule rather than a README line:
// "both" reads like "does everything". outputbind refuses it deliberately — a
// both Channel is origin AND destination, so counting it as somebody else's
// output target would conflate serving itself with serving another channel's
// input, and a cron digest would land in an interactive channel.
//
// bento is the input kind here rather than github because it is the shape that
// makes the mistake natural: a cron trigger with no rendering surface, beside a
// slack channel an author would reasonably mark `both`.
func TestLintRequiredChannels_RoleBothIsNotADeliveryTarget(t *testing.T) {
	findings, err := channelplan.LintRequiredChannels(bundleWith(t,
		`requires: {channels: [{kind: bento, role: input, name: demo-agent-trigger}, {kind: slack, role: both, name: demo-agent-chat}]}`))
	require.NoError(t, err)
	require.Len(t, findings, 1,
		"role=both is a legal role slack serves, so nothing else may object to it — only B-R14")
	assert.Equal(t, "requires.channels[0].role", findings[0].Path)
	assert.Contains(t, findings[0].Reason, `A role "both" channel does NOT satisfy it`)
	assert.Contains(t, findings[0].Reason, `"demo-agent-chat" is role both`,
		"the author must be able to see the set that was judged, or they will look for a channel they did not declare")
}

// TestLintRequiredChannels_TheDeliveryRuleFiresOnlyOnTheCombination. B-R14 is
// about a PAIRING; each arm below is a bundle that is fine on its own terms,
// and a rule that fired on any of them would fail bundles that work.
//
// The no-channels arm is the load-bearing one: it is every bundle written
// before requires.channels existed, and it must be untouched by this and by
// every other declaration rule.
func TestLintRequiredChannels_TheDeliveryRuleFiresOnlyOnTheCombination(t *testing.T) {
	cases := []struct {
		name, fragment string
	}{{
		name:     "no channels declared at all: untouched, as every pre-requires.channels bundle must be",
		fragment: "",
	}, {
		name:     "only an output: an agent that delivers and is triggered another way is a real shape",
		fragment: `requires: {channels: [{kind: slack, role: output, name: demo-agent-out}]}`,
	}, {
		name:     "only a both: self-contained, so it asserts nothing about inputs",
		fragment: `requires: {channels: [{kind: slack, role: both, name: demo-agent-chat}]}`,
	}, {
		name:     "an omitted role, which defaults to both: same, and not read as an input",
		fragment: `requires: {channels: [{kind: slack, name: demo-agent-chat}]}`,
	}, {
		name:     "input plus output: the shape the rule exists to bless",
		fragment: `requires: {channels: [{kind: github, role: input, name: demo-agent-gh}, {kind: slack, role: output, name: demo-agent-out}]}`,
	}, {
		name:     "two inputs and one output: one delivery target serves them all",
		fragment: `requires: {channels: [{kind: github, role: input, name: demo-agent-gh}, {kind: bento, role: input, name: demo-agent-trigger}, {kind: slack, role: output, name: demo-agent-out}]}`,
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			findings, err := channelplan.LintRequiredChannels(bundleWith(t, tc.fragment))
			require.NoError(t, err)
			assert.Empty(t, findings)
		})
	}
}

// An omitted role is the CRD's own default (+kubebuilder:default=both), not a
// shape error — and it is that default, not the empty string, that gets
// judged.
func TestLintRequiredChannels_OmittedRoleIsJudgedAsTheCRDDefault(t *testing.T) {
	t.Run("kind serving both: clean", func(t *testing.T) {
		findings, err := channelplan.LintRequiredChannels(bundleWith(t,
			`requires: {channels: [{kind: slack, name: demo-agent-x}]}`))
		require.NoError(t, err)
		assert.Empty(t, findings)
	})

	t.Run(`kind not serving both: rejected naming "both", not ""`, func(t *testing.T) {
		findings, err := channelplan.LintRequiredChannels(bundleWith(t,
			`requires: {channels: [{kind: github, name: demo-agent-gh}]}`))
		require.NoError(t, err)
		require.Len(t, findings, 1)
		// Naming "both" is the whole point: it proves the default was applied
		// and then judged. An implementation that skipped an empty role would
		// report nothing, and one that judged "" verbatim would say `role ""`.
		assert.Contains(t, findings[0].Reason, `does not serve role "both"`)
	})
}

func TestLintRequiredChannels_RefusesInputItCannotJudge(t *testing.T) {
	t.Run("nil bundle: error", func(t *testing.T) {
		_, err := channelplan.LintRequiredChannels(nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "nil bundle")
	})

	t.Run("no manifest: error, not a clean run", func(t *testing.T) {
		_, err := channelplan.LintRequiredChannels(&oap.Bundle{})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no manifest")
	})

	t.Run("undecodable manifests stream: error", func(t *testing.T) {
		b := bundleWith(t, `requires: {channels: [{kind: slack, role: output, name: demo-agent-x}]}`)
		b.Manifests = []byte("\tthis: is: not: yaml\n")
		_, err := channelplan.LintRequiredChannels(b)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "decode bundled manifests")
	})

	t.Run("undecodable AgentIdentity: error naming the CR", func(t *testing.T) {
		b := bundleWith(t, `requires: {channels: [{kind: github, role: input, name: demo-agent-gh}]}`,
			`apiVersion: agentprimitives.authzed.com/v1alpha1
kind: AgentIdentity
metadata:
  name: demo-agent-id
spec:
  credentials: "not a list"
`)
		_, err := channelplan.LintRequiredChannels(b)
		require.Error(t, err)
		assert.Contains(t, err.Error(), `AgentIdentity "demo-agent-id": decode:`)
	})

	t.Run("unregistered credential type: error naming it, never a silent skip", func(t *testing.T) {
		b := bundleWith(t, `requires: {channels: [{kind: github, role: input, name: demo-agent-gh}]}`,
			`apiVersion: agentprimitives.authzed.com/v1alpha1
kind: AgentIdentity
metadata:
  name: demo-agent-id
spec:
  credentials:
    - name: mystery
      type: nosuchcredtype
`)
		_, err := channelplan.LintRequiredChannels(b)
		require.Error(t, err)
		assert.Contains(t, err.Error(), `credential "mystery": unknown credential type "nosuchcredtype"`)
		assert.Contains(t, err.Error(), "demo-agent-id")
	})

	t.Run("empty channel-kind registry: wiring error, never an authoring accusation", func(t *testing.T) {
		saved := registry.All()
		registry.Reset()
		t.Cleanup(func() {
			registry.Reset()
			for _, k := range saved {
				registry.Register(k)
			}
		})

		_, err := channelplan.LintRequiredChannels(bundleWith(t,
			`requires: {channels: [{kind: github, role: input, name: demo-agent-gh}, {kind: slack, role: output, name: demo-agent-out}]}`))
		require.Error(t, err, "with no kinds linked, every declaration would look unregistered")
		assert.Contains(t, err.Error(), "channel-kind registry is empty")
	})
}

// A declaration with no name must not take the whole lint down. This crashed
// `oap agent lint` outright: crossCheckCredsReferences guards on len(declared)
// == 0, but it builds declaredNames by SKIPPING nameless entries — so one
// nameless declaration left declared non-empty and declaredNames empty, and
// the reason builder indexed declaredNames[0].
//
// The nameless declaration is a first-class authoring typo (nothing validates
// RequiredChannel.Name at parse time), and the panic beat the render, so the
// "name is required" finding the lint had already computed never printed.
//
// require.NotPanics rather than a bare call so a regression fails THIS test
// with its own message instead of aborting the whole package binary.
func TestLintRequiredChannels_NamelessDeclarationDoesNotCrashTheCrossCheck(t *testing.T) {
	// The role is left at its default rather than declared `input`, and NO
	// sibling output declaration is added, because this fixture has to leave
	// declaredNames EMPTY — that is the state that used to panic. An input
	// declaration would additionally raise B-R14 (no delivery target), and a
	// named output sibling would give the cross-check a name to disagree with,
	// so either one would add a second finding that is not this test's subject
	// and hide the "and NOTHING from the cross-check" claim below.
	b := bundleWith(t, `
requires:
  channels:
    - {kind: slack}
`, agentIdentityReferencingSecret(t, "demo-agent-gh-creds"))

	var (
		findings []oap.RequirementFinding
		err      error
	)
	require.NotPanics(t, func() { findings, err = channelplan.LintRequiredChannels(b) })
	require.NoError(t, err)

	// The nameless declaration, and NOTHING from the cross-check: a channel
	// with no name produces no "<name>-creds" Secret, so there is no second
	// string for the credential to disagree with.
	require.Len(t, findings, 1)
	assert.Equal(t, "requires.channels[0].name", findings[0].Path)
	assert.Contains(t, findings[0].Reason, "name is required")
}
