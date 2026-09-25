package slack

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelfeatures"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/wizardkeys"
)

// featureStubKind is the Slack kind with a different answer to one question:
// which features it can satisfy. Embedding *Kind rather than reimplementing the
// interface keeps the stub to the single method under test — a hand-written
// Kind would have to track every method the interface gains.
type featureStubKind struct {
	*Kind
	support map[channelfeatures.Feature]channelkinds.FeatureRequirement
}

func (k featureStubKind) FeatureSupport() map[channelfeatures.Feature]channelkinds.FeatureRequirement {
	return k.support
}

// optionNames returns the capability names of opts, in order.
func optionNames(opts []capabilityOption) []string {
	out := make([]string, 0, len(opts))
	for _, o := range opts {
		out = append(out, o.capability)
	}
	return out
}

// rawCapabilities renders a patch's spec.capabilities as name → raw JSON, so an
// assertion states the bytes that will be applied rather than a struct shape.
func rawCapabilities(t *testing.T, patch *unstructured.Unstructured) map[string]string {
	t.Helper()
	require.NotNil(t, patch, "a patch is required to read capabilities from")
	caps, found, err := unstructured.NestedMap(patch.Object, "spec", "capabilities")
	require.NoError(t, err, "spec.capabilities must be a map")
	require.True(t, found, "the patch must carry spec.capabilities")

	out := map[string]string{}
	for name, v := range caps {
		raw, err := json.Marshal(v)
		require.NoErrorf(t, err, "marshal capability %q", name)
		out[name] = string(raw)
	}
	return out
}

// sortedKeys returns the keys of a decoded JSON object, sorted, for asserting
// on the exact field set a payload carries.
func sortedKeys(obj map[string]json.RawMessage) []string {
	out := make([]string, 0, len(obj))
	for k := range obj {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// decodeObject unmarshals JSON into its top-level fields, undecoded.
func decodeObject(t *testing.T, raw []byte) map[string]json.RawMessage {
	t.Helper()
	var obj map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &obj), "decode JSON object")
	return obj
}

// TestCapabilityOptions_OffersOnlyWhatTheKindCanDeliver is the three-way
// intersection: a capability is offered only when the channelfeatures table
// lists it AND every feature it implies is one this kind supports.
func TestCapabilityOptions_OffersOnlyWhatTheKindCanDeliver(t *testing.T) {
	full := (&Kind{}).FeatureSupport()
	without := func(drop ...channelfeatures.Feature) map[channelfeatures.Feature]channelkinds.FeatureRequirement {
		out := map[channelfeatures.Feature]channelkinds.FeatureRequirement{}
		for f, req := range full {
			out[f] = req
		}
		for _, f := range drop {
			delete(out, f)
		}
		return out
	}

	t.Run("kind supports every feature: every table capability is offered", func(t *testing.T) {
		assert.ElementsMatch(t,
			[]string{"artifacts", "attachments", "channel_history", "credential_update", "directory_sync", "mention_lookup", "session_views", "thread_history"},
			optionNames(capabilityOptions(featureStubKind{support: full})),
			"the offered set is every capability in the channelfeatures table")
	})

	t.Run("kind cannot read inbound files: attachments is omitted, artifacts survives", func(t *testing.T) {
		got := optionNames(capabilityOptions(featureStubKind{support: without(channelfeatures.AttachmentsInbound)}))
		assert.NotContains(t, got, "attachments",
			"a capability needing a feature the kind lacks is omitted entirely, never offered degraded")
		assert.Contains(t, got, "artifacts",
			"a capability whose features are all supported is unaffected")
	})

	t.Run("kind with no permission model at all: nothing is offered", func(t *testing.T) {
		assert.Empty(t, capabilityOptions(featureStubKind{support: nil}),
			"a kind that satisfies no feature has no capability to offer")
	})
}

// TestCapabilityOptions_DescribeWhatEachCosts pins that the option text is
// derived from the kind's own FeatureSupport — the scopes it costs and what
// breaks without them — rather than written out beside it.
func TestCapabilityOptions_DescribeWhatEachCosts(t *testing.T) {
	byName := map[string]capabilityOption{}
	for _, o := range capabilityOptions(&Kind{}) {
		byName[o.capability] = o
	}

	att, ok := byName["attachments"]
	require.True(t, ok, "attachments must be offered by the Slack kind")
	assert.Equal(t, []string{"files:read", "files:write"}, att.scopes,
		"the scopes come from ScopesFor: deduplicated and sorted")
	require.Len(t, att.degrades, 2, "its two features break differently and both must be said")
	joined := strings.Join(att.degrades, " ")
	assert.Contains(t, joined, "could not be retrieved", "the inbound half's degradation")
	assert.Contains(t, joined, "delivery failed", "the outbound half's degradation")
	assert.Contains(t, att.label(), "files:read", "the checkbox says what it costs")

	views, ok := byName["session_views"]
	require.True(t, ok, "session_views must be offered by the Slack kind")
	assert.Empty(t, views.scopes, "session views cost no Slack scope")
	assert.Contains(t, views.label(), "no extra Slack scope",
		"a scope-free option must still say what it costs")

	portal, ok := byName["credential_update"]
	require.True(t, ok, "credential_update must be offered by the Slack kind")
	assert.Empty(t, portal.scopes, "the credential portal costs no scope")
	assert.Contains(t, portal.note(), "home_tab_enabled",
		"but it does cost a manifest setting, so its note must carry the Setup line rather than claim it is free")
}

// TestCapabilityOptions_AreStableAndSorted: the option list keys the checkbox
// numbering, the manifest's scope union and the AgentClass patch. Map iteration
// order leaking into it would make all three churn between runs.
func TestCapabilityOptions_AreStableAndSorted(t *testing.T) {
	first := optionNames(capabilityOptions(&Kind{}))
	second := optionNames(capabilityOptions(&Kind{}))
	require.NotEmpty(t, first, "the Slack kind offers capabilities; an empty list makes this vacuous")
	assert.Equal(t, first, second, "two calls must produce the same order")
	assert.IsIncreasing(t, first, "options are sorted by capability name")
}

// TestCapabilityPatch_EncodesEveryDefaultOnAndCheckedCombination is the
// highest-risk correctness item here, and the answer is the same in all four
// cells: an explicit value, never silence.
//
// "No key" is not a way to say anything to a server where someone else may
// already have written one. SSA removes only fields the applying manager
// itself previously owned, so an omitted key can neither clear another
// manager's {"enabled": false} nor revoke another manager's grant — and a
// bundle installed by `oap agent install` owns exactly those keys under a
// different manager. Both silences would show the user a changed checkbox and
// leave the capability as it was.
//
// So defaultOn does not appear in the encoding at all. It decides which box
// starts CHECKED (see the pre-check tests); it never decides what gets
// written.
func TestCapabilityPatch_EncodesEveryDefaultOnAndCheckedCombination(t *testing.T) {
	cases := []struct {
		name      string
		defaultOn bool
		checked   bool
		wantRaw   string
	}{
		{
			name:      "default-on and checked: an explicit enable, since absence cannot clear another manager's disable",
			defaultOn: true, checked: true, wantRaw: `{"enabled":true}`,
		},
		{
			name:      "default-on and unchecked: an explicit disable, since dropping the key would leave it on",
			defaultOn: true, checked: false, wantRaw: `{"enabled":false}`,
		},
		{
			name:      "opt-in and checked: an explicit enable",
			defaultOn: false, checked: true, wantRaw: `{"enabled":true}`,
		},
		{
			name:      "opt-in and unchecked: an explicit disable, since absence cannot revoke another manager's grant",
			defaultOn: false, checked: false, wantRaw: `{"enabled":false}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := []capabilityOption{{capability: "demo-cap", defaultOn: tc.defaultOn}}
			var checked []string
			if tc.checked {
				checked = []string{"demo-cap"}
			}

			got := rawCapabilities(t, capabilityPatch("demo-ns", "demo-agent", opts, checked))
			assert.Equal(t, map[string]string{"demo-cap": tc.wantRaw}, got,
				"the offered capability, stated outright, and nothing else")
		})
	}
}

// TestCapabilityPatch_CarriesNothingTheWizardDidNotDecide: the CLI applies this
// with Force, so every field the payload carries is a field the wizard's field
// manager takes ownership of. A typed AgentClass would serialize
// spec.systemPrompt — a struct value with no omitempty — and hand the wizard
// ownership of somebody else's system prompt on every channel create.
func TestCapabilityPatch_CarriesNothingTheWizardDidNotDecide(t *testing.T) {
	patch := capabilityPatch("demo-ns", "demo-agent", capabilityOptions(&Kind{}), []string{"attachments"})
	require.NotNil(t, patch)

	raw, err := json.Marshal(patch)
	require.NoError(t, err, "marshal the apply payload")
	doc := decodeObject(t, raw)

	assert.Equal(t, []string{"apiVersion", "kind", "metadata", "spec"}, sortedKeys(doc),
		"the payload names the object and carries one spec field; anything else would be seized on apply")
	assert.Equal(t, []string{"capabilities"}, sortedKeys(decodeObject(t, doc["spec"])),
		"spec must carry only the field the wizard decided")
	assert.Equal(t, []string{"name", "namespace"}, sortedKeys(decodeObject(t, doc["metadata"])),
		"metadata identifies the object and claims nothing else")
	assert.NotContains(t, string(raw), "systemPrompt",
		"the wizard decided nothing about the system prompt, so it must not send one")
	assert.NotContains(t, string(raw), "status",
		"an applied payload never carries status")
}

// TestCapabilityPatch_IsIdenticalWhenBuiltTwice: the CLI server-side-applies
// this patch under its own field manager, so a byte-identical re-apply has to
// be a no-op. Anything volatile reaching the map — a timestamp, a counter, map
// iteration order — would make every re-run rewrite the object.
func TestCapabilityPatch_IsIdenticalWhenBuiltTwice(t *testing.T) {
	opts := capabilityOptions(&Kind{})
	checked := []string{"attachments", "channel_history"}

	first := capabilityPatch("demo-ns", "demo-agent", opts, checked)
	second := capabilityPatch("demo-ns", "demo-agent", opts, checked)
	assert.Equal(t, first, second, "two builds from the same answers must be deeply equal")

	firstJSON, err := json.Marshal(first)
	require.NoError(t, err, "marshal first patch")
	secondJSON, err := json.Marshal(second)
	require.NoError(t, err, "marshal second patch")
	assert.Equal(t, string(firstJSON), string(secondJSON), "and byte-identical once serialized")
}

// TestCapabilityPatch_TargetsTheBoundAgentClass: an apply needs the object's
// identity and its type, and the CLI resolves the GVR from them.
func TestCapabilityPatch_TargetsTheBoundAgentClass(t *testing.T) {
	got := capabilityPatch("demo-ns", "demo-agent", capabilityOptions(&Kind{}), []string{"attachments"})
	require.NotNil(t, got)
	assert.Equal(t, "demo-agent", got.GetName(), "the AgentClass the run bound to")
	assert.Equal(t, "demo-ns", got.GetNamespace(), "the namespace the run was given")
	assert.Equal(t, "AgentClass", got.GetKind(), "an apply needs the kind")
	assert.Equal(t, "agentprimitives.authzed.com/v1alpha1", got.GetAPIVersion(), "and the API version")
}

// capabilityAnswer is the capability answer a run produces after TOGGLING the
// named capabilities away from what the boxes started out as, spelled the way
// `--answer capabilities=a,b` and the multi-select both record it.
//
// The starting set is read from activeCapabilities against the bound class, so
// a change to what a class already grants — or to which capabilities are
// default-on — moves this fixture with it rather than leaving it asserting a
// stale set.
func capabilityAnswer(t *testing.T, class *spiceboxv1alpha1.AgentClass, toggle ...string) string {
	t.Helper()
	opts := slackCapabilityOptions()
	checked, err := activeCapabilities(opts, class)
	require.NoError(t, err)

	on := map[string]bool{}
	for _, c := range checked {
		on[c] = true
	}
	for _, c := range toggle {
		require.Containsf(t, optionNames(opts), c, "cannot toggle %q: this kind does not offer it", c)
		on[c] = !on[c]
	}

	var out []string
	for _, o := range opts {
		if on[o.capability] {
			out = append(out, o.capability)
		}
	}
	return strings.Join(out, ",")
}

// agentAnswers is a complete agent-flow answer map with the capability answer
// toggled, for the cases whose subject is what the capability choice implies.
func agentAnswers(t *testing.T, class *spiceboxv1alpha1.AgentClass, channelName string, toggle ...string) map[string]string {
	t.Helper()
	answers := typedAgentAnswers(t, channelName)
	answers[keyAgentClass] = class.Name
	answers[keyCapabilities] = capabilityAnswer(t, class, toggle...)
	return answers
}

// TestWizard_Capabilities_PreChecksFromTheBoundAgentClass: the boxes start out
// as what the agent can already do, resolved the way every runtime reader
// resolves it — so a default-on capability with no explicit grant shows
// checked, and confirming without touching anything leaves the class as it was.
func TestWizard_Capabilities_PreChecksFromTheBoundAgentClass(t *testing.T) {
	class := newAgentClassWithCaps("demo-agent", "default", map[string]string{
		"attachments":    `{}`,
		"thread_history": `{"enabled":false}`,
	})
	in := defaultInput(class)

	out, err := resolvedAgentRun(t, newWizardWithStub(okAuth(), nil), in,
		agentAnswers(t, class, "my-channel"))
	require.NoError(t, err, "wizard run")

	assert.Equal(t, map[string]string{
		"artifacts":         `{"enabled":false}`,
		"attachments":       `{"enabled":true}`,
		"channel_history":   `{"enabled":true}`,
		"credential_update": `{"enabled":false}`,
		"directory_sync":    `{"enabled":false}`,
		"mention_lookup":    `{"enabled":true}`,
		"session_views":     `{"enabled":false}`,
		"thread_history":    `{"enabled":false}`,
	}, rawCapabilities(t, out.CapabilityPatch),
		"confirming the pre-checked boxes must re-state exactly the grants the class already had — the two it names explicitly, and the defaults for the rest")
}

// TestWizard_Capabilities_UncheckingRevokesAGrantAnotherManagerWrote is the
// hole a "write only what differs from the default" patch would leave open, and
// the worse half of it: a revocation that silently does not happen.
//
// attachments is opt-in, and this class already has it granted — by a bundle
// `oap agent install` applied under ITS field manager, in the case this guards.
// The wizard's manager has never owned that key, so an apply that merely omits
// it removes nothing: the box would go blank and the capability would stay on.
func TestWizard_Capabilities_UncheckingRevokesAGrantAnotherManagerWrote(t *testing.T) {
	class := newAgentClassWithCaps("demo-agent", "default", map[string]string{"attachments": `{}`})

	out, err := resolvedAgentRun(t, newWizardWithStub(okAuth(), nil), defaultInput(class),
		agentAnswers(t, class, "my-channel", "attachments"))
	require.NoError(t, err, "wizard run")

	assert.Equal(t, `{"enabled":false}`, rawCapabilities(t, out.CapabilityPatch)["attachments"],
		"unchecking a granted opt-in must write an explicit disable; omitting the key cannot revoke a grant this field manager never owned")
	assert.NotContains(t, strings.Join(out.Notes, "\n"), "files:read",
		"and the revoked capability's caveat must not be printed either")
}

// TestWizard_Capabilities_UncheckingADefaultOnCapabilityDisablesIt is that same
// subtlety end to end: unchecking thread_history has to reach the AgentClass as
// an explicit disable, because omitting the key means ON.
func TestWizard_Capabilities_UncheckingADefaultOnCapabilityDisablesIt(t *testing.T) {
	class := newAgentClass("demo-agent", "default")

	out, err := resolvedAgentRun(t, newWizardWithStub(okAuth(), nil), defaultInput(class),
		agentAnswers(t, class, "my-channel", "thread_history"))
	require.NoError(t, err, "wizard run")

	assert.Equal(t, `{"enabled":false}`, rawCapabilities(t, out.CapabilityPatch)["thread_history"],
		"unchecking a default-on capability must write an explicit disable")
}

// TestWizard_Capabilities_CheckingAnOptInGrantsIt is the opposite direction: an
// opt-in capability is off until the key exists, so checking one must write it
// — and nothing else may be written on its account.
func TestWizard_Capabilities_CheckingAnOptInGrantsIt(t *testing.T) {
	class := newAgentClass("demo-agent", "default")

	out, err := resolvedAgentRun(t, newWizardWithStub(okAuth(), nil), defaultInput(class),
		agentAnswers(t, class, "my-channel", "attachments"))
	require.NoError(t, err, "wizard run")

	got := rawCapabilities(t, out.CapabilityPatch)
	assert.Equal(t, `{"enabled":true}`, got["attachments"], "checking an opt-in must grant it")
	assert.Equal(t, `{"enabled":false}`, got["artifacts"],
		"an opt-in left unchecked is stated off, not left to whatever the object already said")
	assert.Equal(t, `{"enabled":true}`, got["channel_history"],
		"a default-on capability left checked is stated on for the same reason")
}

// TestWizard_ManifestRequestsOnlyTheScopesChosen is the visible half of the
// complaint the capability question exists for: the generated app manifest asks
// for the permissions the answers imply, not for every permission any agent
// could need.
//
// Driven through the manual route, which is the one that hands the manifest
// over, and read back out of the FILE it left behind as well as the message —
// the two must agree, and the file is what the operator still has once the
// terminal has scrolled away.
func TestWizard_ManifestRequestsOnlyTheScopesChosen(t *testing.T) {
	cases := []struct {
		name       string
		toggle     []string
		wantScopes []string
		denyScopes []string
	}{
		{
			name:       "defaults only: no file scopes are requested",
			wantScopes: []string{"assistant:write", "channels:history", "users:read"},
			denyScopes: []string{"files:read", "files:write"},
		},
		{
			name:       "attachments chosen: both file scopes are requested",
			toggle:     []string{"attachments"},
			wantScopes: []string{"files:read", "files:write"},
		},
		{
			name:       "both history capabilities unchecked: the history scopes drop out",
			toggle:     []string{"thread_history", "channel_history"},
			denyScopes: []string{"channels:history", "groups:history", "im:history"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			class := newAgentClass("demo-agent", "default")
			dir := t.TempDir()
			in := defaultInput(class)
			in.WorkingDir = dir

			_, err := newWizardWithStub(okAuth(), nil).Resolve(context.Background(), in, map[string]string{
				keyAgentClass:   "demo-agent",
				keyHasSlackApp:  string(routeManual),
				keyCapabilities: capabilityAnswer(t, class, tc.toggle...),
			})
			require.Error(t, err, "the manual route hands the manifest over as the message that ends the run")

			manifest := readSavedManifest(t, dir, "demo-agent")
			assert.Contains(t, err.Error(), manifest,
				"the saved manifest must be the one the message carried")
			for _, scope := range tc.wantScopes {
				assert.Contains(t, manifest, scope, "the manifest must request a scope the answers need")
			}
			for _, scope := range tc.denyScopes {
				assert.NotContains(t, manifest, scope, "the manifest must not request a scope nothing chosen needs")
			}
			assert.Contains(t, manifest, "chat:write", "the baseline is requested whatever is chosen")
		})
	}
}

// readSavedManifest reads the manifest copy a run left in dir, failing if it is
// not there — the file is the whole point of the manual route, so its absence
// is a failure rather than an empty string to assert against.
func readSavedManifest(t *testing.T, dir, agentClass string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "slack-app-manifest-"+agentClass+".yaml"))
	require.NoError(t, err, "the run must leave the manifest beside it")
	return string(raw)
}

// TestWizard_PostSetupNotesFollowTheSelection: the caveats printed after the
// apply are the ones the answers earned, and ONLY the ones nothing else
// reports.
//
// Choosing attachments must NOT print "files:read" here: `oap channel create`
// watches the Channel it applied and surfaces channelsd's ScopesValid=False,
// which names the scopes ACTUALLY missing from the app rather than every scope
// the choice implies — a static list would be a worse copy of that report. What
// a note is still for is the case no condition can observe: a capability whose
// cost is a manifest field or an event subscription, not a scope.
func TestWizard_PostSetupNotesFollowTheSelection(t *testing.T) {
	class := newAgentClass("demo-agent", "default")
	in := defaultInput(class)

	defaults, err := resolvedAgentRun(t, newWizardWithStub(okAuth(), nil), in,
		agentAnswers(t, class, "my-channel"))
	require.NoError(t, err, "wizard run")
	assert.NotContains(t, strings.Join(defaults.Notes, "\n"), "files:read",
		"a run that did not choose attachments must not be warned about the file scopes")

	withFiles, err := resolvedAgentRun(t, newWizardWithStub(okAuth(), nil), in,
		agentAnswers(t, class, "my-channel", "attachments"))
	require.NoError(t, err, "wizard run")
	assert.NotContains(t, strings.Join(withFiles.Notes, "\n"), "files:read",
		"a scope gap is what the watch reports, from the live app rather than from the choice")

	withPortal, err := resolvedAgentRun(t, newWizardWithStub(okAuth(), nil), in,
		agentAnswers(t, class, "my-channel", "credential_update"))
	require.NoError(t, err, "wizard run")
	assert.Contains(t, strings.Join(withPortal.Notes, "\n"), "home_tab_enabled",
		"credential_update costs a manifest field and an event, not a scope — no condition can observe it, so these notes are the only place it is said")
}

// TestWizard_Capabilities_RefusesACapabilityThisKindCannotEnable: the checked
// set is written verbatim onto an AgentClass, so an answer naming something
// outside the offered set must be refused rather than granted.
func TestWizard_Capabilities_RefusesACapabilityThisKindCannotEnable(t *testing.T) {
	answers := typedAgentAnswers(t, "seeded-channel")
	answers[keyCapabilities] = "no-such-capability"

	_, err := resolvedAgentRun(t, newWizardWithStub(okAuth(), nil),
		defaultInput(newAgentClass("demo-agent", "default")), answers)
	require.Error(t, err, "an unofferable capability must stop the run")
	assert.Contains(t, err.Error(), "no-such-capability", "the error must name it")
}

// TestWizard_ResultRefusesAnUnansweredCapabilityAnswer: reading an ABSENT
// answer as "everything unchecked" would silently disable every default-on
// capability on the bound agent. An answer that is present and empty is a
// different thing — the operator unchecked everything — and must be honored.
func TestWizard_ResultRefusesAnUnansweredCapabilityAnswer(t *testing.T) {
	in := defaultInput(newAgentClass("demo-agent", "default"))

	absent := typedAgentAnswers(t, "my-channel")
	delete(absent, keyCapabilities)
	_, err := resolvedAgentRun(t, newWizardWithStub(okAuth(), nil), in, absent)
	require.Error(t, err, "Result must refuse an answer map the capability question never answered")
	assert.Contains(t, err.Error(), keyCapabilities, "the error must name the missing answer")

	emptied := typedAgentAnswers(t, "my-channel")
	emptied[keyCapabilities] = ""
	out, err := resolvedAgentRun(t, newWizardWithStub(okAuth(), nil), in, emptied)
	require.NoError(t, err, "unchecking everything is an answer, not an omission")
	for name, raw := range rawCapabilities(t, out.CapabilityPatch) {
		assert.Equalf(t, `{"enabled":false}`, raw, "capability %q must be stated off", name)
	}
}

// TestWizard_MonitoringHasNoCapabilityPatch: a monitoring Channel is bound to
// no AgentClass, so there is nothing to patch — and no capability question to
// ask.
func TestWizard_MonitoringHasNoCapabilityPatch(t *testing.T) {
	in := monitoringWizardInput()

	qs, err := kindWizard(t).Inputs(context.Background(), in)
	require.NoError(t, err)
	for _, q := range qs {
		assert.NotEqual(t, keyCapabilities, q.Name, "the monitoring flow asks no capability question")
	}

	out, err := kindWizard(t).Result(in, map[string]string{
		keyHasSlackApp:            string(routeHave),
		keyBotToken:               validBotToken,
		keyDestinationChannelID:   "C0123ABCDEF",
		wizardkeys.KeyChannelName: "slack-monitoring-demo",
		keyBotUserID:              "U0DEMO123",
	})
	require.NoError(t, err, "wizard run")
	require.NotNil(t, out.ChannelManifest, "the monitoring flow still produces a Channel")
	assert.Nil(t, out.CapabilityPatch, "a flow with no AgentClass produces no patch")
}
