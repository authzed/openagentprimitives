package agentcmd

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/huh"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynfake "k8s.io/client-go/dynamic/fake"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/aptest"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/publicendpoint"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	fakekind "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/fake"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/wizardkeys"
	"github.com/authzed/openagentprimitives/pkg/cli/tui"
	"github.com/authzed/openagentprimitives/pkg/platform/cloud"
	"github.com/authzed/openagentprimitives/pkg/platform/oap"
	"github.com/authzed/openagentprimitives/pkg/platform/oap/channelplan"
)

// The kinds these tests drive are the two REAL registered ones whose flows a
// unit test can run to completion without a terminal, and they are chosen for
// what they prove rather than for convenience:
//
//   - "fake" asks nothing at all (channelkinds.Wizard.Inputs may legitimately
//     return no questions), so a run of it exercises the whole dispatch —
//     Inputs, Handoff, the collision check, Resolve, Result, the apply —
//     without a single screen.
//   - "local" returns channelkinds.UnavailableWizard, so its Inputs fails
//     immediately with the kind's own sentence. That is a REAL failing wizard,
//     not a stub of one, which is what the continuation assertions need.
//   - "bento" asks five plain questions and honours the seeded channel name,
//     so a fully-seeded run of it proves the plan's answers actually reach the
//     manifests.
//
// No test here may present a screen: everything is either question-less or
// fully seeded, and refusingDriver turns a presentation into a failure so that
// a change which starts asking cannot pass silently.
//
// THE SKIP TESTS ARE THE ONE EXEMPTION, and they earn it by setting the driver
// to nil — which is what an unattended install actually has, and what makes
// refusingDriver inapplicable rather than merely unused. They are free to name
// kinds no unit test could ever drive (github, slack), because the property
// they assert is that no flow is entered at all.
const (
	wizardlessKind = "fake"
	refusingKind   = "local"
	questionsKind  = "bento"
)

var channelGVR = schema.GroupVersionResource{
	Group:    "agentprimitives.authzed.com",
	Version:  "v1alpha1",
	Resource: "channels",
}

var agentClassGVR = schema.GroupVersionResource{
	Group:    "agentprimitives.authzed.com",
	Version:  "v1alpha1",
	Resource: "agentclasses",
}

var secretGVR = schema.GroupVersionResource{Group: "", Version: "v1", Resource: "secrets"}

// refusingDriver fails the test if a wizard ever tries to ask something.
//
// Every case below is either a kind that declares no question or a plan that
// seeds every one it does declare, so a presentation means the run took a path
// the test did not intend — which would otherwise show up as a hang or as a
// huh error attributed to the kind.
type refusingDriver struct{ t *testing.T }

func (d refusingDriver) Present(_ context.Context, screenID string, _ *huh.Group) error {
	d.t.Helper()
	d.t.Errorf("no screen may be presented in this test; the run reached %q", screenID)
	return errors.New("refusingDriver: nothing may be asked here")
}

// framedScreen is one screen this pass asked, and the frame it asked it in.
type framedScreen struct {
	screenID string
	chrome   *tui.Chrome
	step     string
}

// framingDriver is a terminal driver's stand-in that records the FRAME each
// screen is presented in and answers nothing (a Present that returns without
// running a form leaves each question on the default the screen preseeded).
//
// It implements tui.Reframer, which is the seam this pass now presents over —
// so what it records is exactly what the pass hands the terminal driver: which
// Chrome, and which rail step marked active. What the terminal then DRAWS from
// that is pinned in pkg/cli/tui (TestReframeDrawsOnePresentationAcrossSeveralRuns);
// asserting it a second time here would only re-test Chrome.Render through a
// hand-rolled copy of ttyDriver.Present.
//
// asked is shared by every reframed copy — that is the whole point, since the
// record has to span the pass rather than restart with each channel.
type framingDriver struct {
	chrome *tui.Chrome
	step   string
	asked  *[]framedScreen
}

func newFramingDriver() *framingDriver { return &framingDriver{asked: &[]framedScreen{}} }

func (d *framingDriver) Reframe(ch *tui.Chrome, stepID string) tui.Driver {
	n := *d
	n.chrome, n.step = ch, stepID
	return &n
}

func (d *framingDriver) Present(_ context.Context, screenID string, _ *huh.Group) error {
	*d.asked = append(*d.asked, framedScreen{screenID: screenID, chrome: d.chrome, step: d.step})
	return nil
}

// fixtureAgentClass is the AgentClass every fixture cluster holds, and the one
// every plan below binds to.
//
// IT IS NOT SCENERY. A channel kind that asks which AgentClass to bind resolves
// the question against the namespace's real listing
// (wizardkeys.AgentClassQuestion), so a cluster with no AgentClass fails that
// kind's Inputs outright — before a single screen is built. A fixture without
// one cannot reach the presentation layer at all, which silently disarms every
// assertion about what the presentation SAYS. That is exactly how the
// --answer guard below came to hold by coincidence rather than by the property
// it names.
const fixtureAgentClass = "demo-agent"

// wiringForTest is a channelWiring over a fake cluster, plus the dynamic
// client every apply lands in so a test can assert on objects rather than on
// stdout.
//
// The cluster always holds fixtureAgentClass, so a kind's questions are
// reachable; see that constant for why that is load-bearing and not just
// convenient.
func wiringForTest(t *testing.T, objs ...client.Object) (channelWiring, *dynfake.FakeDynamicClient) {
	t.Helper()
	objs = append(objs, &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: fixtureAgentClass, Namespace: "default"},
	})
	kb, dyn := aptest.NewFakeBundle(t, map[schema.GroupVersionResource]string{
		channelGVR:    "ChannelList",
		agentClassGVR: "AgentClassList",
		secretGVR:     "SecretList",
	}, objs...)
	return channelWiring{
		kube:   kb,
		in:     strings.NewReader(""),
		out:    io.Discard,
		theme:  tui.NewTheme(tui.Caps{}),
		driver: refusingDriver{t: t},
	}, dyn
}

// planFor is what channelplan.PlanChannels produces for a declaration it found
// nothing in the way of.
//
// It seeds the same two keys the real planner seeds for EVERY declaration —
// the channel name from the declaration, and the AgentClass this bundle
// installs — rather than the name alone. Seeding less than the planner does is
// not a simpler fixture, it is a different one: it leaves a kind's AgentClass
// question unanswered, which changes which screens get built and therefore
// what the presentation ever gets the chance to say.
func planFor(kind, name string) channelplan.ChannelPlan {
	return channelplan.ChannelPlan{
		Required:    oap.RequiredChannel{Kind: kind, Name: name, Role: spiceboxv1alpha1.ChannelRoleBoth},
		WiringKnown: true,
		Seeded: map[string]string{
			wizardkeys.KeyChannelName: name,
			wizardkeys.KeyAgentClass:  fixtureAgentClass,
		},
		SeededFrom: map[string]string{
			wizardkeys.KeyChannelName: "this bundle's requires.channels declaration",
			wizardkeys.KeyAgentClass:  "the AgentClass this bundle installs",
		},
		NotSeeded: map[string]string{},
	}
}

// TestWireChannels_PartialFailureKeepsTheEarlierChannel: two declared, the
// second's wizard errors. The first stays — nothing is torn down, because by
// the time a wizard fails it has already done things outside the cluster that
// deleting a Channel would not unmake — the exit is non-zero, and the report
// carries the command that finishes the one that failed.
func TestWireChannels_PartialFailureKeepsTheEarlierChannel(t *testing.T) {
	ctx := context.Background()
	w, dyn := wiringForTest(t)

	rep, err := wireChannels(ctx, w, []channelplan.ChannelPlan{
		planFor(wizardlessKind, "demo-agent-first"),
		planFor(refusingKind, "demo-agent-second"),
	})

	require.Error(t, err, "an unwired declared channel is a non-zero exit")
	assert.Equal(t, []string{"demo-agent-first"}, rep.Wired(),
		"what succeeded stays — the failure does not unmake it")
	assert.Equal(t, []string{"demo-agent-second"}, rep.Failed())
	assert.Contains(t, rep.String(), "oap channel create --kind local --name demo-agent-second --namespace default --role both",
		"the report must carry the command that finishes the job")

	// The bookkeeping is not the claim: the earlier channel's Channel object
	// has to actually be on the cluster.
	_, getErr := dyn.Resource(channelGVR).Namespace("default").Get(ctx, "fake-channel", metav1.GetOptions{})
	require.NoError(t, getErr, "the first channel's Channel must have been applied and left alone")
}

// TestWireChannels_ContinuesWhenTHEFIRSTChannelIsTheOneThatFails is the same
// property with the fixture reversed, and it is the one that proves the
// assertion above is about CONTINUATION rather than about ordering luck: a
// loop that aborted on the first failure would still satisfy the test above,
// because the failing channel is last there.
func TestWireChannels_ContinuesWhenTHEFIRSTChannelIsTheOneThatFails(t *testing.T) {
	ctx := context.Background()
	w, dyn := wiringForTest(t)

	rep, err := wireChannels(ctx, w, []channelplan.ChannelPlan{
		planFor(refusingKind, "demo-agent-second"),
		planFor(wizardlessKind, "demo-agent-first"),
	})

	require.Error(t, err)
	assert.Equal(t, []string{"demo-agent-first"}, rep.Wired(),
		"the channel AFTER the failure must still be attempted")
	assert.Equal(t, []string{"demo-agent-second"}, rep.Failed())

	_, getErr := dyn.Resource(channelGVR).Namespace("default").Get(ctx, "fake-channel", metav1.GetOptions{})
	require.NoError(t, getErr, "the channel declared after the failing one must still have been created")
}

// TestWireChannels_AlreadyWiredIsReportedNotRecreated: a re-run must be safe.
// The kind here would apply a Channel if its wizard ran, so "nothing was
// applied" is a real observation and not the absence of one.
func TestWireChannels_AlreadyWiredIsReportedNotRecreated(t *testing.T) {
	w, dyn := wiringForTest(t)
	plan := planFor(wizardlessKind, "demo-agent-first")
	plan.AlreadyWired = true

	rep, err := wireChannels(context.Background(), w, []channelplan.ChannelPlan{plan})

	require.NoError(t, err)
	assert.Equal(t, []string{"demo-agent-first"}, rep.AlreadyWired())
	assert.Empty(t, rep.Wired(), "an already-wired channel is not something this run wired")
	assert.Empty(t, dyn.Actions(), "nothing may be applied for a channel that is already there")
}

// TestWireChannels_ConflictIsRefusedRatherThanSkipped: a Channel holding the
// declared name that is NOT ours is a conflict, and channelplan leaves
// AlreadyWired false alongside it deliberately. Skipping it — or reading only
// AlreadyWired and wiring over it — is the failure that ruling exists to
// prevent: every CR healthy and an agent that can never receive anything.
//
// The kind is the one whose wizard SUCCEEDS, so a run that ignored Conflict
// would land in Wired and this would fail on the status rather than on the
// wording.
func TestWireChannels_ConflictIsRefusedRatherThanSkipped(t *testing.T) {
	w, dyn := wiringForTest(t)
	plan := planFor(wizardlessKind, "demo-agent-first")
	plan.Conflict = &channelplan.ChannelConflict{
		Kind:       "slack",
		AgentClass: "demo-other-agent",
		Reason:     `a Channel named "demo-agent-first" already exists in this namespace but is bound to agent "demo-other-agent"`,
	}

	rep, err := wireChannels(context.Background(), w, []channelplan.ChannelPlan{plan})

	require.Error(t, err, "a conflicting name is a non-zero exit, not a skip")
	assert.Equal(t, []string{"demo-agent-first"}, rep.Failed())
	assert.Empty(t, rep.Wired())
	assert.Empty(t, rep.AlreadyWired(), "a name held by somebody else is not this install being a re-run")
	assert.Contains(t, rep.String(), `is bound to agent "demo-other-agent"`,
		"the planner's reason is printable as it stands and must reach the operator")
	assert.Empty(t, dyn.Actions(), "nothing may be written over a Channel that is not ours")
}

// TestWireChannels_UnknownWiringIsNotTreatedAsAbsent: AlreadyWired=false means
// "not wired" only when the planner actually looked. When WiringKnown is false
// it never did, and creating a Channel that may already exist is exactly what
// the flag is there to stop.
//
// Again the kind is the one whose wizard succeeds, so a run that ignored
// WiringKnown would report it wired.
func TestWireChannels_UnknownWiringIsNotTreatedAsAbsent(t *testing.T) {
	w, dyn := wiringForTest(t)
	plan := planFor(wizardlessKind, "demo-agent-first")
	plan.WiringKnown = false

	rep, err := wireChannels(context.Background(), w, []channelplan.ChannelPlan{plan})

	require.Error(t, err)
	assert.Equal(t, []string{"demo-agent-first"}, rep.Failed())
	assert.Empty(t, rep.Wired(), "an unanswered question is not permission to create")
	assert.Contains(t, rep.String(), "could not determine whether a Channel named \"demo-agent-first\" already exists")
	assert.Empty(t, dyn.Actions())
}

// TestWireChannels_UnregisteredKindIsAnErrorNamingIt: the spec's step 2 — a
// kind this binary does not link is an error naming the kind, not a skip. It
// is checked here as well as in the lint before the install, because a lint an
// operator never ran is not a check.
func TestWireChannels_UnregisteredKindIsAnErrorNamingIt(t *testing.T) {
	w, _ := wiringForTest(t)

	rep, err := wireChannels(context.Background(), w,
		[]channelplan.ChannelPlan{planFor("no-such-kind", "demo-agent-first")})

	require.Error(t, err)
	assert.Equal(t, []string{"demo-agent-first"}, rep.Failed())
	assert.Contains(t, rep.String(), `"no-such-kind" is not a channel kind this build of oap has`)
	assert.Contains(t, rep.String(), wizardlessKind,
		"the refusal lists what this build DOES have, derived from the registry")
}

// TestWireChannels_ReportsWhatWasPreSeededAndWhereFrom: §4.1's "pre-seeding is
// not silent". A value taken from a ConfigMap the operator has never seen,
// applied without a word, is impossible to debug when it is wrong.
func TestWireChannels_ReportsWhatWasPreSeededAndWhereFrom(t *testing.T) {
	w, _ := wiringForTest(t)
	plan := planFor(wizardlessKind, "demo-agent-gh")
	plan.Seeded[wizardkeys.KeyExternalBaseURL] = "https://ap.demo.test"
	plan.SeededFrom[wizardkeys.KeyExternalBaseURL] = "ConfigMap agentprimitives-system/spicebox-webd-external-url, published by webd"

	rep, err := wireChannels(context.Background(), w, []channelplan.ChannelPlan{plan})
	require.NoError(t, err)

	assert.Contains(t, rep.String(), "https://ap.demo.test")
	assert.Contains(t, rep.String(), "spicebox-webd-external-url",
		"a value the operator never typed must say where it came from")
}

// TestWireChannels_AnAbsenceReasonIsNeverRenderedAsProvenance: SeededFrom and
// NotSeeded are disjoint by construction, and the split exists so that a
// renderer cannot label a failed read as the source of a value. This pins the
// rendering side of that: the reason appears, and it never appears under the
// heading that means "where this answer came from".
func TestWireChannels_AnAbsenceReasonIsNeverRenderedAsProvenance(t *testing.T) {
	w, _ := wiringForTest(t)
	plan := planFor(wizardlessKind, "demo-agent-gh")
	plan.NotSeeded[wizardkeys.KeyExternalBaseURL] =
		"could not read ConfigMap agentprimitives-system/spicebox-webd-external-url: forbidden"

	rep, err := wireChannels(context.Background(), w, []channelplan.ChannelPlan{plan})
	require.NoError(t, err)
	out := rep.String()

	require.Contains(t, out, "forbidden", "the reason a value is missing must reach the operator")
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "forbidden") {
			assert.NotContains(t, line, "source:",
				"a reason a key could NOT be answered must never be rendered as that key's source")
		}
	}
	assert.NotContains(t, out, wizardkeys.KeyExternalBaseURL+" = ",
		"a key that was not seeded must not be listed among the answers supplied")
}

// TestWireChannels_AChannelWhoseFlowNeverRanClaimsNoAnswers: a channel refused
// before its wizard started answered nothing, so the summary must not tell the
// operator that values were supplied "so you were not asked". Nothing asked
// anything.
//
// The case is a Conflict, but the property is about every pre-flow refusal —
// unknown wiring, an unregistered kind, nobody at the terminal — which all
// share the unwired status and all reach the summary the same way.
func TestWireChannels_AChannelWhoseFlowNeverRanClaimsNoAnswers(t *testing.T) {
	w, _ := wiringForTest(t)
	plan := planFor(wizardlessKind, "demo-agent-conflicted")
	plan.Seeded[wizardkeys.KeyExternalBaseURL] = "https://ap.demo.test"
	plan.SeededFrom[wizardkeys.KeyExternalBaseURL] = "ConfigMap spicebox-webd-external-url"
	plan.Conflict = &channelplan.ChannelConflict{
		Kind:       "slack",
		AgentClass: "demo-other-agent",
		Reason:     "bound to another agent",
	}

	rep, err := wireChannels(context.Background(), w, []channelplan.ChannelPlan{plan})
	require.Error(t, err)
	out := rep.String()

	require.Contains(t, out, "bound to another agent", "the refusal itself must still be reported")
	assert.NotContains(t, out, "answered for you",
		"nothing was asked, so nothing was answered on the operator's behalf")
	assert.NotContains(t, out, "https://ap.demo.test",
		"a value this run never used must not be reported as one it supplied")
}

// TestWireChannels_NoOneAtStdinIsSkippedInInstallsOwnVocabulary covers the
// join between this command and the shared driver.
//
// channelwizard's fail-closed presentation names `--answer`, `--name` and
// `--non-interactive` when it refuses. `oap agent install` registers neither
// `--answer` nor `--non-interactive`, and the `--name` it DOES register means
// the install instance name and is refused outright for a channel-declaring
// bundle — so every one of those three is bad advice here, one of them by
// being rejected rather than by being unknown
// (TestAgentInstallDoesNotOfferTheChannelCreateFlags). This pass therefore
// never enters that presentation: with nobody at stdin it records the channel
// as SKIPPED in its own words and points at the command that does take those
// flags.
//
// There are two wrong ways to implement that skip, and this test is shaped to
// catch both:
//
//   - Hand the run down with a "nobody is watching" hint
//     (tui.Options.NonInteractive) and it reaches the fail-closed refusal,
//     printing advice for a command the operator is not running. The two
//     NotContains catch that, and with fixtureAgentClass present bento's
//     Inputs really does reach the presentation layer, so they can fire.
//   - Enter the flow with no hint at all and it is worse: a nil driver
//     resolves to the line-oriented one over a stream nobody is writing to,
//     and huh turns end-of-input into every field's default with a nil error.
//     The channel is reported WIRED, from answers nobody gave. The empty
//     dyn.Actions and the empty Wired catch that one.
func TestWireChannels_NoOneAtStdinIsSkippedInInstallsOwnVocabulary(t *testing.T) {
	w, dyn := wiringForTest(t)
	w.driver = nil

	rep, err := wireChannels(context.Background(), w,
		[]channelplan.ChannelPlan{planFor(questionsKind, "demo-agent-trigger")})

	// THE VOCABULARY ASSERTIONS COME BEFORE THE EXIT-CODE ONE, and the order
	// is the difference between a guard and a decoration. The wrong fix that
	// produces the `--answer` advice also FAILS this channel, so
	// `require.NoError` reddens under it — and a require aborts, which would
	// leave the four assertions that catch the wrong wording unrun in exactly
	// the case they exist for. Nothing below reads err, so nothing is gained
	// by making them wait on it.
	out := rep.String()
	assert.NotContains(t, out, "--answer",
		"an install must never tell its operator to pass a flag it does not have")
	assert.NotContains(t, out, "--non-interactive",
		"same rule, same flag set: this command has no --non-interactive either")
	assert.Contains(t, out, "nobody is at this terminal",
		"the words the operator reads must be this command's own")
	assert.Contains(t, out, "oap channel create --kind bento --name demo-agent-trigger --namespace default --role both")

	require.NoError(t, err, "a channel nobody was there to answer for is reported, not failed")
	assert.Equal(t, []string{"demo-agent-trigger"}, rep.Skipped())
	assert.Empty(t, rep.Failed(), "a skip is not a failure — that difference IS the exit code")
	assert.Empty(t, rep.Wired())
	assert.Empty(t, dyn.Actions())
}

// TestWireChannels_NonInteractiveSkipsAndNamesTheCommands is spec §2.2 whole:
// a scripted install applies the bundle, names every channel it could not
// wire, prints the command that wires each one, and exits 0. CI stays green
// and the gap is named rather than discovered later.
//
// The kinds are github and slack — the two whose flows a unit test could never
// drive, one needing a browser handoff and the other a real workspace. That is
// the point rather than a hazard: the skip is decided BEFORE any flow starts,
// so the kinds that most need a human are exactly the ones this must handle
// without touching them. refusingDriver is not in play here at all; the driver
// is nil, which is what an unattended install actually has.
//
// Two channels rather than one, because the claim is about every declaration:
// a pass that skipped the first and fell through on the second would satisfy a
// single-channel test.
//
// The ROLES are input and output, not the fixture's usual `both`, because that
// is the pairing an agent with two channels actually has — and it is the one
// where the printed command is load-bearing rather than convenient. A paste
// that cannot say `--role output` produces a `both` Channel, which outputbind
// deliberately does not match, so the input Channel beside it goes
// Valid=False and the agent delivers nothing. Every command below is asserted
// WHOLE for that reason.
func TestWireChannels_NonInteractiveSkipsAndNamesTheCommands(t *testing.T) {
	w, dyn := wiringForTest(t)
	w.driver = nil

	gh := planFor("github", "demo-agent-gh")
	gh.Required.Role = spiceboxv1alpha1.ChannelRoleInput
	slack := planFor("slack", "demo-agent-slack")
	slack.Required.Role = spiceboxv1alpha1.ChannelRoleOutput

	rep, err := wireChannels(context.Background(), w, []channelplan.ChannelPlan{gh, slack})

	require.NoError(t, err, "CI must stay green: an unpromptable channel is reported, not failed")
	assert.Empty(t, rep.Wired())
	assert.Equal(t, []string{"demo-agent-gh", "demo-agent-slack"}, rep.Skipped())
	assert.Empty(t, rep.Failed())

	out := rep.String()
	// The WHOLE command, not the `--kind` prefix. Install is namespaced and
	// each declaration is roled, and a come-back an operator pastes without
	// either creates a Channel nothing is looking for.
	assert.Contains(t, out, "oap channel create --kind github --name demo-agent-gh --namespace default --role input")
	assert.Contains(t, out, "oap channel create --kind slack --name demo-agent-slack --namespace default --role output")
	assert.Contains(t, out, "cannot prompt",
		"the reason must be stated — a silent skip is indistinguishable from a bug")
	assert.Contains(t, out, "2 declared, 0 wired, 0 already wired, 2 skipped, 0 not wired",
		"a skipped channel must be counted, not dropped from the one line an operator skims")
	assert.Empty(t, dyn.Actions(), "an install that cannot prompt creates no Channel")
}

// TestWireChannels_CannotPromptSwallowsNothingElse: "nobody is at this
// terminal" is a reason to skip a channel this run would otherwise have set
// up. It is NOT a blanket over verdicts that have nothing to do with
// prompting, and the only thing enforcing that is the position of wireOne's
// stdin guard — moved to the front, a name held by somebody else's Channel
// becomes a green install.
//
// Every case pairs the plan under test with a plain one, and that second
// channel is what makes the row discriminating: a run that could still prompt
// would wire it, so its appearing in Skipped proves the driver really was
// absent when the row's own verdict was reached.
func TestWireChannels_CannotPromptSwallowsNothingElse(t *testing.T) {
	cases := []struct {
		name             string
		plan             func() channelplan.ChannelPlan
		wantErr          bool
		wantFailed       []string
		wantAlreadyWired []string
	}{
		{
			name: "a name held by somebody else: still a failure, still non-zero",
			plan: func() channelplan.ChannelPlan {
				p := planFor(wizardlessKind, "demo-agent-conflicted")
				p.Conflict = &channelplan.ChannelConflict{
					Kind:       "slack",
					AgentClass: "demo-other-agent",
					Reason:     "bound to another agent",
				}
				return p
			},
			wantErr:    true,
			wantFailed: []string{"demo-agent-conflicted"},
		},
		{
			name: "wiring state never established: still a failure, still non-zero",
			plan: func() channelplan.ChannelPlan {
				p := planFor(wizardlessKind, "demo-agent-unknown")
				p.WiringKnown = false
				return p
			},
			wantErr:    true,
			wantFailed: []string{"demo-agent-unknown"},
		},
		{
			name: "a kind this build does not have: still a failure, still non-zero",
			plan: func() channelplan.ChannelPlan {
				return planFor("no-such-kind", "demo-agent-unknown-kind")
			},
			wantErr:    true,
			wantFailed: []string{"demo-agent-unknown-kind"},
		},
		{
			name: "already ours: still already wired, and still exit 0",
			plan: func() channelplan.ChannelPlan {
				p := planFor(wizardlessKind, "demo-agent-existing")
				p.AlreadyWired = true
				return p
			},
			wantAlreadyWired: []string{"demo-agent-existing"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w, dyn := wiringForTest(t)
			w.driver = nil

			rep, err := wireChannels(context.Background(), w,
				[]channelplan.ChannelPlan{tc.plan(), planFor(questionsKind, "demo-agent-trigger")})

			if tc.wantErr {
				require.Error(t, err, "a verdict a prompt could not have changed must not exit 0")
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tc.wantFailed, rep.Failed())
			assert.Equal(t, tc.wantAlreadyWired, rep.AlreadyWired())
			assert.Equal(t, []string{"demo-agent-trigger"}, rep.Skipped(),
				"the plain channel was skipped, so this run genuinely could not prompt")
			assert.Empty(t, rep.Wired())
			assert.Empty(t, dyn.Actions())
		})
	}
}

// TestWireChannels_ASeededKeyTheKindNeverAsksForIsNotRefused is the other half
// of that join, and the discriminating one.
//
// channelwizard.checkAnswerKeys refuses a seeded key the kind does not declare
// — quoting `--answer <key>=…` — and it is driven by Seeded.Keys, which this
// pass leaves nil precisely because these answers came from the bundle and the
// cluster rather than from a flag. The plan seeds `external-base-url` for
// EVERY kind, and bento asks for no such thing; populating Keys would fail
// this run with advice naming a flag `oap agent install` does not have.
func TestWireChannels_ASeededKeyTheKindNeverAsksForIsNotRefused(t *testing.T) {
	ctx := context.Background()
	w, dyn := wiringForTest(t)

	plan := planFor(questionsKind, "demo-agent-trigger")
	plan.Required.Role = spiceboxv1alpha1.ChannelRoleInput
	// The key nothing in bento's flow asks for — seeded by every plan.
	plan.Seeded[wizardkeys.KeyExternalBaseURL] = "https://ap.demo.test"
	plan.SeededFrom[wizardkeys.KeyExternalBaseURL] = "ConfigMap spicebox-webd-external-url"
	// bento's remaining three questions, so the run needs no terminal. In
	// production these are asked; here they stand in for what an operator
	// would have typed.
	plan.Seeded[wizardkeys.KeyAuthzSubject] = "service:demo-agent-bot"
	plan.Seeded["interval"] = "@every 168h"
	plan.Seeded["mapping"] = `root = "demo"`

	rep, err := wireChannels(ctx, w, []channelplan.ChannelPlan{plan})

	require.NoError(t, err, "a bundle-supplied seed is not a typed --answer key and must not be checked as one")
	assert.Equal(t, []string{"demo-agent-trigger"}, rep.Wired())
	assert.NotContains(t, rep.String(), "--answer")

	// The declared name is the load-bearing one: the credentials Secret is
	// derived from it, and a bundled AgentIdentity names that Secret directly.
	got, getErr := dyn.Resource(channelGVR).Namespace("default").Get(ctx, "demo-agent-trigger", metav1.GetOptions{})
	require.NoError(t, getErr, "the Channel must carry the name the bundle declared")
	assert.Equal(t, fixtureAgentClass, got.Object["spec"].(map[string]any)["agentClass"],
		"the AgentClass the plan seeded must be the one the Channel binds to")
}

// TestWireChannels_StampsTheDeclaredRole: the role is the one part of a
// declaration that is not a wizard answer — no kind asks for one — so it
// reaches the Channel from the caller or not at all. The fixture kind sets no
// role, which is exactly the case where the CRD's `both` default would
// silently override a declared one.
func TestWireChannels_StampsTheDeclaredRole(t *testing.T) {
	ctx := context.Background()
	w, dyn := wiringForTest(t)
	plan := planFor(wizardlessKind, "demo-agent-out")
	plan.Required.Role = spiceboxv1alpha1.ChannelRoleOutput

	_, err := wireChannels(ctx, w, []channelplan.ChannelPlan{plan})
	require.NoError(t, err)

	got, getErr := dyn.Resource(channelGVR).Namespace("default").Get(ctx, "fake-channel", metav1.GetOptions{})
	require.NoError(t, getErr)
	assert.Equal(t, spiceboxv1alpha1.ChannelRoleOutput, got.Object["spec"].(map[string]any)["role"],
		"a bundle that declares role: output must not get a Channel that also accepts input")
}

// TestWireChannels_ReportsAChannelThatDidNotTakeTheDeclaredName: the declared
// name is what removes the -creds footgun the whole feature exists for, so a
// kind whose manifests fix their own name has to be called out rather than
// counted as a clean wire.
func TestWireChannels_ReportsAChannelThatDidNotTakeTheDeclaredName(t *testing.T) {
	w, _ := wiringForTest(t)

	rep, err := wireChannels(context.Background(), w,
		[]channelplan.ChannelPlan{planFor(wizardlessKind, "demo-agent-first")})

	require.NoError(t, err, "the Channel exists, so this is a warning and not a failure")
	assert.Equal(t, []string{"demo-agent-first"}, rep.Wired())
	assert.Contains(t, rep.String(), `named this Channel "fake-channel", not the declared "demo-agent-first"`)
	assert.Contains(t, rep.String(), "demo-agent-first-creds",
		"the warning must name the Secret the rest of the bundle is reading")
}

// TestWireChannels_NoDeclarationsIsNotAnError: a bundle that declares no
// channel must be untouched by any of this, including by the cluster checks —
// which is what keeps every bundle written before requires.channels existed
// installing exactly as it did.
func TestWireChannels_NoDeclarationsIsNotAnError(t *testing.T) {
	rep, err := wireChannels(context.Background(), channelWiring{}, nil)
	require.NoError(t, err, "an empty plan must not even require a cluster")
	assert.Empty(t, rep.String())
	assert.Empty(t, rep.Wired())
}

// TestWireChannels_AsksEveryChannelInOnePlanDerivedFrame is the presentation
// defect whole.
//
// What the operator saw was `oap · agent install` printed again for every
// screen batch of every channel, with nothing between them saying which
// channel they were answering for or how many were left. That was not a
// missing Title: this pass presents over the driver the BUNDLE questions built
// (they share one stdin — installQuestionPresentation), and a shared driver
// carries the chrome it was built with, so tui.Options.Title is never read at
// all. The channels were being framed as more install questions.
//
// Three claims, and each is a different way the fix could be got wrong:
//
//   - ONE Chrome spans the pass. Rebuilding one per channel would still title
//     every frame the same and still look fixed, while restarting the rail —
//     so the identity of the pointer is the assertion, not the title.
//   - The rail comes from the PLAN, not from the channels this run got as far
//     as asking about. The already-wired channel below is never presented and
//     must still be on the rail; an operator can only see the whole pass if
//     the steps are the whole declaration.
//   - Each channel's screens are pinned to that channel's step. The screens
//     are a kind's own question names, which the rail has never heard of, so
//     without pinning every step renders pending and the rail says nothing.
func TestWireChannels_AsksEveryChannelInOnePlanDerivedFrame(t *testing.T) {
	w, _ := wiringForTest(t)
	d := newFramingDriver()
	w.driver = d
	w.theme = tui.NewTheme(tui.Caps{Width: 80})

	// A channel this run never asks about, declared FIRST so its absence from
	// the rail could not be mistaken for a trailing-element bug.
	alreadyWired := planFor(wizardlessKind, "demo-agent-out")
	alreadyWired.AlreadyWired = true

	_, err := wireChannels(context.Background(), w, []channelplan.ChannelPlan{
		alreadyWired,
		planFor(questionsKind, "demo-agent-trigger"),
		planFor(questionsKind, "demo-agent-second"),
	})
	require.NoError(t, err)

	asked := *d.asked
	require.NotEmpty(t, asked, "the two bento channels leave questions unseeded, so screens must be presented")

	frames := map[*tui.Chrome]int{}
	for _, a := range asked {
		require.NotNil(t, a.chrome, "screen %q was presented with no frame at all", a.screenID)
		frames[a.chrome]++
	}
	require.Len(t, frames, 1,
		"one presentation must span the pass; %d distinct frames means one was built per channel", len(frames))

	chrome := asked[0].chrome
	header := strings.SplitN(chrome.Render(0, "BODY"), "\n", 2)[0]
	assert.Equal(t, "oap · agent install · channels", header)
	assert.NotEqual(t, installQuestionTitle, header,
		"the bundle questions' title is what the operator saw five times over; this pass has its own")

	// The rail is the plan: every declared channel, in declaration order,
	// including the one nothing was asked for.
	for i, name := range []string{"demo-agent-out", "demo-agent-trigger", "demo-agent-second"} {
		assert.Equal(t, i, chrome.StepIndex(name), "the rail must carry %q at its declared position", name)
	}

	// And every screen is drawn under its own channel's step.
	for _, a := range asked {
		assert.Contains(t, []string{"demo-agent-trigger", "demo-agent-second"}, a.step,
			"screen %q must be pinned to the channel it is being asked for", a.screenID)
	}
	assert.Equal(t, "demo-agent-trigger", asked[0].step, "the first channel asked is the first declared one that asks")
	assert.Equal(t, "demo-agent-second", asked[len(asked)-1].step,
		"and the pass moves the active step on rather than staying on the first")
}

// TestChannelsChrome_IsNotAcquiredByARunThatCannotDrawIt: a scripted install
// has no driver and no theme, and tui.NewChrome dereferences its theme for the
// terminal width. A frame built for a run that will never render one is a
// panic waiting on a code path that only exists to skip every channel.
func TestChannelsChrome_IsNotAcquiredByARunThatCannotDrawIt(t *testing.T) {
	plans := []channelplan.ChannelPlan{planFor(questionsKind, "demo-agent-trigger")}
	theme := tui.NewTheme(tui.Caps{Width: 80})

	assert.Nil(t, channelsChrome(plans, nil, nil), "nobody at the terminal: no driver, no theme, no frame")
	assert.Nil(t, channelsChrome(plans, theme, nil), "a theme without a driver is still nobody at the terminal")
	assert.Nil(t, channelsChrome(nil, theme, refusingDriver{t: t}), "a bundle declaring nothing has no pass to frame")
	assert.NotNil(t, channelsChrome(plans, theme, refusingDriver{t: t}))
}

// TestChannelStepID_CutsALongChannelNameToTheRailsBudget: declared Channel
// names run longer than the question prompts this rail was built for
// (`demo-reviewbot-slack` is twenty columns), and Chrome drops the rail
// ENTIRELY once its widest label leaves no usable body column — so one long
// name would cost every channel its rail rather than overflowing its own row.
func TestChannelStepID_CutsALongChannelNameToTheRailsBudget(t *testing.T) {
	long := "demo-reviewbot-github-pull-requests"
	require.Greater(t, len(long), tui.MaxRailLabelColumns, "precondition: the fixture must exceed the budget")

	theme := tui.NewTheme(tui.Caps{Width: 80})
	chrome := channelsChrome([]channelplan.ChannelPlan{planFor(questionsKind, long)}, theme, refusingDriver{t: t})
	require.NotNil(t, chrome)

	rendered := chrome.Render(0, "BODY")
	assert.Contains(t, rendered, "BODY", "the rail must not have squeezed the form body out of the frame")
	assert.Contains(t, rendered, long[:tui.MaxRailLabelColumns],
		"the label is cut, and cut from the end so the distinguishing head survives")

	// The ID stays whole even though the label is cut: it is what the pass
	// resolves the active step by, and two channels sharing a cut prefix must
	// still be told apart.
	assert.Equal(t, 0, chrome.StepIndex(long))
}

// TestWireChannels_RefusesWithoutACluster: every failure this pass reports is
// about a channel, so a missing cluster connection must be reported as the
// wiring bug it is rather than as N channels that mysteriously could not be
// created.
func TestWireChannels_RefusesWithoutACluster(t *testing.T) {
	rep, err := wireChannels(context.Background(), channelWiring{out: io.Discard},
		[]channelplan.ChannelPlan{planFor(wizardlessKind, "demo-agent-first")})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "no cluster connection")
	assert.Empty(t, rep.outcomes, "nothing was attempted, so nothing may be reported about it")
}

// channelDeclaringBundleDir writes a minimal .oap source folder that DECLARES
// channels: one AgentClass, and requires.channels entries that are otherwise
// well-formed, so the only thing wrong with a `--name` install of it is the
// combination itself.
//
// TWO channels, in reviewbot's own shape — an input and the output it delivers
// to — because one is not a well-formed declaration any more: B-R14 refuses an
// input channel with no role=output sibling, since the Channel controller
// resolves an input channel's delivery target at reconcile time and matches
// role=output alone. A bento-only bundle describes an agent that is woken on a
// schedule and can answer nowhere.
//
// Package-local rather than built on the shared oaptest fixture for the same
// reason capacityBundleDir is: that fixture's manifest carries required
// questions of its own, which would make this test answer bundle concerns
// unrelated to the property under test.
func channelDeclaringBundleDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "manifests"), 0o755))
	// agent.name is inherited from the bundled AgentClass below.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "oap.yaml"), []byte(`
oapFormatVersion: "1"
agent:
  version: "1.0.0"
requires:
  channels:
    - kind: bento
      role: input
      name: demo-agent-trigger
      purpose: "Starts one session on a schedule."
    - kind: fake
      role: output
      name: demo-agent-out
      purpose: "Where this agent's work is delivered."
`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "manifests", "agent.yaml"), []byte(`
apiVersion: agentprimitives.authzed.com/v1alpha1
kind: AgentClass
metadata:
  name: `+channelBundleAgent+`
spec:
  description: Fixture agent for the declared-channel install tests.
`), 0o644))
	return dir
}

const channelBundleAgent = "chan-fixture-agent"

// TestAgentInstall_NamedInstallOfAChannelBundleIsRefusedBeforeAnyClusterWrite
// pins the PLACEMENT of checkDeclaredChannels, which is the entire reason
// RefuseNamedInstall is exported at all.
//
// Reached only through PlanChannels, the refusal fires after install.Install
// has already applied every CR — the plan is made after the install lands,
// because a channel wizard binds to an AgentClass that does not exist until
// then. So the operator would be told their flag combination cannot work
// while looking at the agent it just installed.
//
// Nothing but the call's position enforces that, and a future edit moving it
// below install.Install is silent: the command still errors, still with the
// right words. Only the cluster can tell the difference, which is why this
// asserts on the cluster and not on the message.
func TestAgentInstall_NamedInstallOfAChannelBundleIsRefusedBeforeAnyClusterWrite(t *testing.T) {
	kb := fakeBundle(t)
	out, err := runAgentInstall(t, aptest.GlobalsFor(kb), channelDeclaringBundleDir(t),
		"--name", "second-instance")

	require.Error(t, err, "--name cannot be combined with a bundle that declares channels")
	assert.Contains(t, err.Error(), "second-instance")
	assert.Contains(t, err.Error(), "demo-agent-trigger",
		"the refusal names the declaration it collides with")

	// The claim is that NOTHING was written, and only the cluster can say so.
	got := &unstructured.Unstructured{}
	got.SetAPIVersion(spiceboxv1alpha1.SchemeGroupVersion.String())
	got.SetKind("AgentClass")
	getErr := kb.Controller.Get(context.Background(),
		client.ObjectKey{Namespace: kb.Namespace, Name: "second-instance-" + channelBundleAgent}, got)
	require.Error(t, getErr, "the refusal must precede the first cluster write")
	assert.True(t, apierrors.IsNotFound(getErr),
		"expected the AgentClass to be absent, got a different failure: %v", getErr)
	assert.NotContains(t, out, "Installed ", "and the command must not report an install it did not do")
}

// TestAgentInstallDoesNotOfferTheChannelCreateFlags pins the fact every
// "never say --answer" comment and NotContains in this package rests on.
//
// Those guards are written as prose claims about a flag set defined in another
// function, and prose does not fail when the flag set changes. Register
// `--answer` on `oap agent install` tomorrow and the guards keep passing while
// the reasoning behind them is silently false.
//
// `--name` is the interesting row and the reason this is not a two-line test.
// It IS registered here — it names the install instance — and
// checkDeclaredChannels refuses it outright for a bundle that declares
// channels. So the wizard's `--name <value>` advice is worse than advice for a
// flag that does not exist: it names one the operator can type and this
// command will reject for this very bundle.
func TestAgentInstallDoesNotOfferTheChannelCreateFlags(t *testing.T) {
	var install *cobra.Command
	for _, c := range NewCmd(&apcmd.Globals{}).Commands() {
		if c.Name() == "install" {
			install = c
		}
	}
	require.NotNil(t, install, "oap agent install must exist for this claim to mean anything")

	assert.Nil(t, install.Flags().Lookup("answer"),
		"channelwizard's refusal names --answer; if install ever registers one, the refusal stops being wrong and these guards need rewriting rather than deleting")
	assert.Nil(t, install.Flags().Lookup("non-interactive"),
		"same for --non-interactive: the fail-closed refusal tells the operator to drop it")
	assert.NotNil(t, install.Flags().Lookup("name"),
		"--name IS install's own flag, with a different meaning — see this test's doc")
}

// TestAgentInstall_ScriptedInstallOfAChannelBundleExitsZero is spec §2.2 where
// an operator actually experiences it: the exit code of the command, not a
// classification inside a helper.
//
// wireChannels' own tests prove the classification and cannot prove the join —
// wireDeclaredChannels has to return that verdict unchanged, and nothing above
// it in RunE may re-fail on the way out. "Each half is right, the join is
// unguarded" is the shape most of this branch's defects have taken, so this
// asserts at cobra's Execute, on the error a shell turns into $?.
//
// The exit-code assertion is FIRST here, and that is the opposite of the
// ordering in NoOneAtStdinIsSkipped… above, deliberately: there the require
// would have hidden the vocabulary guards, which are that test's whole
// payload; here the exit code IS the payload, and the fold-Skipped-into-Failed
// control has to redden on this line specifically.
func TestAgentInstall_ScriptedInstallOfAChannelBundleExitsZero(t *testing.T) {
	// Pins the process's real os.Stdin to a pipe, which is what makes
	// installQuestionPresentation hand back a nil driver. That is the same
	// condition a CI job or the UI's ref-based install arrives in, reached the
	// same way rather than simulated by poking a field.
	forceNonInteractiveStdin(t)
	kb := fakeBundle(t)

	out, err := runAgentInstall(t, aptest.GlobalsFor(kb), channelDeclaringBundleDir(t))

	require.NoError(t, err, "a scripted install of a channel-declaring bundle must exit 0")

	// The agent really was installed. A command that "skipped the channel" by
	// doing nothing at all would also exit 0, so the exit code alone is not
	// the claim.
	got := &unstructured.Unstructured{}
	got.SetAPIVersion(spiceboxv1alpha1.SchemeGroupVersion.String())
	got.SetKind("AgentClass")
	require.NoError(t, kb.Controller.Get(context.Background(),
		client.ObjectKey{Namespace: kb.Namespace, Name: channelBundleAgent}, got),
		"the agent is installed; only its channel is skipped")

	// The gap is NAMED, in this command's own words, with a command the
	// operator can paste. The namespace is part of that: this install went to
	// kb.Namespace, which is not the ambient default, so a come-back missing
	// it would create the Channel where nothing is looking for it.
	assert.Contains(t, out, "2 declared, 0 wired, 0 already wired, 2 skipped, 0 not wired",
		"the summary must account for every channel it could not wire")
	assert.Contains(t, out, "cannot prompt",
		"a silent skip is indistinguishable from a bug")
	// --role is part of the paste, not an ornament. No kind's wizard asks for
	// a role, so a command that cannot express the declared one creates a
	// Channel on ChannelSpec.Role's `both` default — which outputbind
	// deliberately does not match — and the operator's paste silently produces
	// something other than what this install would have made. The whole string
	// is asserted, because a Contains over the prefix would keep passing with
	// the role dropped.
	assert.Contains(t, out,
		"oap channel create --kind bento --name demo-agent-trigger --namespace "+kb.Namespace+" --role input")
	assert.Contains(t, out,
		"oap channel create --kind fake --name demo-agent-out --namespace "+kb.Namespace+" --role output",
		"the delivery channel is skipped too, and its role is the half that cannot be guessed")
	assert.NotContains(t, out, "--answer",
		"an install must never tell its operator to pass a flag it does not have")

	// And nothing was created behind that report.
	assert.Empty(t, fakeDynamicOf(t, kb).Actions(),
		"a scripted install must not create the Channel it just said it skipped")
}

// TestCheckDeclaredChannels covers what `oap agent install` refuses BEFORE it
// writes anything, all of which were previously reachable only after the CRs
// had landed (or not at all).
//
// THE FIXTURE CARRIES CRs, and that is not scenery. The credentials
// cross-check — the finding the whole declaration feature exists for — walks
// the bundled AgentIdentities, so a bundle with none gives it nothing to
// compare and the check passes vacuously. Running it only through
// `oap agent lint` would leave the INSTALL path, which is the one the spec
// cares about, untested for it.
func TestCheckDeclaredChannels(t *testing.T) {
	// One AgentClass and one AgentIdentity whose credential reads
	// "<credsFor>-creds". Named after a channel the caller declares, this is a
	// clean bundle; named after one it does not, it is the dangling reference.
	bundleCRs := func(credsFor string) []byte {
		return []byte(`apiVersion: agentprimitives.authzed.com/v1alpha1
kind: AgentClass
metadata:
  name: demo-agent
spec:
  description: Fixture agent for the declared-channel checks.
---
apiVersion: agentprimitives.authzed.com/v1alpha1
kind: AgentIdentity
metadata:
  name: demo-agent-id
spec:
  credentials:
    - name: channel-creds
      type: static
      static:
        secretRef:
          name: ` + credsFor + `-creds
          key: token
`)
	}
	// The delivery target every input declaration below needs: B-R14 refuses
	// an input channel with no role=output sibling, so a fixture without one
	// would raise a second finding in every subtest and each would stop being
	// about the one thing it names.
	delivery := oap.RequiredChannel{Kind: "fake", Role: spiceboxv1alpha1.ChannelRoleOutput, Name: "demo-agent-out"}
	bundleWith := func(chans ...oap.RequiredChannel) *oap.Bundle {
		if len(chans) > 0 {
			chans = append(chans, delivery)
		}
		return &oap.Bundle{
			Manifest: &oap.Manifest{
				OapFormatVersion: "1",
				Agent:            oap.Agent{Name: "demo-agent", Version: "1.0.0"},
				Requires:         oap.Requires{Channels: chans},
			},
			// Every declaration below that is otherwise well-formed uses this
			// name, so the cross-check is satisfied and each subtest fails for
			// the one reason it is about.
			Manifests: bundleCRs("demo-agent-trigger"),
		}
	}
	good := oap.RequiredChannel{Kind: "bento", Role: spiceboxv1alpha1.ChannelRoleInput, Name: "demo-agent-trigger"}

	t.Run("no declared channels: nothing is checked, --name is fine", func(t *testing.T) {
		require.NoError(t, checkDeclaredChannels(bundleWith(), "second-instance"))
	})

	t.Run("--name with a declared channel: refused, naming the flag and the channel", func(t *testing.T) {
		err := checkDeclaredChannels(bundleWith(good), "second-instance")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "second-instance")
		assert.Contains(t, err.Error(), "demo-agent-trigger")
		assert.Contains(t, err.Error(), "oap channel create",
			"the refusal must name a route that works")
	})

	t.Run("an unregistered kind: refused before the cluster, naming the kind", func(t *testing.T) {
		err := checkDeclaredChannels(bundleWith(oap.RequiredChannel{
			Kind: "no-such-kind", Role: spiceboxv1alpha1.ChannelRoleInput, Name: "demo-agent-trigger",
		}), "")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no-such-kind")
		assert.Contains(t, err.Error(), "requires.channels[0].kind")
	})

	t.Run("a role the kind does not serve: refused, naming what it does serve", func(t *testing.T) {
		err := checkDeclaredChannels(bundleWith(oap.RequiredChannel{
			Kind: "bento", Role: spiceboxv1alpha1.ChannelRoleOutput, Name: "demo-agent-trigger",
		}), "")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "does not serve role")
	})

	t.Run("a credential reading a Secret no declared channel produces: refused, naming both strings", func(t *testing.T) {
		b := bundleWith(good)
		b.Manifests = bundleCRs("demo-agent-github")

		err := checkDeclaredChannels(b, "")
		require.Error(t, err, "this is THE defect requires.channels exists to catch; it must not pass on the install path")
		assert.Contains(t, err.Error(), "demo-agent-github-creds",
			"the finding names the Secret the credential reads")
		assert.Contains(t, err.Error(), "demo-agent-trigger",
			"and the channel the manifest declares, because either one could be the typo")
		assert.Contains(t, err.Error(), "spec.credentials[0].static.secretRef.name",
			"and where to fix it")
	})

	t.Run("a well-formed declaration passes", func(t *testing.T) {
		require.NoError(t, checkDeclaredChannels(bundleWith(good), ""))
	})
}

// ————————————————————————————————————————————————————————————————————————
// The on-demand tunnel: a declared webhook channel needs this cluster to have
// a public address BEFORE its wizard registers anything with a third party.
// ————————————————————————————————————————————————————————————————————————

const (
	// desktopLoopbackURL is what `oap desktop` leaves in webd's external-URL
	// ConfigMap: 127.0.0.1 on the port it picked at runtime. Nothing outside
	// the machine owns that value, so a tunnel may take it over — and it is
	// what a created endpoint carries as spec.localURL.
	desktopLoopbackURL = "http://127.0.0.1:17080"
	// gatewayHostURL is what `oap install --trusted-hostname` leaves there
	// instead: a real https:// host with a Gateway and a certificate behind
	// it, which nothing here may seize.
	gatewayHostURL = "https://webd.demo.test"
	// fixtureTunnelURL is the address the reconciler stand-in publishes.
	fixtureTunnelURL = "https://demo-tunnel.demo.test"
)

// tunnelComesUp / tunnelNeverReady name endpointWiring's last argument at its
// call sites, because `false` alone reads as nothing.
const (
	tunnelComesUp    = true
	tunnelNeverReady = false
)

// publishOnCreate is the PublicEndpoint controller's visible effect, in one
// interceptor: the endpoint it is handed goes Ready with a public URL, and
// webd's external-URL ConfigMap carries that URL in BOTH keys.
//
// Without it every created endpoint would sit at its zero-valued status
// forever, and the wait for Ready — the thing this fixture exists to exercise —
// could only ever be tested in the direction that times out.
//
// THE CONFIGMAP HALF IS NOT DECORATION. That ConfigMap is where the planner
// reads a channel's external-base-url from, so a fixture that published only
// status would leave the loopback standing and could not tell a pass that
// plans after the tunnel from one that plans before it. The controller writes
// the ConfigMap before it writes status (see publishExternalURL's call site),
// which is what makes "Ready" a safe signal to plan on; this reproduces that
// order.
func publishOnCreate(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
	pe, isEndpoint := obj.(*spiceboxv1alpha1.PublicEndpoint)
	if isEndpoint {
		pe.Status.Phase = spiceboxv1alpha1.PublicEndpointPhaseReady
		pe.Status.URL = fixtureTunnelURL
	}
	if err := c.Create(ctx, obj, opts...); err != nil {
		return err
	}
	if !isEndpoint {
		return nil
	}
	var cm corev1.ConfigMap
	key := types.NamespacedName{
		Namespace: cloud.WebdServiceNamespace,
		Name:      spiceboxv1alpha1.WebdExternalURLConfigMap,
	}
	if err := c.Get(ctx, key, &cm); err != nil {
		return err
	}
	cm.Data[spiceboxv1alpha1.WebdTrustedURLKey] = fixtureTunnelURL
	cm.Data[spiceboxv1alpha1.WebdSandboxURLKey] = fixtureTunnelURL
	return c.Update(ctx, &cm)
}

// bundleDeclaring is the bundle an `oap agent install` of a channel-declaring
// agent carries: one requires.channels entry, and nothing else this decision
// reads.
func bundleDeclaring(kind, name string) *oap.Bundle {
	return &oap.Bundle{Manifest: &oap.Manifest{
		Requires: oap.Requires{Channels: []oap.RequiredChannel{{
			Kind: kind, Name: name, Role: spiceboxv1alpha1.ChannelRoleBoth,
		}}},
	}}
}

// planDeclared runs the legacy direct-install sequence — bring the address up,
// then make the read-only channel plan — for one declared channel.
func planDeclared(t *testing.T, w channelWiring, kind, name string) ([]channelplan.ChannelPlan, error) {
	t.Helper()
	bundle := bundleDeclaring(kind, name)
	if err := w.ensureWebhookAddress(context.Background(), bundle.Manifest.Requires.Channels); err != nil {
		return nil, err
	}
	return w.planDeclaredChannels(context.Background(), bundle, fixtureAgentClass, "")
}

// webdExternalURLConfigMap is the ConfigMap `oap install` seeds and the desktop
// updates. Its trusted-url key is where the on-demand path reads the address
// webd answers on locally, and how it tells whether anything else owns it.
func webdExternalURLConfigMap(trustedURL string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      spiceboxv1alpha1.WebdExternalURLConfigMap,
			Namespace: cloud.WebdServiceNamespace,
		},
		Data: map[string]string{
			spiceboxv1alpha1.WebdTrustedURLKey: trustedURL,
			spiceboxv1alpha1.WebdSandboxURLKey: trustedURL,
		},
	}
}

// endpointWiring is wiringForTest for the tunnel decision: a wiring whose
// cluster kind is kindKey, over a cluster whose webd external-URL ConfigMap
// carries trustedURL, and which behaves like the PublicEndpoint controller when
// tunnelUp.
//
// The driver is nil — an unattended run — for the same reason
// TestWireChannels_NonInteractiveSkipsAndNamesTheCommands relies on: every
// fixture below declares a channel whose flow a unit test could never drive,
// and the endpoint decision is made from the PLAN, before the pass, so it is
// reachable without entering one.
//
// The ready timeout is milliseconds rather than the production bound, so the
// case that never goes Ready is a fast assertion rather than a slow one.
func endpointWiring(t *testing.T, kindKey, trustedURL string, tunnelUp bool) channelWiring {
	t.Helper()
	objs := []client.Object{
		&spiceboxv1alpha1.AgentClass{ObjectMeta: metav1.ObjectMeta{Name: fixtureAgentClass, Namespace: "default"}},
		webdExternalURLConfigMap(trustedURL),
	}
	kb, _ := aptest.NewFakeBundle(t, map[schema.GroupVersionResource]string{
		channelGVR:    "ChannelList",
		agentClassGVR: "AgentClassList",
		secretGVR:     "SecretList",
	}, objs...)

	cb := ctrlfake.NewClientBuilder().WithScheme(aptest.Scheme(t)).WithObjects(objs...)
	if tunnelUp {
		cb = cb.WithInterceptorFuncs(interceptor.Funcs{Create: publishOnCreate})
	}
	kb.Controller = cb.Build()

	return channelWiring{
		kube:                 kb,
		in:                   strings.NewReader(""),
		out:                  io.Discard,
		theme:                tui.NewTheme(tui.Caps{}),
		driver:               nil,
		strat:                cloud.MustFor(kindKey),
		endpointReadyTimeout: 200 * time.Millisecond,
	}
}

// endpointExists reports whether the pass left webd's PublicEndpoint on the
// cluster, which is the whole observable of this decision.
func endpointExists(t *testing.T, w channelWiring) bool {
	t.Helper()
	var pe spiceboxv1alpha1.PublicEndpoint
	err := w.kube.Controller.Get(context.Background(), types.NamespacedName{Name: publicendpoint.WebdName}, &pe)
	if apierrors.IsNotFound(err) {
		return false
	}
	require.NoError(t, err, "reading webd's PublicEndpoint")
	return true
}

// webhookFixtureKind is a registered channel kind that receives webhooks and is
// not github.
//
// It exists because github is the ONLY registered kind with a non-nil
// WebhookReceiver, so a test using github alone cannot tell the seam apart from
// `kind == "github"`. Everything but the two methods below is the fake kind's,
// because none of the rest is what is under test.
type webhookFixtureKind struct {
	fakekind.Kind
	name string
}

func (k webhookFixtureKind) Name() string { return k.name }

func (webhookFixtureKind) WebhookReceiver(channelkinds.Deps) channelkinds.WebhookReceiver {
	return webhookFixtureReceiver{}
}

// webhookFixtureReceiver is a receiver that is never called: the decision under
// test is whether a kind HAS one, and no delivery is made in a unit test.
type webhookFixtureReceiver struct{}

func (webhookFixtureReceiver) Verify(context.Context, channelkinds.WebhookSecrets, channelkinds.WebhookRequest) error {
	return nil
}

func (webhookFixtureReceiver) Translate(context.Context, *spiceboxv1alpha1.Channel, channelkinds.WebhookRequest) (*channelkinds.WebhookInbound, error) {
	return nil, nil
}

// registerFakeWebhookKind registers the fixture kind in the REAL registry, so
// the lookup under test is the one production does, and restores the registry
// afterwards.
//
// Restore is snapshot-and-reinstate rather than a bare Reset: this package's
// own blank imports register the real kinds at init, and a Reset that did not
// put them back would leave every later test in this binary running against an
// empty registry — order-dependent green.
//
// Registry mutation is process-global, so no test that calls this may be
// parallel.
func registerFakeWebhookKind(t *testing.T, name string) {
	t.Helper()
	before := registry.All()
	registry.Register(webhookFixtureKind{name: name})
	t.Cleanup(func() {
		registry.Reset()
		for _, k := range before {
			registry.Register(k)
		}
	})
}

// TestPlanDeclaredChannels_SeedsTheTunnelsURLNotTheAddressItReplaced is the
// join this whole ordering exists for: the plan a declared webhook channel is
// wired from must carry the address the tunnel just published, not the one the
// cluster answered on before it existed.
//
// The plan is built by the REAL planner, so external-base-url is seeded the way
// production seeds it — from webd's external-URL ConfigMap, with the provenance
// note that says so. Asserting on a hand-built plan would prove nothing: a
// fixture that seeds only the channel name and the AgentClass never carries the
// key this is about.
//
// On the headline path this guards — a desktop, a bundle declaring github, an
// ngrok token present — planning first meant the pass printed "PublicEndpoint
// webd is Ready at https://…" and then handed the wizard http://127.0.0.1:17080,
// which the github wizard refuses outright.
func TestPlanDeclaredChannels_SeedsTheTunnelsURLNotTheAddressItReplaced(t *testing.T) {
	w := endpointWiring(t, cloud.KeyDesktop, desktopLoopbackURL, tunnelComesUp)

	plans, err := planDeclared(t, w, "github", "demo-agent-gh")
	require.NoError(t, err)
	require.Len(t, plans, 1)

	assert.Equal(t, fixtureTunnelURL, plans[0].Seeded[wizardkeys.KeyExternalBaseURL],
		"the wizard registers this address with GitHub, so it must be the tunnel's, not the loopback the tunnel replaced")
	assert.NotContains(t, plans[0].Seeded[wizardkeys.KeyExternalBaseURL], "127.0.0.1",
		"a loopback address is one GitHub refuses outright")
	assert.NotEmpty(t, plans[0].SeededFrom[wizardkeys.KeyExternalBaseURL],
		"the value keeps the planner's own provenance: it is still read from the ConfigMap, just after the tunnel filled it in")
}

// TestPlanDeclaredChannels_CreatesAnEndpointForADeclaredWebhookChannel is the
// bootstrapping requirement: the wizard needs a public URL BEFORE it registers
// the App, so the trigger is the DECLARATION of a webhook channel, not the
// Channel's existence — which is a thing that does not exist yet at this point
// in the install.
func TestPlanDeclaredChannels_CreatesAnEndpointForADeclaredWebhookChannel(t *testing.T) {
	w := endpointWiring(t, cloud.KeyDesktop, desktopLoopbackURL, tunnelComesUp)

	_, err := planDeclared(t, w, "github", "demo-agent-gh")
	require.NoError(t, err)

	assert.True(t, endpointExists(t, w),
		"github needs inbound webhooks, so the URL must exist before the App is registered")
}

// TestPlanDeclaredChannels_CreatesNoEndpointForAChannelThatNeedsNoWebhook: a
// tunnel is an unannounced inbound path from the public Internet. Opening one
// for a channel that dials OUT is a surprise, not a convenience.
func TestPlanDeclaredChannels_CreatesNoEndpointForAChannelThatNeedsNoWebhook(t *testing.T) {
	w := endpointWiring(t, cloud.KeyDesktop, desktopLoopbackURL, tunnelComesUp)

	_, err := planDeclared(t, w, "slack", "demo-agent-slack")
	require.NoError(t, err)

	assert.False(t, endpointExists(t, w),
		"a desktop with no webhook channel never opens a tunnel")
}

// TestPlanDeclaredChannels_AsksTheRegistryNotTheKindName is what makes the seam
// observable. github is the only registered kind that receives webhooks, so
// with it alone this decision is indistinguishable from `kind == "github"`; a
// second webhook-needing kind, registered the way a real one would be, is the
// difference.
func TestPlanDeclaredChannels_AsksTheRegistryNotTheKindName(t *testing.T) {
	registerFakeWebhookKind(t, "demo-hook")
	w := endpointWiring(t, cloud.KeyDesktop, desktopLoopbackURL, tunnelComesUp)

	_, err := planDeclared(t, w, "demo-hook", "demo-agent-hook")
	require.NoError(t, err)

	assert.True(t, endpointExists(t, w),
		"a new webhook kind must get the tunnel for free, without this code naming it")
}

// TestPlanDeclaredChannels_TimesOutLoudlyWhenTheEndpointNeverGoesReady. A
// cluster with no ngrok credential never reaches Ready — the endpoint sits at
// Pending by design — so an unbounded wait would hang `oap agent install` with
// nothing on screen, which is the silent-hang failure this repo has a standing
// rule against.
func TestPlanDeclaredChannels_TimesOutLoudlyWhenTheEndpointNeverGoesReady(t *testing.T) {
	w := endpointWiring(t, cloud.KeyDesktop, desktopLoopbackURL, tunnelNeverReady)

	_, err := planDeclared(t, w, "github", "demo-agent-gh")

	require.Error(t, err, "a desktop with no ngrok credential must not hang the install")
	assert.Contains(t, err.Error(), "public endpoint")
	assert.Contains(t, err.Error(), publicendpoint.NgrokAuthTokenSecret,
		"the refusal must say what to do, and the missing credential is the usual cause")
	assert.True(t, endpointExists(t, w),
		"the endpoint is left in place: the operator supplies the token and the controller finishes the job")
}

// TestPlanDeclaredChannels_CreatesNoEndpointOnAKindWhosePolicyIsNotOnDemand.
// The policy is asked SEPARATELY from whether a webhook is declared, and both
// halves are load-bearing: `local` already had an endpoint created at install
// time, and every durable kind reaches the Internet through its own ingress,
// where a tunnel would be an unannounced inbound path into production
// infrastructure.
func TestPlanDeclaredChannels_CreatesNoEndpointOnAKindWhosePolicyIsNotOnDemand(t *testing.T) {
	cases := []struct {
		name string
		kind string
	}{
		{name: "local: install already created one, so this pass creates nothing", kind: cloud.KeyLocal},
		{name: "gke: real ingress, so a tunnel is refused outright", kind: cloud.KeyGKE},
		{name: "default: real ingress, so a tunnel is refused outright", kind: cloud.KeyDefault},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := endpointWiring(t, tc.kind, desktopLoopbackURL, tunnelComesUp)

			_, err := planDeclared(t, w, "github", "demo-agent-gh")
			require.NoError(t, err, "a kind that opens no tunnel is not a failed install")

			assert.False(t, endpointExists(t, w),
				"only a kind whose policy is OnDemand opens a tunnel here")
		})
	}
}

// TestPlanDeclaredChannels_CreatesNoEndpointWhenWebdsExternalURLAlreadyHasAnOwner
// is webdExternalURLHasAnotherOwner's question, arriving by a different path. A
// desktop installed with --trusted-hostname has a Gateway, a certificate and a
// real https:// host in the two ConfigMap keys; an endpoint created here would
// hand them to a controller that re-applies them under ForceOwnership every
// reconcile, rewriting a working public hostname to a loopback address.
//
// And the channel loses nothing by it: a cluster with a real external host
// already has an address GitHub can deliver to.
func TestPlanDeclaredChannels_CreatesNoEndpointWhenWebdsExternalURLAlreadyHasAnOwner(t *testing.T) {
	cases := []struct {
		name       string
		trustedURL string
	}{
		{name: "a real external host is not ours to seize", trustedURL: gatewayHostURL},
		{name: "an empty value is the Gateway's seed, still waiting to be filled", trustedURL: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := endpointWiring(t, cloud.KeyDesktop, tc.trustedURL, tunnelComesUp)

			_, err := planDeclared(t, w, "github", "demo-agent-gh")
			require.NoError(t, err)

			assert.False(t, endpointExists(t, w),
				"webd's external URL has an owner already; a tunnel would take it over")
		})
	}
}
