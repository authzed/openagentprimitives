package channelcmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/aptest"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/channelwizard"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/kube"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/bento"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/fake"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/wizardkeys"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/wizardrun"
	"github.com/authzed/openagentprimitives/pkg/cli/tui"
	"github.com/authzed/openagentprimitives/pkg/platform/oap/install"
)

// channelGVR is the dynamic-client GVR for Channel, matching kube.Apply's
// CRD-derived pluralization.
var channelGVR = schema.GroupVersionResource{
	Group:    "agentprimitives.authzed.com",
	Version:  "v1alpha1",
	Resource: "channels",
}

// agentClassGVR is where the wizard's capability patch lands.
var agentClassGVR = schema.GroupVersionResource{
	Group:    "agentprimitives.authzed.com",
	Version:  "v1alpha1",
	Resource: "agentclasses",
}

var secretGVR = schema.GroupVersionResource{Group: "", Version: "v1", Resource: "secrets"}

// newFakeBundle is aptest.NewFakeBundle over the three GVRs a channel-create
// run applies through the dynamic client.
func newFakeBundle(t *testing.T, objs ...client.Object) (*kube.Bundle, *dynfake.FakeDynamicClient) {
	t.Helper()
	return aptest.NewFakeBundle(t, map[schema.GroupVersionResource]string{
		channelGVR:    "ChannelList",
		agentClassGVR: "AgentClassList",
		secretGVR:     "SecretList",
	}, objs...)
}

// TestChannelCreateFakeWizardEmitsManifests: --apply=false prints the manifests
// a run would have applied, without needing a cluster.
//
// It also exercises DriverFor: runChannelCreate never injects a tui.Driver, so
// tui.Run resolves its own (and assembles the chrome from the title + screens)
// on the way through. Nothing else in the repo takes that path.
func TestChannelCreateFakeWizardEmitsManifests(t *testing.T) {
	var out bytes.Buffer
	g := &apcmd.Globals{Namespace: "default"}
	require.NoError(t,
		runChannelCreate(context.Background(), strings.NewReader(""), &out, g,
			channelCreateOptions{kind: "fake"}),
		"runChannelCreate")

	s := out.String()
	assert.Contains(t, s, "kind: Channel", "Channel kind in output")
	// The fake wizard's SecretManifest lacks TypeMeta (no kind: Secret in YAML),
	// so we assert the section header and the secret name instead.
	assert.Contains(t, s, "--- Secret ---", "Secret section in output")
	assert.Contains(t, s, "fake-creds", "secret name 'fake-creds' in output")
}

// TestChannelCreateRendersRunSummary: what a run decided must land in the
// user's scrollback as the tui summary, not as a "note:" debug line.
func TestChannelCreateRendersRunSummary(t *testing.T) {
	var out bytes.Buffer
	g := &apcmd.Globals{Namespace: "default"}
	require.NoError(t,
		runChannelCreate(context.Background(), strings.NewReader(""), &out, g,
			channelCreateOptions{kind: "fake"}))

	s := out.String()
	assert.Contains(t, s, "fake-channel (test fixture)", "the screen's summary note")
	assert.NotContains(t, s, "note: ", "the old unconditional debug prefix must be gone")
}

// TestWizardTitle pins the chrome title the dispatcher hands tui.Run. The rail
// itself is only drawn on a TTY, so this is the one assertion available for it
// off-terminal.
func TestWizardTitle(t *testing.T) {
	assert.Equal(t, "oap · channel create · slack", wizardTitle("slack"))
}

// TestChannelWizardApply_AppliesCapabilityPatch: the AgentClass capability patch
// is a separate object under a separate field manager, and applying it is the
// whole point of the capability screen — a dispatcher that dropped it would
// leave the agent without the capabilities the user just checked.
func TestChannelWizardApply_AppliesCapabilityPatch(t *testing.T) {
	ctx := context.Background()
	b, dyn := newFakeBundle(t)

	wizOut := channelkinds.WizardOutput{
		SecretManifest: &corev1.Secret{
			TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
			ObjectMeta: metav1.ObjectMeta{Name: "slack-helpdesk-creds", Namespace: "default"},
		},
		ChannelManifest: &spiceboxv1alpha1.Channel{
			TypeMeta:   metav1.TypeMeta{APIVersion: "agentprimitives.authzed.com/v1alpha1", Kind: "Channel"},
			ObjectMeta: metav1.ObjectMeta{Name: "slack-helpdesk", Namespace: "default"},
		},
		CapabilityPatch: &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "agentprimitives.authzed.com/v1alpha1",
			"kind":       "AgentClass",
			"metadata":   map[string]any{"name": "helpdesk", "namespace": "default"},
			"spec": map[string]any{"capabilities": map[string]any{
				"attachments": map[string]any{"enabled": true},
			}},
		}},
	}
	require.NoError(t, channelwizard.Apply(ctx, b, wizOut), "channelwizard.Apply")

	got, err := dyn.Resource(agentClassGVR).Namespace("default").Get(ctx, "helpdesk", metav1.GetOptions{})
	require.NoError(t, err, "the capability patch must have been applied to the AgentClass")

	caps, found, err := unstructured.NestedMap(got.Object, "spec", "capabilities")
	require.NoError(t, err)
	require.True(t, found, "spec.capabilities must be present")
	assert.Equal(t, map[string]any{"attachments": map[string]any{"enabled": true}}, caps)

	spec, _, err := unstructured.NestedMap(got.Object, "spec")
	require.NoError(t, err)
	assert.Equal(t, []string{"capabilities"}, slices.Sorted(maps.Keys(spec)),
		"the patch must claim spec.capabilities and nothing else")
}

// TestChannelWizardFieldManagersAreDistinct: the capability patch lands on an
// AgentClass no channel wizard owns, so it must not share a field manager with
// the objects a wizard does own (the Channel and its Secret), nor with the
// agent installer's. Both commands that drive a wizard apply under the same
// two managers, so this holds for `oap agent install` as well.
func TestChannelWizardFieldManagersAreDistinct(t *testing.T) {
	assert.NotEqual(t, wizardrun.FieldManager, wizardrun.CapabilityFieldManager,
		"the AgentClass patch must not share a manager with the Channel/Secret apply")
	assert.NotEqual(t, install.FieldManager, wizardrun.CapabilityFieldManager,
		"the AgentClass patch must not claim fields under the agent installer's manager")
	assert.NotEqual(t, install.FieldManager, wizardrun.FieldManager,
		"the Channel/Secret apply must not claim fields under the agent installer's manager")
}

// TestChannelWizardApply_RefusesExistingChannel: a server-side apply of a
// Channel whose name already exists would silently overwrite it. channelwizard.Apply
// must fail closed instead.
func TestChannelWizardApply_RefusesExistingChannel(t *testing.T) {
	existing := &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Name: "slack-helpdesk", Namespace: "default"},
	}
	b, _ := newFakeBundle(t, existing)

	wizOut := channelkinds.WizardOutput{
		SecretManifest: &corev1.Secret{
			TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
			ObjectMeta: metav1.ObjectMeta{Name: "slack-helpdesk-creds", Namespace: "default"},
		},
		ChannelManifest: &spiceboxv1alpha1.Channel{
			TypeMeta:   metav1.TypeMeta{APIVersion: "agentprimitives.authzed.com/v1alpha1", Kind: "Channel"},
			ObjectMeta: metav1.ObjectMeta{Name: "slack-helpdesk", Namespace: "default"},
		},
	}

	err := channelwizard.Apply(context.Background(), b, wizOut)
	require.Error(t, err, "must refuse to overwrite an existing Channel")
	assert.Contains(t, err.Error(), "already exists")
	assert.Contains(t, err.Error(), "slack-helpdesk")
}

// TestChannelWizardApply_ReplaceExistingAllowsUpdate: with ReplaceExisting set,
// channelwizard.Apply updates the existing Channel instead of refusing.
//
// A nil kube.Bundle.Dynamic would panic inside kube.Apply (dyn.Resource(...)
// on a true nil dynamic.Interface dereferences a nil method table), so this
// test wires a real fake dynamic client and asserts the apply actually
// succeeds and lands the Channel — not just that the pre-check didn't fire.
func TestChannelWizardApply_ReplaceExistingAllowsUpdate(t *testing.T) {
	ctx := context.Background()
	existing := &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Name: "slack-monitoring-acme", Namespace: "default"},
		Spec:       spiceboxv1alpha1.ChannelSpec{Kind: "slack", Role: spiceboxv1alpha1.ChannelRoleMonitoring},
	}
	b, dyn := newFakeBundle(t, existing)

	wizOut := channelkinds.WizardOutput{
		ChannelManifest: &spiceboxv1alpha1.Channel{
			TypeMeta:   metav1.TypeMeta{APIVersion: "agentprimitives.authzed.com/v1alpha1", Kind: "Channel"},
			ObjectMeta: metav1.ObjectMeta{Name: "slack-monitoring-acme", Namespace: "default"},
			Spec:       spiceboxv1alpha1.ChannelSpec{Kind: "slack", Role: spiceboxv1alpha1.ChannelRoleMonitoring},
		},
		SecretManifest:  nil, // keep existing creds
		ReplaceExisting: true,
	}
	require.NoError(t, channelwizard.Apply(ctx, b, wizOut),
		"ReplaceExisting must bypass the refuse-to-overwrite guard and apply cleanly")

	got, getErr := dyn.Resource(channelGVR).Namespace("default").Get(ctx, "slack-monitoring-acme", metav1.GetOptions{})
	require.NoError(t, getErr, "the Channel should have been applied to the dynamic client")
	assert.Equal(t, "slack-monitoring-acme", got.GetName())
}

// TestChannelCreate_NonInteractiveFailsClosed: with no seeded answers, a
// --non-interactive run must refuse at the first question and apply nothing.
// Anything else — prompting, or defaulting the answer — would create a Channel
// nobody described.
func TestChannelCreate_NonInteractiveFailsClosed(t *testing.T) {
	ctx := context.Background()
	// An AgentClass to bind to, so the first screen has a real question to ask
	// and the refusal comes from the fail-closed driver rather than from a
	// namespace with nothing in it.
	b, dyn := newFakeBundle(t, &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-agent", Namespace: "default"},
	})
	var out bytes.Buffer

	err := runChannelCreate(ctx, aptest.RefusingReader{T: t}, &out, aptest.GlobalsFor(b), channelCreateOptions{
		kind:           "bento",
		apply:          true,
		nonInteractive: true,
	})
	require.Error(t, err, "an unseeded --non-interactive run must fail")
	assert.ErrorIs(t, err, tui.ErrUnanswered)
	// bento answers channelkinds.Wizard: the unanswered "screen" here is
	// questionscreen.NewScreen's rendering of Inputs' first question, whose ID
	// is the Question's Name ("agentclass") rather than the old
	// agentClassScreen's own ID ("agent") — see bento/wizard.go's Inputs.
	assert.Contains(t, err.Error(), `"agentclass"`, "the error must name the question that has no answer")
	assert.NotContains(t, err.Error(), "tui: ", "the sequencer's own framing is not for users")

	list, listErr := dyn.Resource(channelGVR).Namespace("default").List(ctx, metav1.ListOptions{})
	require.NoError(t, listErr)
	assert.Empty(t, list.Items, "a refused run must not have applied a Channel")
}

// TestChannelCreate_NonInteractiveFullySeededApplies: every answer supplied up
// front means no question is asked and the manifests land. Asserted on the
// applied objects, never on err == nil: huh's accessible renderer cannot report
// a read error, so a run that silently answered nothing also returns nil.
//
// The AgentClass the run names has to EXIST on the cluster, seeded or not —
// wizardkeys.AgentClassQuestion verifies a seeded binding against the listing
// rather than taking the flag's word for it, because a Channel bound to an
// AgentClass that is not there lints clean and never works. This test used to
// pass an empty cluster, which is the leniency that convergence removed.
func TestChannelCreate_NonInteractiveFullySeededApplies(t *testing.T) {
	ctx := context.Background()
	// TWO AgentClasses, "alt-agent" sorting first, so the bound one is not
	// also the only one: a fully-seeded run has to work in a namespace that
	// holds more than what it binds to, and a single class whose name happens
	// to equal the seed cannot show that.
	//
	// WHAT THIS TEST CANNOT PIN, stated so the next reader does not assume it
	// does: that the SEED rather than classes[0] is what the run bound to.
	// spec.agentClass is read out of the answer map, which the flag filled
	// directly, so it says "demo-agent" either way. The only values derived
	// from the resolved class are the Channel name and the authzSubject
	// defaults — and a --non-interactive run cannot reach a default at all
	// (the fail-closed driver refuses any question it is shown, Default or
	// not: "screen \"authzsubject\" has no answer"), so a fully-seeded run
	// necessarily supplies both. That discrimination lives one level down, in
	// wizardkeys.TestAgentClassQuestion's seeded-and-present row, which seeds
	// the SECOND of two classes precisely so classes[0] cannot satisfy it.
	b, dyn := newFakeBundle(t,
		&spiceboxv1alpha1.AgentClass{
			ObjectMeta: metav1.ObjectMeta{Name: "alt-agent", Namespace: "default"},
		},
		&spiceboxv1alpha1.AgentClass{
			ObjectMeta: metav1.ObjectMeta{Name: "demo-agent", Namespace: "default"},
		})
	var out bytes.Buffer

	require.NoError(t, runChannelCreate(ctx, aptest.RefusingReader{T: t}, &out, aptest.GlobalsFor(b), channelCreateOptions{
		kind:           "bento",
		apply:          true,
		nonInteractive: true,
		name:           "nightly-trigger",
		answers: []string{
			"agentclass=demo-agent",
			"authzsubject=service:demo-bot",
			"interval=@every 24h",
			"mapping=root = \"nightly\"",
		},
	}), "a fully seeded --non-interactive run must complete")

	got, err := dyn.Resource(channelGVR).Namespace("default").Get(ctx, "nightly-trigger", metav1.GetOptions{})
	require.NoError(t, err, "the Channel must have been applied under the seeded name")

	assertNested := func(want string, fields ...string) {
		t.Helper()
		v, found, nerr := unstructured.NestedString(got.Object, fields...)
		require.NoError(t, nerr)
		require.True(t, found, "%v must be set", fields)
		assert.Equal(t, want, v)
	}
	assertNested("bento", "spec", "kind")
	assertNested("demo-agent", "spec", "agentClass")
	assertNested("service:demo-bot", "spec", "authzSubject")
	assertNested("@every 24h", "spec", "bento", "generate", "interval")
	assertNested(`root = "nightly"`, "spec", "bento", "generate", "mapping")

	_, secErr := dyn.Resource(secretGVR).Namespace("default").Get(ctx, "nightly-trigger-creds", metav1.GetOptions{})
	require.NoError(t, secErr, "the credentials Secret must have been applied too")
}

// TestChannelCreate_AnswerFlagParsing: a malformed --answer is refused before
// anything runs, rather than being silently ignored.
func TestChannelCreate_AnswerFlagParsing(t *testing.T) {
	cases := []struct {
		name     string
		answers  []string
		seedName string
		wantErr  string
	}{
		{
			name:    "no equals sign: refused naming the offending value",
			answers: []string{"agentclass"},
			wantErr: "key=value",
		},
		{
			name:    "empty key: refused",
			answers: []string{"=value"},
			wantErr: "key=value",
		},
		{
			name:     "--name disagreeing with --answer name=: refused rather than guessed",
			answers:  []string{"name=other"},
			seedName: "chosen",
			wantErr:  "two different names",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := seedState(channelCreateOptions{answers: tc.answers, name: tc.seedName})
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

// TestSeedStateShapes: State holds strings, bools and lists in separate maps,
// and a screen reads exactly one of them. A flag-supplied answer must satisfy
// whichever the screen asks for, or a seeded run fails closed on an answer that
// was in fact supplied.
func TestSeedStateShapes(t *testing.T) {
	st, _, err := seedState(channelCreateOptions{answers: []string{
		"slackapp=true",
		"capabilities=attachments,threads",
		"agentclass=demo-agent",
		"empty=",
	}})
	require.NoError(t, err)

	assert.True(t, st.Bool("slackapp"), "a boolean answer must be readable as a bool")
	assert.Equal(t, []string{"attachments", "threads"}, st.All("capabilities"),
		"a comma-separated answer must be readable as a list")
	assert.Equal(t, "demo-agent", st.Get("agentclass"))
	assert.True(t, st.Has("empty"), `an explicitly empty answer is "answered with nothing", not "never asked"`)
	assert.Empty(t, st.All("empty"), "an empty answer is an empty list, not a list holding one blank")
}

// TestSeedStateMultiValueSpacing: a space after a comma is what anyone
// separating a list actually types, and the values are matched against
// untrimmed capability constants downstream — so an untrimmed " threads" is
// not a rejected answer, it is a capability silently left off the AgentClass
// with no error anywhere.
func TestSeedStateMultiValueSpacing(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want []string
	}{
		{name: "no spaces: both values", raw: "capabilities=attachments,threads", want: []string{"attachments", "threads"}},
		{name: "space after the comma: both values, neither padded", raw: "capabilities=attachments, threads", want: []string{"attachments", "threads"}},
		{name: "spaces around every value: both values", raw: "capabilities= attachments , threads ", want: []string{"attachments", "threads"}},
		{name: "trailing comma: harmless, not an empty capability", raw: "capabilities=attachments,", want: []string{"attachments"}},
		{name: "only separators: an empty answer, not a list of blanks", raw: "capabilities= , ", want: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st, _, err := seedState(channelCreateOptions{answers: []string{tc.raw}})
			require.NoError(t, err)
			assert.Equal(t, tc.want, st.All("capabilities"))
			assert.True(t, st.Has("capabilities"),
				`an answer given must stay "answered", even when it resolves to nothing`)
		})
	}
}

// TestSeedStateNameSeedsTheSharedKey: --name must reach the shared
// ChannelName screen's key, or the flag pre-fills nothing.
func TestSeedStateNameSeedsTheSharedKey(t *testing.T) {
	st, _, err := seedState(channelCreateOptions{name: "team-alerts"})
	require.NoError(t, err)
	assert.Equal(t, "team-alerts", st.Get(wizardkeys.KeyChannelName))
}

func TestFindExistingMonitoringChannel(t *testing.T) {
	scheme := aptest.Scheme(t)
	agent := &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Name: "slack-helpdesk", Namespace: "default"},
		Spec:       spiceboxv1alpha1.ChannelSpec{Kind: "slack", AgentClass: "helpdesk"},
	}

	cases := []struct {
		name string
		objs []client.Object
		want string // "" means nil
	}{
		{name: "one monitoring channel: returned", objs: []client.Object{monitoringChan("m1", "slack"), agent}, want: "m1"},
		{name: "none: nil", objs: []client.Object{agent}, want: ""},
		{name: "two monitoring channels: nil (do not guess)", objs: []client.Object{monitoringChan("m1", "slack"), monitoringChan("m2", "slack")}, want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tc.objs...).Build()
			got, err := findExistingMonitoringChannel(context.Background(), c, "default", "slack")
			require.NoError(t, err)
			if tc.want == "" {
				assert.Nil(t, got)
			} else {
				require.NotNil(t, got)
				assert.Equal(t, tc.want, got.Name)
			}
		})
	}
}

// TestResolveMonitoringExisting covers the plan's hazard 5: the role scan picks
// a monitoring Channel of the kind REGARDLESS of name, and the wizard's name
// screen refuses a seeded name that differs from the Channel it was handed. A
// requested name must therefore win over the scan, or a second monitoring
// Channel could never be created once a first one existed.
func TestResolveMonitoringExisting(t *testing.T) {
	scheme := aptest.Scheme(t)
	agentChan := &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Name: "taken", Namespace: "default"},
		Spec:       spiceboxv1alpha1.ChannelSpec{Kind: "slack", AgentClass: "helpdesk"},
	}

	cases := []struct {
		name     string
		objs     []client.Object
		wantName string
		want     string // resolved Channel name; "" means nil
		wantErr  string
	}{
		{
			name: "no name requested, one monitoring channel: falls back to the role scan",
			objs: []client.Object{monitoringChan("mon-a", "slack")},
			want: "mon-a",
		},
		{
			name:     "name matches an existing monitoring channel: reconfigures that one",
			objs:     []client.Object{monitoringChan("mon-a", "slack"), monitoringChan("mon-b", "slack")},
			wantName: "mon-b",
			want:     "mon-b",
		},
		{
			name:     "new name while another monitoring channel exists: creates a second, not a refusal",
			objs:     []client.Object{monitoringChan("mon-a", "slack")},
			wantName: "mon-b",
			want:     "",
		},
		{
			name:     "name belongs to a non-monitoring Channel: refused, never adopted",
			objs:     []client.Object{agentChan},
			wantName: "taken",
			wantErr:  "not a slack monitoring channel",
		},
		{
			name:     "name belongs to a monitoring Channel of another kind: refused",
			objs:     []client.Object{monitoringChan("mon-a", "bento")},
			wantName: "mon-a",
			wantErr:  "not a slack monitoring channel",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tc.objs...).Build()
			got, err := resolveMonitoringExisting(context.Background(), c, "default", "slack", tc.wantName)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
				return
			}
			require.NoError(t, err)
			if tc.want == "" {
				assert.Nil(t, got, "an unused name must resolve to no existing Channel")
				return
			}
			require.NotNil(t, got)
			assert.Equal(t, tc.want, got.Name)
		})
	}
}

// TestChannelCreate_ManualRouteCarriesTheNameFlagIntoTheComeBackCommand goes
// through runChannelCreate, the production entry point, because that is the
// only place `--name` is turned into an answer.
//
// The kind's own test for this message calls Resolve directly with a channel
// name in the map, which the CLI did not in fact put there: answersFrom
// iterates the DECLARED questions, and the manual route deliberately declares
// no name question, so the flag was accepted by checkAnswerKeys — which
// hardcodes the name as always allowed — and then read by nothing. The
// operator got a come-back command missing the name they had just supplied,
// silently.
func TestChannelCreate_ManualRouteCarriesTheNameFlagIntoTheComeBackCommand(t *testing.T) {
	// The refusal writes the app manifest beside the run; keep it out of the
	// package directory. The path is kept because the FILE is asserted on
	// below — see the closing check.
	dir := t.TempDir()
	t.Chdir(dir)

	ctx := context.Background()
	b, _ := newFakeBundle(t, &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-agent", Namespace: "default"},
	})
	var out bytes.Buffer

	err := runChannelCreate(ctx, aptest.RefusingReader{T: t}, &out, aptest.GlobalsFor(b), channelCreateOptions{
		kind:           "slack",
		apply:          true,
		nonInteractive: true,
		name:           "my-chan",
		answers: []string{
			"agentclass=demo-agent",
			"slackapp=false",
			"capabilities=attachments",
		},
	})
	require.Error(t, err, "the manual route ends the run with the manifest")
	assert.Contains(t, err.Error(), "--name my-chan",
		"the name the operator supplied must survive into the command that finishes the job")

	// THE FILE IS THE POINT, and this is the only test that can prove it.
	// WizardInput.WorkingDir is what makes the copy on disk happen; the kind's
	// own tests set it themselves, so deleting the one line in runChannelCreate
	// that passes "." left every suite green while the operator silently
	// stopped getting the file. The message above is scrollback the next
	// command scrolls away and a `2>/dev/null` never sees at all — the copy
	// beside the run is what they still have when they come back for the
	// second run this route costs.
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	var saved []string
	for _, e := range entries {
		saved = append(saved, e.Name())
	}
	assert.Contains(t, saved, "slack-app-manifest-demo-agent.yaml",
		"the run must leave the app manifest in the operator's working directory")
}

// TestChannelCreate_RunChannelCreateWiresSeededIntoWizardInput goes one layer
// higher than the test above: through runChannelCreate itself, the actual
// production entry point, rather than reaching into Screens/checkAnswerKeys
// directly. That is what proves the wizIn.Seeded: st line in runChannelCreate
// (not just the slack kind's own use of it) is the thing carrying the answer.
//
// app-config-token is deliberately left unseeded, so the run stops on that
// question — a deterministic, network-free stopping point the fail-closed
// driver reaches before anything talks to Slack. Stopping THERE, rather than
// on checkAnswerKeys's "is not a question" refusal, is only possible once
// app-token-source was accepted AND the route resolved to provisioning: those
// two keys are declared only for a provisioning run (slack's
// agentCredentialQuestions, mirroring provisionScreen.AnswerKeys), and both
// require Seeded to have carried the answer through. A dropped or mistyped
// Seeded field would instead produce the checkAnswerKeys refusal, exactly like
// the "slackapp=true" case above.
//
// The refusal is slack's OWN, not the dispatcher's generic missing-answer
// message: WizardInput.NonInteractive (P5-R19) carries the one fact
// provisionScreen.RequiresInteraction needed, so the kind refuses from Inputs
// with the wording that says where to generate a configuration token. Pinning
// that sentence rather than "--answer app-config-token=<value>" is what makes
// this test notice if the kind-authored guidance is lost again — the generic
// message would still be produced by a wizard that had stopped refusing at all.
func TestChannelCreate_RunChannelCreateWiresSeededIntoWizardInput(t *testing.T) {
	ctx := context.Background()
	var out bytes.Buffer

	err := runChannelCreate(ctx, aptest.RefusingReader{T: t}, &out, &apcmd.Globals{Namespace: "default"}, channelCreateOptions{
		kind:           "slack",
		nonInteractive: true,
		answers: []string{
			"agentclass=demo-agent",
			"slackapp=provision",
			"capabilities=attachments",
			"app-token-source=paste",
		},
	})
	require.Error(t, err, "a configuration token was deliberately left unsupplied")
	assert.NotContains(t, err.Error(), "is not a question the slack wizard asks",
		"runChannelCreate must carry Seeded through to Screens, or checkAnswerKeys would reject app-token-source before ever reaching the refusal below")
	assert.Contains(t, err.Error(), "app-configuration token",
		"the refusal reached must be slack's own, not the dispatcher's generic missing-answer one")
	assert.Contains(t, err.Error(), "Your App Configuration Tokens",
		"and it must still say where to generate one — proof the route resolved to provisioning")
}

// TestChannelCreate_SeededNameCollisionIsRefusedBeforeTheWizardRuns: a name
// that is already taken is refused BEFORE any screen runs, because a wizard's
// screens are not all reversible.
//
// The Slack kind's provisioning route creates and installs a real Slack app
// partway through, and Slack has no API that lists a user's apps. Checking the
// name only in front of the apply — where channelwizard.Apply has always checked
// it — means that app is created, the run then fails on the name, and the
// summary naming the app is never rendered because the run returned an error.
// The app exists in the workspace with nothing anywhere recording its ID.
//
// Both subtests are network-free by construction, which is the point: the
// refusal has to land before anything can reach Slack.
func TestChannelCreate_SeededNameCollisionIsRefusedBeforeTheWizardRuns(t *testing.T) {
	ctx := context.Background()
	const taken = "slack-helpdesk"

	// A run that gets as far as a screen would prompt, and aptest.RefusingReader
	// fails the test when anything reads. That is the "no screen ran" proof:
	// nothing about a refusal message alone distinguishes where it came from.
	t.Run("interactive: the wizard is never presented", func(t *testing.T) {
		b, dyn := newFakeBundle(t,
			&spiceboxv1alpha1.Channel{ObjectMeta: metav1.ObjectMeta{Name: taken, Namespace: "default"}},
			&spiceboxv1alpha1.AgentClass{ObjectMeta: metav1.ObjectMeta{Name: "demo-agent", Namespace: "default"}},
		)
		var out bytes.Buffer

		err := runChannelCreate(ctx, aptest.RefusingReader{T: t}, &out, aptest.GlobalsFor(b), channelCreateOptions{
			kind:  "slack",
			apply: true,
			name:  taken,
		})
		require.Error(t, err, "a seeded name that is already taken must be refused")
		assert.Contains(t, err.Error(), "already exists")
		assert.Contains(t, err.Error(), taken, "the refusal must name the Channel")

		list, listErr := dyn.Resource(channelGVR).Namespace("default").List(ctx, metav1.ListOptions{})
		require.NoError(t, listErr)
		assert.Empty(t, list.Items, "a refused run must not have applied anything")
	})

	// The provisioning route specifically: this run is one --answer short of
	// creating a Slack app, and the collision has to win over every other
	// refusal the wizard could raise — including CheckInteractionRequired's,
	// which is itself as early as the old ordering allowed.
	t.Run("non-interactive provisioning route: the collision outranks the wizard's own refusal", func(t *testing.T) {
		b, _ := newFakeBundle(t,
			&spiceboxv1alpha1.Channel{ObjectMeta: metav1.ObjectMeta{Name: taken, Namespace: "default"}},
		)
		var out bytes.Buffer

		err := runChannelCreate(ctx, aptest.RefusingReader{T: t}, &out, aptest.GlobalsFor(b), channelCreateOptions{
			kind:           "slack",
			apply:          true,
			name:           taken,
			nonInteractive: true,
			answers: []string{
				"agentclass=demo-agent",
				"slackapp=provision",
				"capabilities=attachments",
				"app-token-source=paste",
			},
		})
		require.Error(t, err, "a seeded name that is already taken must be refused")
		assert.Contains(t, err.Error(), "already exists",
			"the name check must run before the wizard, whose own refusal would otherwise be reached first")
		assert.NotContains(t, err.Error(), "app-configuration token",
			"reaching provisionScreen.RequiresInteraction means the wizard was assembled and consulted first")
	})
}

// TestChannelCreate_AFailureAfterTheWizardStillSaysWhatItDid: some of what a
// wizard does outlives the run. The Slack provisioning route creates and
// installs a real app and records it as a summary note; if the run then fails,
// returning only the error would throw that note away — and with it the only
// record of an app no Slack API can list.
//
// The failure is made at the apply's own existence check — the shape of the
// race the up-front name check cannot close, where the name was free when the
// run started and is not free (or not answerable) when the apply gets there.
// The kind does not matter to what is being pinned, so the run uses the test
// fixture kind, which keeps it hermetic and records a note of its own.
func TestChannelCreate_AFailureAfterTheWizardStillSaysWhatItDid(t *testing.T) {
	ctx := context.Background()
	b, _ := newFakeBundle(t)
	b.Controller = failingChannelGet(t)
	var out bytes.Buffer

	err := runChannelCreate(ctx, strings.NewReader(""), &out, aptest.GlobalsFor(b), channelCreateOptions{
		kind:  "fake",
		apply: true,
	})
	require.Error(t, err, "the apply's own Get failure must still fail the run")
	assert.Contains(t, err.Error(), "check for existing Channel", "and must say what went wrong")

	assert.Contains(t, out.String(), "fake-channel (test fixture)",
		"what the run had already decided must still reach the terminal: on the Slack provisioning route this block is the only record of an app that now exists")
}

// failingChannelGet builds a client whose Channel Gets all fail, so a run
// reaches channelwizard.Apply's existence check and stops there — after the
// wizard has run and recorded what it did.
func failingChannelGet(t *testing.T) client.Client {
	t.Helper()
	return fake.NewClientBuilder().WithScheme(aptest.Scheme(t)).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, isChannel := obj.(*spiceboxv1alpha1.Channel); isChannel {
				return errors.New("the API server went away")
			}
			return cl.Get(ctx, key, obj, opts...)
		},
	}).Build()
}

// TestKindNamesIncludesFake verifies the fake kind is registered via its
// init() side-effect import.
func TestKindNamesIncludesFake(t *testing.T) {
	assert.Contains(t, KindNames(), "fake", "KindNames should contain 'fake'")
}

func TestPromptKind(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "empty input: selects first registered kind",
			input: "\n",
			want:  "", // sentinel; filled in below from KindNames()[0]
		},
		{
			name:  "explicit name 'fake': selects fake",
			input: "fake\n",
			want:  "fake",
		},
	}
	names := KindNames()
	if len(names) == 0 {
		t.Skip("no kinds registered")
	}
	cases[0].want = names[0]

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			got, err := promptKind(strings.NewReader(tc.input), &out)
			require.NoError(t, err, "promptKind")
			assert.Equal(t, tc.want, got, "selected kind")
		})
	}
}

// TestChannelCreate_MonitoringRefusesAKindThatCannotDeliver: bento's wizard
// ignores WizardInput.Monitoring, so without this guard `--monitoring` would
// hand back an ordinary agent Channel the monitoring relay never writes to.
func TestChannelCreate_MonitoringRefusesAKindThatCannotDeliver(t *testing.T) {
	ctx := context.Background()
	b, dyn := newFakeBundle(t)
	var out bytes.Buffer

	err := runChannelCreate(ctx, aptest.RefusingReader{T: t}, &out, aptest.GlobalsFor(b), channelCreateOptions{
		kind:           "bento",
		apply:          true,
		monitoring:     true,
		nonInteractive: true,
	})
	require.Error(t, err, "a kind that cannot deliver monitoring events must be refused")
	assert.Contains(t, err.Error(), "monitoring")
	assert.Contains(t, err.Error(), "slack", "the refusal must name a kind that would work")

	list, listErr := dyn.Resource(channelGVR).Namespace("default").List(ctx, metav1.ListOptions{})
	require.NoError(t, listErr)
	assert.Empty(t, list.Items, "a refused run must not have applied a Channel")
}

// TestMonitoringCapableKindNames: the suggestion in that refusal is derived
// from the registry, so it can never name a kind that would itself be refused
// — and it must not advertise the test fixture to a real user, which the
// registry alone would, since fake reports SupportsMonitoring() == true.
func TestMonitoringCapableKindNames(t *testing.T) {
	got := monitoringCapableKindNames()
	require.NotEmpty(t, got)
	assert.Contains(t, got, "slack")
	assert.NotContains(t, got, "bento", "bento reports SupportsMonitoring() == false")
	assert.NotContains(t, got, "fake", "the test fixture must never be suggested to a user")
	assert.NotContains(t, got, "local", "the local kind must never be suggested to a user")
}

// TestChannelCreate_RejectsAnUnknownAnswerKey: an --answer key no screen asks
// for is a typo. Dropping it silently makes it resurface later as some other
// screen's refusal, or — interactively — as a question the user believed they
// had already answered.
//
// The namespace carries an AgentClass because the key check can only run
// against what Inputs returned, and slack's Inputs lists the namespace's
// AgentClasses first (so a flag cannot bind a Channel to an agent that does
// not exist). An empty namespace therefore refuses THERE — a true refusal,
// but not the one this test is about. Its sibling below seeded one for the
// same reason.
func TestChannelCreate_RejectsAnUnknownAnswerKey(t *testing.T) {
	ctx := context.Background()
	b, dyn := newFakeBundle(t, &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-agent", Namespace: "default"},
	})
	var out bytes.Buffer

	err := runChannelCreate(ctx, aptest.RefusingReader{T: t}, &out, aptest.GlobalsFor(b), channelCreateOptions{
		kind:           "slack",
		apply:          true,
		nonInteractive: true,
		answers:        []string{"botoken=xoxb-typo"},
	})
	require.Error(t, err, "a key no screen asks for must be refused")
	assert.Contains(t, err.Error(), "botoken", "the refusal must name the offending key")
	for _, key := range []string{"agentclass", "slackapp", "capabilities", "bot-token", "app-token", "name"} {
		assert.Contains(t, err.Error(), key, "the refusal must list the keys that DO work")
	}

	list, listErr := dyn.Resource(channelGVR).Namespace("default").List(ctx, metav1.ListOptions{})
	require.NoError(t, listErr)
	assert.Empty(t, list.Items, "a refused run must not have applied a Channel")
}

// TestChannelCreate_NonInteractiveNeedsAKind: a run that cannot prompt cannot
// be asked which kind to build, so it must say so rather than reach the kind
// menu with nobody watching.
func TestChannelCreate_NonInteractiveNeedsAKind(t *testing.T) {
	var out bytes.Buffer
	err := runChannelCreate(context.Background(), aptest.RefusingReader{T: t}, &out, &apcmd.Globals{Namespace: "default"},
		channelCreateOptions{nonInteractive: true})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--kind")
}

func monitoringChan(name, kind string) *spiceboxv1alpha1.Channel {
	return &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec:       spiceboxv1alpha1.ChannelSpec{Role: spiceboxv1alpha1.ChannelRoleMonitoring, Kind: kind},
	}
}

func nonMonitoringChan(name, kind string) *spiceboxv1alpha1.Channel {
	ch := monitoringChan(name, kind)
	ch.Spec.Role = "" // not monitoring
	return ch
}

func TestListMonitoringChannels(t *testing.T) {
	scheme := aptest.Scheme(t)

	cases := []struct {
		name      string
		objs      []client.Object
		wantNames []string
	}{
		{name: "no channels: empty", objs: nil, wantNames: nil},
		{
			name:      "one monitoring channel: returned",
			objs:      []client.Object{monitoringChan("team-alerts", "slack")},
			wantNames: []string{"team-alerts"},
		},
		{
			name: "monitoring filtered from non-monitoring, sorted by name",
			objs: []client.Object{
				monitoringChan("zeta", "slack"),
				nonMonitoringChan("helpdesk", "slack"),
				monitoringChan("alpha", "slack"),
			},
			wantNames: []string{"alpha", "zeta"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tc.objs...).Build()
			got, err := ListMonitoringChannels(context.Background(), c, "default")
			require.NoError(t, err)
			var names []string
			for _, ch := range got {
				names = append(names, ch.Name)
			}
			assert.Equal(t, tc.wantNames, names)
		})
	}
}

func TestListMonitoringChannels_NilClientReportsNone(t *testing.T) {
	got, err := ListMonitoringChannels(context.Background(), nil, "default")
	require.NoError(t, err)
	assert.Nil(t, got)
}

// --- --role -------------------------------------------------------------------

// TestChannelCreate_RoleFlagReachesTheChannel is what makes the remedy this
// repo prints actually work.
//
// `oap agent install` prints `oap channel create …` for every declared channel
// a scripted run could not wire, and the bundle's README tells an operator to
// run it. Without --role that paste cannot express the declared role, so a
// bundle declaring `role: output` gets a Channel on ChannelSpec.Role's `both`
// default — and `both` is deliberately not an output-binding candidate, so the
// agent's input Channel goes Valid=False with nowhere to deliver.
//
// The fake kind is the fixture because its Result sets NO role, which is the
// only case where the flag is load-bearing (slack is the production instance
// of that shape). --apply=false is what makes this a manifest assertion rather
// than a cluster one; the stamp happens in wizardrun.Finish, before either.
func TestChannelCreate_RoleFlagReachesTheChannel(t *testing.T) {
	var out bytes.Buffer
	g := &apcmd.Globals{Namespace: "default"}
	require.NoError(t,
		runChannelCreate(context.Background(), strings.NewReader(""), &out, g,
			channelCreateOptions{kind: "fake", role: spiceboxv1alpha1.ChannelRoleOutput}))

	assert.Contains(t, out.String(), "role: output",
		"the Channel this run would apply must carry the role the flag asked for")
}

// TestChannelCreate_NoRoleFlagLeavesTheKindsOwnAnswerAlone: every run that
// existed before the flag did must reach the same manifests. The fake kind
// sets no role, so "left alone" means the field is absent and the apiserver
// applies its own default — which is what has always happened.
func TestChannelCreate_NoRoleFlagLeavesTheKindsOwnAnswerAlone(t *testing.T) {
	var out bytes.Buffer
	g := &apcmd.Globals{Namespace: "default"}
	require.NoError(t,
		runChannelCreate(context.Background(), strings.NewReader(""), &out, g,
			channelCreateOptions{kind: "fake"}))

	assert.NotContains(t, out.String(), "role:",
		"a run that named no role must not invent one")
}

// TestChannelCreate_ARoleThisCommandCannotHonourIsRefusedBeforeTheWizard.
//
// The refusal has to land before the flow runs, not after: the slack kind's
// provisioning route creates and installs a real app that no API will list
// afterwards, so a flag combination that was never going to work must cost
// nothing. Asserted by the ABSENCE of any wizard output in the buffer — a
// refusal that fired after the run would return the same error.
//
// monitoring is its own row because it is not a typo: it is a legal
// ChannelSpec.Role, and --monitoring already asks for it while doing more than
// setting the field (it selects the kind's monitoring flow and resolves the
// Channel being reconfigured). Two ways to say one thing is two things that
// can disagree.
func TestChannelCreate_ARoleThisCommandCannotHonourIsRefusedBeforeTheWizard(t *testing.T) {
	cases := []struct {
		name   string
		opts   channelCreateOptions
		wantIn string
	}{{
		name:   "a role no ChannelSpec.Role value matches: refused naming the legal set",
		opts:   channelCreateOptions{kind: "fake", role: "outputs"},
		wantIn: `unknown --role "outputs"`,
	}, {
		name:   "--role monitoring: refused, pointing at the flag that does ask for one",
		opts:   channelCreateOptions{kind: "fake", role: spiceboxv1alpha1.ChannelRoleMonitoring},
		wantIn: "pass --monitoring instead",
	}, {
		name:   "--monitoring with an agent role: refused as the contradiction it is",
		opts:   channelCreateOptions{kind: "fake", monitoring: true, role: spiceboxv1alpha1.ChannelRoleOutput},
		wantIn: "binds to no agent",
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			err := runChannelCreate(context.Background(), strings.NewReader(""), &out,
				&apcmd.Globals{Namespace: "default"}, tc.opts)

			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantIn)
			assert.Empty(t, out.String(),
				"the refusal must precede the kind's flow, which for some kinds creates something outside the cluster")
		})
	}
}

// TestChannelCreate_TheTwoRoleRefusalsAgreeOnEveryInput pins the pair.
//
// `--role` is refused in two places by design: here, before anything runs and
// in this command's own vocabulary, and again in wizardrun.Finish, which is
// the backstop every client shares. That is the shape
// channelplan.RefuseNamedInstall documents and it is fine — but two wordings
// of one rule is exactly how two RULES appear, and this branch has already
// paid for that once. This asserts the VERDICTS match on every input: neither
// may refuse what the other allows.
//
// The texts are deliberately NOT compared. They must differ: wizardrun serves
// a browser form too and cannot name a flag, while the whole value of the
// early refusal is that it can say `pass --monitoring instead`.
//
// The table is the inputs where they could plausibly disagree — the two ends
// of the legal set, the empty string that means "leave the kind's answer
// alone", monitoring in both of the ways it can be asked for, a typo, a
// case variant, and whitespace that only one of them might trim.
func TestChannelCreate_TheTwoRoleRefusalsAgreeOnEveryInput(t *testing.T) {
	roles := []string{
		"", "input", "output", "both", "monitoring",
		"outputs", "Output", "  output  ", "  ", "INPUT", "input,output",
	}
	for _, monitoring := range []bool{false, true} {
		for _, role := range roles {
			t.Run(fmt.Sprintf("role=%q monitoring=%v", role, monitoring), func(t *testing.T) {
				shared := wizardrun.CheckRole(role, monitoring)
				cli := checkRoleFlag(channelCreateOptions{kind: "fake", role: role, monitoring: monitoring})

				assert.Equal(t, shared == nil, cli == nil,
					"the early refusal and the shared one must refuse the same set: wizardrun says %v, this command says %v",
					shared, cli)
				if shared != nil {
					assert.NotEmpty(t, cli.Error(),
						"a refusal with no words is worse than the shared one it replaced")
				}
			})
		}
	}
}

// TestChannelCreate_ARoleTheSharedRuleRefusesIsNeverSilentlyAccepted is the
// other half of the pin, and the one a future edit needs.
//
// checkRoleFlag re-derives which case fired only to phrase it. A rule added to
// wizardrun.CheckRole that this switch has no arm for must still refuse — by
// returning the shared error verbatim — rather than falling through to nil,
// which would make the early check quietly weaker than the backstop for
// exactly the rule somebody just added.
//
// Simulated by a role the switch's arms cannot claim: an empty-after-trim
// value is not monitoring, not "monitoring", and IS absent from
// AgentChannelRoles, so it exercises the fall-through only if CheckRole
// refuses it. It does not — an empty role is legal — so this asserts the
// contract rather than the current arm set: whatever CheckRole refuses,
// checkRoleFlag refuses.
func TestChannelCreate_ARoleTheSharedRuleRefusesIsNeverSilentlyAccepted(t *testing.T) {
	for _, role := range []string{"outputs", "monitoring", "INPUT"} {
		require.Error(t, wizardrun.CheckRole(role, false),
			"fixture check: the shared rule must refuse %q for this assertion to mean anything", role)
		assert.Error(t, checkRoleFlag(channelCreateOptions{kind: "fake", role: role}),
			"this command must not accept a role the tail it feeds will refuse")
	}
}

// TestChannelCreate_RegistersRole pins the flag itself. Both the install
// summary's finishing command and the reviewbot README now print `--role`, and
// those are prose claims about a flag set defined elsewhere: prose does not
// fail when the flag is renamed or dropped.
func TestChannelCreate_RegistersRole(t *testing.T) {
	f := newChannelCreateCmd(&apcmd.Globals{}).Flags().Lookup("role")
	require.NotNil(t, f, "oap agent install prints `oap channel create --role <role>`; that command must accept it")
	assert.Empty(t, f.DefValue,
		"the default must be empty — a default role would silently overwrite what every kind's own Result sets")
	for _, r := range spiceboxv1alpha1.AgentChannelRoles() {
		assert.Contains(t, f.Usage, r, "the help must list every role this flag accepts")
	}
	assert.NotContains(t, f.Usage, "monitoring, ",
		"monitoring is not one of them; --monitoring is, and the help says so")
}
