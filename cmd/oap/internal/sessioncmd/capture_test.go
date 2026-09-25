package sessioncmd

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/kube"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	bt "github.com/authzed/openagentprimitives/pkg/bronzethread"
	"github.com/authzed/openagentprimitives/pkg/controllers/agentsession/cosidecar"
	"github.com/authzed/openagentprimitives/pkg/steelthread"
)

// TestSessionCaptureCmd_FlagWiring pins the surface the command promises. The
// output flag being REQUIRED is the load-bearing half: without it the command
// would run a whole capture — a port-forward, a SpiceDB dial, a full read — and
// then have nowhere to put the answer.
func TestSessionCaptureCmd_FlagWiring(t *testing.T) {
	cmd := newSessionCaptureCmd(nil)

	assert.Equal(t, "capture <session>", cmd.Use)
	// require, not assert: the two lines below dereference the --output lookup,
	// so a missing flag would panic instead of reporting itself.
	for _, name := range []string{"output", "name", "description", "overwrite", "redact", "redact-from", "elide-skill"} {
		require.NotNil(t, cmd.Flags().Lookup(name), "flag --%s must exist", name)
	}
	assert.Equal(t, "o", cmd.Flags().Lookup("output").Shorthand,
		"the tree gives --output the -o shorthand everywhere; see TestFlagSpellingIsConsistentAcrossTheTree")
	assert.Nil(t, cmd.Flags().Lookup("out"), "--out is the banned spelling of --output")

	required := cmd.Flags().Lookup("output").Annotations[cobra.BashCompOneRequiredFlag]
	assert.Equal(t, []string{"true"}, required, "--output must be required")

	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"demo-session"})
	err := cmd.Execute()
	require.Error(t, err, "a capture with no --output must fail before it touches a cluster")
	assert.Contains(t, err.Error(), "output")
}

// TestSessionCaptureCmd_IsRegistered pins that the command is reachable. A
// command nobody wired is a feature that exists only in its own test file.
func TestSessionCaptureCmd_IsRegistered(t *testing.T) {
	var found bool
	for _, c := range NewCmd(nil).Commands() {
		if c.Name() == "capture" {
			found = true
		}
	}
	assert.True(t, found, "`oap session capture` must be registered on the session command")
}

// TestReportThenWrite_WritesNothingOnAHardFinding is the failure policy.
//
// A hard finding means the bundle would replay differently than it recorded.
// Writing it anyway leaves a directory that looks exactly like a clean capture,
// and the divergence surfaces later inside a replay pointing at the code under
// test. Every finding is reported first: five findings must cost one command
// invocation, not five.
func TestReportThenWrite_WritesNothingOnAHardFinding(t *testing.T) {
	dir := t.TempDir()
	var out bytes.Buffer
	findings := []steelthread.Finding{
		{Severity: steelthread.SeverityWarn, Code: "no-agent-reply", Message: "the session never spoke to the user"},
		{Severity: steelthread.SeverityHard, Code: "sandbox-tool-call", Message: "run_shell has no route back into a replay"},
		{Severity: steelthread.SeverityHard, Code: "unseeded-allow", Message: "widget_catalog:wc1#list is unseeded"},
	}

	err := reportThenWrite(&out, dir, steelthread.Result{}, findings, false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "2 blocking finding(s)")
	assert.Contains(t, err.Error(), "nothing written")

	entries, readErr := os.ReadDir(dir)
	require.NoError(t, readErr)
	assert.Empty(t, entries, "a refused capture must leave --output untouched")

	// Every finding is reported, warnings included — a reader deciding what to
	// fix needs the whole picture, not the first blocker.
	for _, want := range []string{"sandbox-tool-call", "unseeded-allow", "no-agent-reply"} {
		assert.Contains(t, out.String(), want)
	}
}

// TestResolveRedactions covers the two flags together, and the ordering
// between them.
//
// The file comes FIRST so a shared, reviewed list of an agent's recurring
// identifiers is applied before the one-off names typed on the command line.
// That matters when two rules' originals overlap, since the rules run in
// sequence and the earlier one wins the shared text.
func TestResolveRedactions(t *testing.T) {
	t.Run("the file's rules precede the flag's", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "redactions.txt")
		require.NoError(t, os.WriteFile(p, []byte("# customers\nacme-corp=COMPANY-A\n"), 0o644))

		got, err := resolveRedactions(captureFlags{redactFrom: p, redact: []string{"beta-labs=COMPANY-B"}})
		require.NoError(t, err)
		require.Len(t, got, 2)
		assert.Equal(t, "COMPANY-A", got[0].New)
		assert.Equal(t, "COMPANY-B", got[1].New)
	})

	t.Run("a bare original is resolved to a same-length stand-in, not refused", func(t *testing.T) {
		got, err := resolveRedactions(captureFlags{redact: []string{"no-separator-here"}})
		require.NoError(t, err)
		require.Len(t, got, 1)
		assert.Len(t, got[0].New, len("no-separator-here"),
			"a stand-in of a different byte length desyncs every recorded value derived from a byte count")
		assert.NotEmpty(t, got[0].New, "resolveRedactions must fill the stand-in in, not leave it to Capture alone")
	})

	t.Run("a malformed flag is refused before any cluster is touched", func(t *testing.T) {
		_, err := resolveRedactions(captureFlags{redact: []string{"trailing-separator="}})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "empty replacement")
	})

	// The refusal that matters most, and the reason it lives here rather than
	// only in the package: the operator learns the required length before the
	// port-forward, not after a replay that diverged three walls away.
	t.Run("a length-changing replacement is refused, naming the byte count", func(t *testing.T) {
		_, err := resolveRedactions(captureFlags{redact: []string{"acme-corporation=COMPANY-A"}})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "exactly 16 byte(s)")
	})

	t.Run("an unreadable file is an error, never an empty rule set", func(t *testing.T) {
		_, err := resolveRedactions(captureFlags{redactFrom: filepath.Join(t.TempDir(), "absent.txt")})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "redact-from")
	})
}

// TestResolveSkillElisions pins that --elide-skill is parsed BEFORE the cluster
// is touched, for the reason --redact is: a mistyped rule should cost a re-run
// of the flag, not a port-forward, a SpiceDB dial and a full records read first.
//
// The rules themselves are ParseSkillElision's subject, so this covers only the
// flag's own contract: repeatable, order-preserving, resolved (so a rule written
// without a stand-in gets one before the cluster is touched, not after), and
// fail-before-connect.
func TestResolveSkillElisions(t *testing.T) {
	t.Run("repeatable, in the order given", func(t *testing.T) {
		got, err := resolveSkillElisions(captureFlags{elideSkill: []string{
			"github.com/someorg/somerepo=github.com/exampleorg/first",
			"github.com/otherorg/otherrepo=github.com/exampleorg/seconds",
		}})
		require.NoError(t, err)
		require.Len(t, got, 2)
		assert.Equal(t, "github.com/exampleorg/first", got[0].New)
		assert.Equal(t, "github.com/exampleorg/seconds", got[1].New)
	})

	t.Run("a rule with no stand-in is RESOLVED here, before the cluster is touched", func(t *testing.T) {
		// Resolution is what can fail on a crowded rule set, so it belongs in
		// front of the port-forward with the parse — not inside Capture, after
		// a SpiceDB dial and a full records read.
		got, err := resolveSkillElisions(captureFlags{elideSkill: []string{
			"github.com/someorg/somerepo",
		}})
		require.NoError(t, err)
		require.Len(t, got, 1)
		assert.NotEmpty(t, got[0].New, "an unresolved rule would rewrite the authority to the empty string")
		assert.Len(t, got[0].New, len(got[0].Old), "a generated stand-in is the same byte length")
	})

	t.Run("a length-changing rule is refused before any cluster is touched", func(t *testing.T) {
		_, err := resolveSkillElisions(captureFlags{elideSkill: []string{
			"github.com/someorg/somerepo=github.com/exorg/repo",
		}})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "Give a replacement of exactly 27 byte(s)")
	})

	t.Run("no rules is no rules, not an empty-authority rule", func(t *testing.T) {
		got, err := resolveSkillElisions(captureFlags{})
		require.NoError(t, err)
		assert.Empty(t, got)
	})

	t.Run("a malformed rule is refused before any cluster is touched", func(t *testing.T) {
		_, err := resolveSkillElisions(captureFlags{elideSkill: []string{"https://github.com/o/r=github.com/e/r"}})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "non-canonical")
	})
}

// TestPrintRedactions pins that a rule which matched NOTHING is reported
// loudly. It is the case an operator most needs to hear about and least likely
// to notice: they named a customer, believed it removed, and the bundle still
// carries it.
func TestPrintRedactions(t *testing.T) {
	res := writableResult("demo-lists-widgets")
	res.Bundle.Capture = &bt.Capture{Redactions: []bt.Redaction{
		{Replacement: "COMPANY-A", Count: 7},
		{Replacement: "COMPANY-Z", Count: 0},
	}}

	var out bytes.Buffer
	printRedactions(&out, res)
	assert.Contains(t, out.String(), "COMPANY-A")
	assert.Contains(t, out.String(), "7 occurrence")
	assert.Contains(t, out.String(), "MATCHED NOTHING")
}

// writableResult is the smallest Result WriteResult accepts.
//
// Emitted is the only thing it writes, and a bundle.json entry is what says the
// Result came from Capture at all — a hand-built one without it is refused
// rather than written empty, which is the whole point of that precondition. The
// cases below care about the report and the on-disk replacement semantics, not
// about a bundle's contents, so the emitted bytes are a stub.
func writableResult(name string) steelthread.Result {
	res := steelthread.Result{
		Golden: []byte("\n"),
		Emitted: []steelthread.EmittedFile{
			{Name: "bundle.json", Bytes: []byte(`{"name":"` + name + `"}` + "\n")},
			{Name: "trace.golden", Bytes: []byte("\n")},
		},
	}
	res.Bundle.Name = name
	return res
}

// TestReportThenWrite_WritesWhenOnlyWarningsRemain pins the other half: a
// warning qualifies a capture without stopping it. A session whose evidence is
// its authorization trace and which never spoke to a person is still worth
// emitting.
func TestReportThenWrite_WritesWhenOnlyWarningsRemain(t *testing.T) {
	dir := t.TempDir()
	var out bytes.Buffer
	res := writableResult("demo-lists-widgets")

	err := reportThenWrite(&out, dir, res,
		[]steelthread.Finding{{Severity: steelthread.SeverityWarn, Code: "no-agent-reply", Message: "never spoke"}}, false)
	require.NoError(t, err)

	assert.FileExists(t, dir+"/bundle.json")
	assert.Contains(t, out.String(), "no-agent-reply")
	assert.Contains(t, out.String(), "wrote bundle")
}

// TestMetaToolNames_AnswersFromTheCapabilityAssembly is R3.
//
// SelfCheckInput.MetaTools is fail-closed: a name missing from it turns every
// call to that tool into a hard sandbox-tool-call finding, so a capture of a
// session that used a granted meta tool would never emit. The names therefore
// have to come from capability.Assemble — the same seam the runner drives —
// rather than from a list transcribed here, which would drift the first time a
// capability gained a tool. The meta REGISTRY alone answers with two names, and
// a capture built on those two refuses almost every real session.
//
// The cases build up the three things the answer depends on, so a failure says
// WHICH input stopped working rather than only that the count changed.
func TestMetaToolNames_AnswersFromTheCapabilityAssembly(t *testing.T) {
	cases := []struct {
		name     string
		bind     bool
		grants   map[string]apiextensionsv1.JSON
		planGate string // resolved mode; "" leaves status.effectiveSettings unset
		want     []string
		wantNot  []string
	}{
		{
			// The always-on terminal tools every session has regardless of
			// grants. A capture that could not name these could not capture any
			// session at all.
			name:    "a kubectl-driven session gets the always-on tools and no channel tools",
			want:    []string{"agent_work_complete", "new_operation"},
			wantNot: []string{"respond_to_user", "artifact_prepare"},
		},
		{
			// Channel-attached, but this process cannot reach the cluster to
			// resolve the Channel. The channel-sourced capabilities read
			// RunnerEnv.ResolveErr to know not to dereference what was never
			// resolved; without it two of them nil-panic outright.
			name: "a channel-attached session whose Channel cannot be resolved still assembles",
			bind: true,
			want: []string{"respond_to_user", "await_user_message", "update_status"},
		},
		{
			// The grants the class actually made. artifact_* additionally pins
			// the asset-renderer imports this file carries: with an empty
			// renderer registry the artifacts capability skips, and every
			// artifact_* call in a captured transcript would then read as an
			// unreplayable sandbox call. query_knowledge and search_memory pin
			// the availability booleans, which answer for the CLASS rather than
			// for whatever this laptop happens to have wired.
			name: "granted capabilities contribute their tools",
			bind: true,
			grants: map[string]apiextensionsv1.JSON{
				"artifacts": {Raw: []byte(`{}`)},
				"memory":    {Raw: []byte(`{}`)},
				"knowledge": {Raw: []byte(`{}`)},
			},
			want: []string{"artifact_prepare", "artifact_offer_view", "query_memory", "search_memory", "query_knowledge"},
		},
		{
			// The plan gate, and the case that was silently wrong. select_phase
			// and complete_phase are offered only when the gate RUNS, and this
			// function did not set RunnerEnv.PlanGateActive at all — so they
			// were missing from every capture, and because MetaTools is
			// fail-closed, every select_phase call in a plan-gated transcript
			// was refused as an unreplayable sandbox call. Plan-gated sessions
			// are the ones this feature most exists to capture, so the effect
			// was to reject its own flagship scenarios.
			name:     "a plan-gated session is offered the phase tools",
			planGate: "enforcing",
			want:     []string{"update_plan", "select_phase", "complete_phase"},
		},
		{
			// The negative control, and what stops the fix being "always true".
			// With the gate off there is no frozen plan to index into, so
			// select_phase could do nothing but fail — and offering an
			// always-failing tool spends the model's attention and invites
			// retries. update_plan is unconditional and must survive.
			name:     "a session whose gate is disabled is not offered them",
			planGate: "disabled",
			want:     []string{"update_plan"},
			wantNot:  []string{"select_phase", "complete_phase"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			class := &spiceboxv1alpha1.AgentClass{
				ObjectMeta: metav1.ObjectMeta{Name: "demo-agent", Namespace: "default"},
				Spec:       spiceboxv1alpha1.AgentClassSpec{Capabilities: tc.grants},
			}
			sess := &spiceboxv1alpha1.AgentSession{
				ObjectMeta: metav1.ObjectMeta{Name: "demo-session", Namespace: "default"},
			}
			if tc.bind {
				sess.Spec.InputChannel = &spiceboxv1alpha1.ChannelBinding{
					Name: "demo-chat", Kind: "slack", Capabilities: []string{"text", "markdown"},
				}
			}
			if tc.planGate != "" {
				// On status, not on the class spec: the mode the gate reads is
				// the tier-clamped one, and a class spec value is unclamped.
				sess.Status.EffectiveSettings = &spiceboxv1alpha1.EffectiveSettings{
					Authz: spiceboxv1alpha1.EffectiveAuthz{
						PlanGate: &spiceboxv1alpha1.PlanGateConfig{Mode: tc.planGate},
					},
				}
			}

			got := liveMetaTools(t, nil, class, sess, io.Discard)

			for _, want := range tc.want {
				assert.Contains(t, got, want)
			}
			for _, notWant := range tc.wantNot {
				assert.NotContains(t, got, notWant)
			}
		})
	}
}

// liveMetaTools is the LIVE half of the capture's tool prediction, driven
// exactly as `oap session capture` drives it: resolve the bound Channel off the
// cluster, then assemble against what came back.
//
// Both halves in one helper rather than a call to each, because the resolve
// failure the second case pins is a fact about the PAIR — the assembly is short
// BECAUSE the resolve failed, and a test that skipped the resolve would be
// asserting about an env it hand-built.
func liveMetaTools(
	t *testing.T,
	cli client.Client,
	class *spiceboxv1alpha1.AgentClass,
	sess *spiceboxv1alpha1.AgentSession,
	warn io.Writer,
) []string {
	t.Helper()
	ctx := context.Background()
	return assembleMetaTools(ctx, class, sess, sess.Spec.InputChannel,
		resolveLiveChannel(ctx, cli, sess, warn), nil, warn)
}

// fakeChannelClient serves exactly the objects resolve.ForSession reads: the
// Channel the binding names and the Secret it references.
func fakeChannelClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

// boundSession is a channel-attached session and the class behind it, the shape
// every case below starts from.
func boundSession() (*spiceboxv1alpha1.AgentClass, *spiceboxv1alpha1.AgentSession) {
	class := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-agent", Namespace: "default"},
	}
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-session", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			Class: "demo-agent",
			InputChannel: &spiceboxv1alpha1.ChannelBinding{
				Name: "demo-chat", Kind: "fake", Capabilities: []string{"text"},
			},
		},
	}
	return class, sess
}

// TestMetaToolNames_ResolvesTheBoundChannel exercises the branch that exists
// BECAUSE it once panicked.
//
// Two capabilities dereference RunnerEnv.ResolvedChannel with no nil check and
// read ResolveErr to know not to (channelhistorygate.Offer, mention_lookup's
// Offer). A test that only ever passed a nil client took the early return and
// never reached resolve.ForSession at all — so the fix was uncovered by the
// very test written for it.
//
// The failure case additionally pins that the shortfall is not silent: because
// MetaTools is fail-closed, a resolve failure surfaces to the operator as a
// capture refused for a sandbox-tool-call naming read_channel_history, and the
// warning is the only thing that says otherwise.
func TestMetaToolNames_ResolvesTheBoundChannel(t *testing.T) {
	t.Run("a resolvable Channel assembles without panicking", func(t *testing.T) {
		class, sess := boundSession()
		ch := &spiceboxv1alpha1.Channel{
			ObjectMeta: metav1.ObjectMeta{Name: "demo-chat", Namespace: "default"},
			Spec: spiceboxv1alpha1.ChannelSpec{
				Kind:           "fake",
				AgentClass:     "demo-agent",
				CredentialsRef: spiceboxv1alpha1.ChannelCredentialsRef{SecretName: "demo-chat-creds"},
			},
		}
		sec := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "demo-chat-creds", Namespace: "default"},
			Data:       map[string][]byte{"placeholder": []byte("unused")},
		}
		var warn bytes.Buffer

		got := liveMetaTools(t, fakeChannelClient(t, ch, sec), class, sess, &warn)

		assert.Contains(t, got, "respond_to_user")
		assert.Contains(t, got, "agent_work_complete")
		assert.NotContains(t, warn.String(), "could not resolve",
			"a Channel that resolved must not be reported as one that did not")
	})

	t.Run("an unresolvable Channel is warned about, not swallowed", func(t *testing.T) {
		class, sess := boundSession()
		var warn bytes.Buffer

		// A client with no Channel object at all: the Get inside
		// resolve.ForSession fails, which is the shape of a session whose
		// Channel was deleted after the run.
		got := liveMetaTools(t, fakeChannelClient(t), class, sess, &warn)

		assert.Contains(t, got, "agent_work_complete", "the always-on tools survive a resolve failure")
		assert.Contains(t, warn.String(), "demo-chat")
		assert.Contains(t, warn.String(), "sandbox tool call",
			"the warning has to name the MISLEADING finding the operator will otherwise see")

		// The second sink, in the one case that reaches it: capability.Assemble
		// hands every granted-but-unavailable capability to logSkip, and
		// mention_lookup turns exactly this ResolveErr into a SkipReason. A
		// discard logger there would throw away the per-capability half of the
		// explanation and leave only the line above.
		assert.Contains(t, warn.String(), "mention_lookup",
			"a capability that declined must reach the operator; it is why a name is missing "+
				"from a list the self-check then refuses a transcript against")
	})
}

// TestReportThenWrite_OverwriteReplacesAnExistingBundle drives the flag through
// the command's own path, so --overwrite is proven to reach WriteResult's
// replace semantics rather than only its emptiness check.
func TestReportThenWrite_OverwriteReplacesAnExistingBundle(t *testing.T) {
	dir := t.TempDir()
	// A previous capture: its bundle.json is what marks the directory as one
	// this command may replace, rather than an arbitrary path a typo produced.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "bundle.json"), []byte(`{"name":"older"}`), 0o644))
	stale := filepath.Join(dir, "02-mcpserver.yaml")
	require.NoError(t, os.WriteFile(stale, []byte("kind: MCPServer\n"), 0o644))

	res := writableResult("demo-lists-widgets")

	var out bytes.Buffer
	require.NoError(t, reportThenWrite(&out, dir, res, nil, true))

	assert.NoFileExists(t, stale, "--overwrite says Replace; a survivor is a merge")
	assert.FileExists(t, filepath.Join(dir, "bundle.json"))

	// And without the flag the same directory is refused rather than merged.
	require.NoError(t, os.WriteFile(stale, []byte("kind: MCPServer\n"), 0o644))
	err := reportThenWrite(&out, dir, res, nil, false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not empty")
}

// secretWith builds a Secret carrying one data key.
func secretWith(ns, name, key, value string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Data:       map[string][]byte{key: []byte(value)},
	}
}

// liveSecretNames returns the gathered entries' names, which is all any
// assertion here may look at. The VALUES are live credential material and a
// test that printed one on failure would put it in whatever captured the
// terminal — the same rule Finding.Message holds.
func liveSecretNames(got []steelthread.LiveSecret) []string {
	out := make([]string, 0, len(got))
	for _, s := range got {
		out = append(out, s.Name)
	}
	return out
}

// TestGatherLiveSecrets_ReadsTheResolvedStatusNotJustTheSpec is the gathering
// half of the leak scan, and it exists because the spec half was not enough.
//
// A live capture refused with secret-check-skipped over an AgentClass whose
// spec.model was absent entirely: its model and its credential are chosen by
// the 4-tier settings resolver, stamped on AgentSession.status.effectiveSettings,
// and materialized into a per-session Secret. Gathering from the spec alone
// found nothing to scan for, so the fail-closed gate refused — correctly, over
// a real gathering gap.
func TestGatherLiveSecrets_ReadsTheResolvedStatusNotJustTheSpec(t *testing.T) {
	const ns = "default"

	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-session", Namespace: ns},
		Status: spiceboxv1alpha1.AgentSessionStatus{
			EffectiveSettings: &spiceboxv1alpha1.EffectiveSettings{
				Model: spiceboxv1alpha1.ModelConfig{
					APIKey: spiceboxv1alpha1.SecretKeyRef{Name: "demo-session-memory-token", Key: "llm-api-key"},
				},
				ModelTokenSource: &spiceboxv1alpha1.NamespacedSecretKeyRef{
					Name: "model-default-token", Namespace: "agentprimitives-system", Key: "token",
				},
			},
			ResolvedSidecarToolboxes: []spiceboxv1alpha1.ResolvedSidecarToolbox{{Name: "det", Ref: "det"}},
		},
	}
	// A class with NO model at all — the shape that produced the refusal.
	class := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-agent", Namespace: ns},
	}

	cli := fakeChannelClient(t,
		secretWith(ns, "demo-session-memory-token", "llm-api-key", "fixture-only-model-key"),
		secretWith(ns, "demo-session-passthrough-creds", "github-token", "fixture-only-passthrough"),
		secretWith("agentprimitives-system", "model-default-token", "token", "fixture-only-central-token"),
		secretWith(ns, cosidecar.CredentialSecretName("demo-session", "det"), "upstream", "fixture-only-sidecar"),
	)
	b := &kube.Bundle{Controller: cli, Namespace: ns}

	var warn bytes.Buffer
	got := gatherLiveSecrets(context.Background(), b, sess,
		steelthread.FixtureInput{Class: class}, &warn)

	names := liveSecretNames(got)
	assert.Contains(t, names, "demo-session-memory-token/llm-api-key",
		"the RESOLVED model credential is the one the run used; the class spec named none")
	assert.Contains(t, names, "demo-session-passthrough-creds/github-token",
		"the per-session passthrough Secret is where a toolBundle's credentialRemap resolves")
	assert.Contains(t, names, "model-default-token/token",
		"the central token source lives in ANOTHER namespace and must still be read")
	assert.Contains(t, names, cosidecar.CredentialSecretName("demo-session", "det")+"/upstream",
		"a sidecar toolbox's materialized upstream credential is live material a transcript can carry")

	// demo-session-secret-outputs was derived from the same suffix list and does
	// not exist: a session that produced no secret outputs never gets one. That
	// is not a failure to read, and warning about it on every capture would
	// train an operator to ignore the warnings that matter.
	assert.NotContains(t, warn.String(), "secret-outputs",
		"a derived per-session Secret that was never created holds nothing that could leak")
}

// TestGatherLiveSecrets_RecordsAnEmptySecretAsRead pins the distinction the
// leak scan's gate turns on.
//
// A browser-started session's Channel credentials Secret is created EMPTY —
// the browser surface needs no credential — and the fixture still emits a
// placeholder standing in for it. Without an entry recording that the Secret WAS
// read, the gate cannot tell that from nobody having looked, and every
// browser-started capture is refused over a Secret that is empty by design.
// That is not hypothetical: it is the finding a live capture produced.
func TestGatherLiveSecrets_RecordsAnEmptySecretAsRead(t *testing.T) {
	const ns = "default"
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-session", Namespace: ns},
	}
	// kind: browser, because that is the session this test describes — and
	// because the gather now asks the Channel's own kind which of its Secret's
	// data keys are public identifiers. A Channel with no kind cannot be asked,
	// which is a warning of its own and would drown the fact under test.
	ch := &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-chat", Namespace: ns},
		Spec: spiceboxv1alpha1.ChannelSpec{
			Kind:           "browser",
			CredentialsRef: spiceboxv1alpha1.ChannelCredentialsRef{SecretName: "demo-session-chan-creds"},
		},
	}
	empty := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "demo-session-chan-creds", Namespace: ns}}

	var warn bytes.Buffer
	got := gatherLiveSecrets(context.Background(),
		&kube.Bundle{Controller: fakeChannelClient(t, empty), Namespace: ns}, sess,
		steelthread.FixtureInput{Channels: []*spiceboxv1alpha1.Channel{ch}}, &warn)

	require.Len(t, got, 1)
	assert.Equal(t, "demo-session-chan-creds", got[0].Name,
		"an EmptyRead entry names the SECRET, because there is no key to qualify it with")
	assert.True(t, got[0].EmptyRead)
	assert.Empty(t, got[0].Value,
		"the entry must stay unscannable: \"\" is a substring of every file")
	assert.Empty(t, warn.String(), "an empty Secret that was read is not a failure to read")
}

// TestGatherLiveSecrets_WarnsWhenAReferencedSecretCannotBeRead is the other
// direction: something POINTED AT this Secret, so its absence is a value the
// leak scan cannot look for, and silence there is how a credential reaches a
// repo.
func TestGatherLiveSecrets_WarnsWhenAReferencedSecretCannotBeRead(t *testing.T) {
	const ns = "default"
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-session", Namespace: ns},
	}
	class := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-agent", Namespace: ns},
		Spec: spiceboxv1alpha1.AgentClassSpec{
			Model: &spiceboxv1alpha1.ModelConfig{
				APIKey: spiceboxv1alpha1.SecretKeyRef{Name: "vanished-model-key", Key: "api-key"},
			},
		},
	}

	var warn bytes.Buffer
	got := gatherLiveSecrets(context.Background(), &kube.Bundle{Controller: fakeChannelClient(t), Namespace: ns},
		sess, steelthread.FixtureInput{Class: class}, &warn)

	assert.Empty(t, got)
	assert.Contains(t, warn.String(), "vanished-model-key",
		"a referenced Secret that cannot be read must be named, not silently skipped")
}

// grantsOwnedBy builds an AgentSessionGrants shaped the way the AgentClass
// reconciler writes one: named after the class, and carrying the class as its
// CONTROLLER owner reference.
func grantsOwnedBy(className string, slots ...spiceboxv1alpha1.GrantPair) *spiceboxv1alpha1.AgentSessionGrants {
	return &spiceboxv1alpha1.AgentSessionGrants{
		ObjectMeta: metav1.ObjectMeta{
			Name:      className + "-grants",
			Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: spiceboxv1alpha1.SchemeGroupVersion.String(),
				Kind:       "AgentClass",
				Name:       className,
				UID:        types.UID("uid-" + className),
				Controller: ptr.To(true),
			}},
		},
		Spec: spiceboxv1alpha1.AgentSessionGrantsSpec{Slots: slots},
	}
}

// TestGatherSessionGrants covers the lookup DeriveSeed's slot exemption depends
// on, in all three states it can be in.
//
// The owner reference is the handle rather than the "<class>-grants" name,
// because a wrong answer here is SILENT in the permissive direction: returning
// nil for a class that does declare slots makes the capture hard-fail on a
// collected slot binding, which is the exact regression the exemption exists to
// remove. Picking up a SIBLING class's CR is the opposite failure and worse —
// it would exempt pairs this class never declared.
func TestGatherSessionGrants(t *testing.T) {
	fetch := spiceboxv1alpha1.GrantPair{ResourceType: "extrepo_repo", Permission: "fetch"}

	cases := []struct {
		name      string
		objs      []client.Object
		wantSlots []spiceboxv1alpha1.GrantPair
		wantWarn  string
	}{
		{
			name: "the class's own CR is found by owner reference, not by name",
			objs: []client.Object{grantsOwnedBy("demo-agent", fetch)},
			// Named for the OTHER class on purpose: matching on the name alone
			// would return this one too.
			wantSlots: []spiceboxv1alpha1.GrantPair{fetch},
		},
		{
			name:      "a sibling class's CR in the same namespace is not picked up",
			objs:      []client.Object{grantsOwnedBy("other-agent", fetch)},
			wantSlots: nil,
			wantWarn:  "demo-agent",
		},
		{
			name:      "no CR at all: nil, and a warning naming the class",
			objs:      nil,
			wantSlots: nil,
			wantWarn:  "demo-agent",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			class := &spiceboxv1alpha1.AgentClass{
				ObjectMeta: metav1.ObjectMeta{Name: "demo-agent", Namespace: "default"},
			}

			var warn bytes.Buffer
			got, err := gatherSessionGrants(context.Background(),
				&kube.Bundle{Controller: fakeChannelClient(t, tc.objs...), Namespace: "default"}, class, &warn)
			require.NoError(t, err)

			if tc.wantSlots == nil {
				assert.Nil(t, got, "nothing owned by this class means no declaration to consult")
			} else {
				require.NotNil(t, got)
				assert.Equal(t, tc.wantSlots, got.Spec.Slots)
			}
			if tc.wantWarn == "" {
				assert.Empty(t, warn.String(), "a CR that was found is not worth a warning")
				return
			}
			assert.Contains(t, warn.String(), tc.wantWarn,
				"a class with no grant declaration must be named: it is why an unseeded allow will refuse")
		})
	}
}
