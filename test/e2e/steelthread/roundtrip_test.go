//go:build e2e

package steelthread_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/pkg/agent/modality/files"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta/capability"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	bt "github.com/authzed/openagentprimitives/pkg/bronzethread"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	chregistry "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/resolve"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/authzdecision"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/plangateaudit"
	"github.com/authzed/openagentprimitives/pkg/platform/artifacts"
	blobstore "github.com/authzed/openagentprimitives/pkg/platform/artifactstore/blob"
	credkindregistry "github.com/authzed/openagentprimitives/pkg/platform/identity/credkind/registry"
	"github.com/authzed/openagentprimitives/pkg/steelthread"
	"github.com/authzed/openagentprimitives/test/e2e"
	"github.com/authzed/openagentprimitives/test/e2e/threadrun"
)

// sourceBundle is the authored scenario this round trip captures.
//
// A BRONZE bundle, deliberately: its records were produced by the real runner
// over the real hook pipeline, so they are a faithful stand-in for a live
// session — but the transcript is scripted, so the test needs no cluster, no
// credentials and no model, and it runs in CI.
//
// This one out of the 37, because a bronze run is only emittable as a bundle
// when two things hold at once, and most scenarios break one of them:
//
//   - No tool call came back an ERROR. Fold turns every recorded MCP result
//     into a canned toolOutputs entry, and a denied call's body is the
//     plain-text "permission denied: <subject> does not have …" — not JSON, and
//     naming a subject the replay does not have. Most authorization scenarios
//     exist precisely to record one, which rules them out as sources.
//   - Its approvals are PLAN-GATE ones. plan_phase and plan_amendment are
//     recoverable from the plan-gate log, so the capture derives autoApprove;
//     a tool_approval's interaction category is durably recorded nowhere, so
//     the capture cannot answer it and the replay would sit on the prompt
//     until the class budget expired.
//
// A declared tool the transcript never called does NOT disqualify a scenario,
// and a reader picking a second source should not treat it as though it does:
// the capture synthesizes the placeholder entry class admission needs and says
// so with a placeholder-tool-outputs WARNING. This bundle happens to call both
// of its server's tools anyway — status once and push twice — so nothing here
// rests on that leniency either way.
//
// What it exercises is the point of picking it over something simpler: two MCP
// tools with real arguments, a plan-gate card a human cleared, a permission
// ALLOWED only because of that approval (so the seed derivation and its
// subtraction rules both run), an agent reply, and a frozen golden trace.
const sourceBundle = "../bronzethread/testdata/plangate-external-covered-by-approved-slot"

// TestRoundTrip_CapturedBundleReplays is the proof the feature works.
//
// Every other test in this feature proves ONE stage is self-consistent — the
// fold folds, the seed subtracts, the self-check fires. None of them can catch
// the capture and the replay disagreeing about the same fact, which is the
// failure this whole design is exposed to: the capture writes what it believes
// the driver wants, the driver reads something slightly different, and both
// halves pass their own tests.
func TestRoundTrip_CapturedBundleReplays(t *testing.T) {
	// 1. Run the authored bundle and keep the harness it ran in.
	source := threadrun.Load(t, sourceBundle)
	source.AgentDir = rebaseAgentDir(sourceBundle, source.AgentDir)
	h := threadrun.Run(t, sourceBundle, source)

	// 2. Read back exactly what the run recorded.
	recs := readRecords(t, h)
	require.NotEmpty(t, recs.Turns, "the source run recorded no transcript; the capture would be vacuous")

	// 3. Capture, with every gate's input gathered the way `oap session
	//    capture` gathers it — not stubbed past.
	res, findings, err := steelthread.Capture(recs, captureInput(t, h, capturedName))
	require.NoError(t, err, "assembling the capture")

	report, hard := steelthread.FormatFindings(findings)
	require.False(t, steelthread.HasHardFinding(findings),
		"a capture of a known-good run must have no blocking findings; got %d:\n%s", hard, report)
	if report != "" {
		t.Logf("capture findings (all advisory):\n%s", report)
	}

	// 4. Write it where the driver can discover it.
	//
	// Under this package's own testdata/, not a temp dir: bt.Bundle.AgentDir is
	// "testdata/<name>" — the conventional home every scenario uses — and the
	// harness resolves it relative to the suite's working directory. A capture
	// written anywhere else would boot no fixture at all. Removed on cleanup so
	// TestSteelthread's discovery does not inherit it.
	dir := filepath.Join("testdata", capturedName)
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Errorf("removing the captured bundle at %s: %v", dir, err)
		}
	})
	require.NoError(t, steelthread.WriteResult(dir, res, steelthread.Overwrite()))

	// 5. Replay the CAPTURE. A divergence here is the whole point of the test:
	//    it means the capture emitted something the driver reads differently
	//    than the capture meant it.
	captured := threadrun.Load(t, dir)
	require.NotNil(t, captured.Capture, "the re-emitted bundle must declare its provenance")
	assert.Equal(t, recs.Session, captured.Capture.Session,
		"the re-emitted bundle must name the session it came from")
	replay := threadrun.Run(t, dir, captured)

	// 6. The golden the capture FROZE must be the trace its own replay
	//    produces.
	//
	// threadrun.Run already compares them — but through assert.Equal inside
	// checkGoldenTrace, which BRONZE_UPDATE_GOLDEN silently turns into a
	// rewrite. Restated here so the claim holds under the one environment
	// variable that suspends it. If these differ, every captured bundle fails
	// its golden on the first run, somebody regenerates it unread, and the
	// golden stops asserting anything.
	assert.Equal(t, string(res.Golden), string(traceOf(t, replay)),
		"the golden frozen from the SOURCE run is not the trace the REPLAY produced, so a "+
			"freshly captured bundle fails its own golden on the very first run")
}

// capturedName is the bundle name the capture is emitted under. Distinct enough
// that testdata/.gitignore can exclude it without shadowing a real capture.
const capturedName = "roundtrip-scratch"

// rebaseAgentDir re-points a bronze bundle's agentDir at this package.
//
// bt.Bundle.AgentDir is a path relative to the SUITE that owns the bundle, and
// the bronze scenarios are split between two spellings: "../testdata/<agent>"
// for the shared fixtures (which resolves identically from either suite,
// because both packages sit one level under test/e2e) and "testdata/<name>"
// for a scenario whose fixture lives in its own bundle directory. Only the
// second needs re-basing, and re-basing it is pure path arithmetic — the same
// files, addressed from a different working directory. Nothing about what the
// bundle asserts changes.
func rebaseAgentDir(bundleDir, agentDir string) string {
	if filepath.IsAbs(agentDir) || strings.HasPrefix(agentDir, "../") {
		return agentDir
	}
	// bundleDir is "<bronze package>/testdata/<bundle>", so the bronze package
	// root — the directory a suite-relative agentDir is measured from — is its
	// grandparent.
	return filepath.Join(filepath.Dir(filepath.Dir(bundleDir)), agentDir)
}

// readRecords reads the session the harness just ran, through the same reader
// `oap session capture` uses. A second query list here would be free to go
// stale, and the symptom would be a capture silently missing a record class.
func readRecords(t *testing.T, h *e2e.Harness) steelthread.Records {
	t.Helper()
	ns, name := h.SessionRef()
	recs, err := steelthread.RecordsFromMemory(
		captureCtx(), h.Memory(), memory.Scope{Kind: "session", ID: ns + "/" + name})
	require.NoError(t, err, "reading the records the run produced")
	return recs
}

// captureCtx carries the memory capability every read on this data plane needs.
// The name is what lands in the audit trail, so it says who is reading.
func captureCtx() context.Context {
	return memory.WithSystemApproval(context.Background(), "steelthread-roundtrip")
}

// captureInput assembles the CaptureInput from the live harness, mirroring
// `oap session capture` field for field.
//
// Deliberately NOT a set of convenient zero values. Three of these fields are
// fail-closed by design — MetaTools, LiveSecrets and (via the class)
// GuardConfigured — and an empty one is not "nothing to say", it is a hard
// finding. Supplying what is honestly true is the only thing that makes a green
// round trip mean anything.
func captureInput(t *testing.T, h *e2e.Harness, name string) steelthread.CaptureInput {
	t.Helper()
	sess := sessionOf(t, h)
	fixture := gatherManifests(t, h, sess)
	return steelthread.CaptureInput{
		Name:        name,
		Description: "Round-trip fixture: captured from a scripted bronze run, replayed by the same driver.",
		Session:     sess.Namespace + "/" + sess.Name,
		Cluster:     "envtest",
		OapVersion:  "roundtrip-test",
		// Fixed, not time.Now(): Capture takes the clock as an input precisely
		// so its output is a pure function of its records, and a test that fed
		// it a moving value would be unable to tell a re-capture's diff from
		// noise.
		Now:     time.Date(2026, 8, 27, 0, 0, 0, 0, time.UTC),
		Fixture: fixture,
		// steelthread.CaptureSource: this mirrors `oap session capture`
		// (cmd/oap/internal/sessioncmd/capture.go), which references the same
		// var. See pkg/authz/spicedb/relsource and pkg/authz/spicedb/writer.go.
		Expand:    steelthread.NewSpiceDBExpander(h.SpiceDB.Writer(steelthread.CaptureSource)),
		MetaTools: metaToolNames(t, h, fixture.Class, sess),
		// The other half of the same question, against the Channel the rewrite
		// will emit. Fail-closed like MetaTools: leaving it empty is a hard
		// finding, not "the fixture offers nothing".
		FixtureTools: fixtureToolPrediction(t, h, fixture, sess),
		LiveSecrets:  gatherLiveSecrets(t, h, fixture),
	}
}

// sessionOf loads the AgentSession the run produced.
func sessionOf(t *testing.T, h *e2e.Harness) *spiceboxv1alpha1.AgentSession {
	t.Helper()
	ns, name := h.SessionRef()
	var sess spiceboxv1alpha1.AgentSession
	require.NoError(t, h.K8s.Get(captureCtx(), client.ObjectKey{Namespace: ns, Name: name}, &sess),
		"get AgentSession %s/%s", ns, name)
	return &sess
}

// gatherManifests reads the live CRs the fixture is rewritten from: the class,
// every MCPServer it references, its AgentIdentity, and every Channel bound to
// it. A referenced object that is missing FAILS rather than being omitted — the
// bundle would still emit, with fewer tools or no channel than the session had.
func gatherManifests(t *testing.T, h *e2e.Harness, sess *spiceboxv1alpha1.AgentSession) steelthread.FixtureInput {
	t.Helper()
	ctx := captureCtx()
	ns := sess.Namespace

	var class spiceboxv1alpha1.AgentClass
	require.NoError(t, h.K8s.Get(ctx, client.ObjectKey{Namespace: ns, Name: sess.Spec.Class}, &class),
		"get AgentClass %q (the session's class)", sess.Spec.Class)
	in := steelthread.FixtureInput{Class: &class}

	for _, ref := range class.Spec.MCPServers {
		var server spiceboxv1alpha1.MCPServer
		require.NoError(t, h.K8s.Get(ctx, client.ObjectKey{Namespace: ns, Name: ref.Ref}, &server),
			"get MCPServer %q (referenced by the class as %q)", ref.Ref, ref.Name)
		in.MCPServers = append(in.MCPServers, &server)
	}

	if class.Spec.AgentIdentity != "" {
		var identity spiceboxv1alpha1.AgentIdentity
		require.NoError(t, h.K8s.Get(ctx, client.ObjectKey{Namespace: ns, Name: class.Spec.AgentIdentity}, &identity),
			"get AgentIdentity %q", class.Spec.AgentIdentity)
		in.Identity = &identity
	}

	var channels spiceboxv1alpha1.ChannelList
	require.NoError(t, h.K8s.List(ctx, &channels, client.InNamespace(ns)), "list Channels")
	for i := range channels.Items {
		if channels.Items[i].Spec.AgentClass == class.Name {
			in.Channels = append(in.Channels, &channels.Items[i])
		}
	}

	// TriggerChannel stays empty: a bronze bundle driven by userTurns records
	// no trigger delivery, and naming a trigger Channel for one would keep that
	// Channel's real kind in the fixture for a bundle with no delivery to sign.
	return in
}

// gatherLiveSecrets reads every Secret the gathered manifests reference, for
// the sole purpose of proving their values are absent from the emitted files.
//
// RewriteFixture never sees a live Secret's value, so nothing else in the
// pipeline could do this — and an empty result does not pass the scan, it
// raises secret-check-skipped. A Secret that cannot be read FAILS here rather
// than being skipped: in a fixture this test applied itself, an unreadable
// Secret is a harness defect, not a cluster the operator cannot see into.
func gatherLiveSecrets(t *testing.T, h *e2e.Harness, in steelthread.FixtureInput) []steelthread.LiveSecret {
	t.Helper()
	ctx := captureCtx()

	var names []string
	seen := map[string]bool{}
	add := func(n string) {
		if n != "" && !seen[n] {
			seen[n] = true
			names = append(names, n)
		}
	}
	if in.Class != nil && in.Class.Spec.Model != nil {
		add(in.Class.Spec.Model.APIKey.Name)
	}
	for _, ch := range in.Channels {
		add(ch.Spec.CredentialsRef.SecretName)
	}
	if in.Identity != nil {
		for _, cred := range in.Identity.Spec.Credentials {
			// Through the credkind registry rather than a switch on cred.Type,
			// so a credential type added later is read here unchanged.
			secret, err := credkindregistry.SecretNameFor(cred)
			require.NoError(t, err, "resolve the Secret name for credential %q", cred.Name)
			add(secret)
		}
	}

	var out []steelthread.LiveSecret
	for _, secretName := range names {
		var sec corev1.Secret
		require.NoError(t, h.K8s.Get(ctx, client.ObjectKey{Namespace: in.Class.Namespace, Name: secretName}, &sec),
			"read Secret %q referenced by the captured manifests", secretName)
		for key, val := range sec.Data {
			// An empty value is not scannable — "" is a substring of every
			// file — so it is dropped rather than counted as a value looked for.
			if len(val) == 0 {
				continue
			}
			out = append(out, steelthread.LiveSecret{Name: secretName + "/" + key, Value: string(val)})
		}
	}
	require.NotEmpty(t, out, "no live secret value could be read; the leak scan would have nothing to look for")
	return out
}

// metaToolNames answers "which meta tools did this session's class offer?"
// through capability.Assemble — the same seam internal/cmd/runner, the e2e
// in-process factory and `oap session capture` all drive.
//
// Assembled rather than listed. Which meta tools a session gets is decided by
// its capabilities, so a list written here would be a snapshot that drifts the
// first time a capability gains a tool — and because MetaTools is fail-closed,
// every call to the new tool would then read as an unreplayable sandbox call.
func metaToolNames(
	t *testing.T, h *e2e.Harness, class *spiceboxv1alpha1.AgentClass, sess *spiceboxv1alpha1.AgentSession,
) []string {
	t.Helper()
	ctx := captureCtx()

	var ch *spiceboxv1alpha1.Channel
	var sec *corev1.Secret
	var kind channelkinds.Kind
	if sess.Spec.InputChannel != nil {
		var err error
		ch, sec, kind, err = resolve.ForSession(ctx, h.K8s, sess)
		if err == nil && (ch == nil || kind == nil) {
			err = fmt.Errorf("the bound Channel resolved to no channel or no kind")
		}
		// Fatal rather than warned. In production an unresolvable Channel costs
		// the capture its channel-sourced meta tools and the operator is told;
		// here it would silently shrink the fail-closed list and turn a
		// perfectly replayable call into a sandbox-tool-call finding naming the
		// wrong thing.
		require.NoError(t, err, "resolve the session's bound Channel %q", sess.Spec.InputChannel.Name)
	}

	names := assembleMetaTools(t, h, class, sess, sess.Spec.InputChannel, ch, sec, kind)
	require.NotEmpty(t, names, "capability.Assemble offered no meta tools; MetaTools is fail-closed, so "+
		"every non-MCP call in the transcript would be refused as a sandbox call")
	return names
}

// assembleMetaTools is the assembly itself, over an explicitly supplied binding
// and bound channel.
//
// Split out because the capture needs the answer TWICE: once for the live
// session (above) and once for the Channel the fixture rewrite will emit (see
// fixtureToolPrediction). Two separately-written assemblies could differ for
// reasons that have nothing to do with the rewrite, which is the one thing the
// comparison between them must not confuse.
func assembleMetaTools(
	t *testing.T,
	h *e2e.Harness,
	class *spiceboxv1alpha1.AgentClass,
	sess *spiceboxv1alpha1.AgentSession,
	binding *spiceboxv1alpha1.ChannelBinding,
	ch *spiceboxv1alpha1.Channel,
	sec *corev1.Secret,
	kind channelkinds.Kind,
) []string {
	t.Helper()
	env := capability.RunnerEnv{
		ChannelAttached: binding != nil,
		// TRUE because the question is what the CLASS was offered, not what
		// this process happens to have wired. The grants still gate everything.
		MemoryAvailable: true,
		SearchAvailable: true,
		KGAvailable:     true,
		Artifacts:       artifacts.NewService(h.Memory(), nil),
		// fetch_artifact exists whenever a reader does, and the replay driver
		// wires one for every bundle. Left nil, the prediction would claim the
		// fixture offers no fetch_artifact about a replay that certainly does.
		ArtifactReader: files.StoreReader{Store: blobstore.NewMem()},
		// Gates select_phase and complete_phase. Read from the session's
		// RESOLVED settings, the same source the runner uses — and the reason
		// the source bundle, which is plan-gated, is capturable at all.
		PlanGateActive: sess.Status.EffectiveSettings.PlanGateActive(),
	}
	if env.ChannelAttached {
		env.ResolvedChannel, env.ResolvedSecret, env.ResolvedKind = ch, sec, kind
	}

	tools := capability.Assemble(captureCtx(), capability.AssembleDeps{
		Class:   class,
		Session: sess,
		Binding: binding,
		Env:     env,
		// Every line is a granted capability that contributed no tool — exactly
		// what a reader needs when the capture is then refused for calling one.
		Logger: skipLogger(t),
	})
	names := make([]string, 0, len(tools))
	for _, tl := range tools {
		names = append(names, tl.Name())
	}
	return names
}

// fixtureToolPrediction answers "which meta tools will the EMITTED fixture
// offer?", the half that can see a tool the rewrite destroyed.
//
// Mirrors `oap session capture`'s own fixtureMetaTools, which lives under
// cmd/oap/internal and so cannot be imported here. The duplication is the same
// one metaToolNames above already carries, and it is what makes this round trip
// evidence about the capture rather than about the CLI.
//
// The external-surface half is deliberately NOT reproduced: this fixture's
// Channel is the fake kind, which reports no trigger status, so the difference
// it would compute is empty by construction. A round trip over a triggered
// provider-backed session would need it, and would be refused by the capture
// long before reaching here.
func fixtureToolPrediction(
	t *testing.T,
	h *e2e.Harness,
	fixture steelthread.FixtureInput,
	sess *spiceboxv1alpha1.AgentSession,
) []steelthread.FixtureTool {
	t.Helper()
	if sess.Spec.InputChannel == nil {
		return asFixtureTools(assembleMetaTools(t, h, fixture.Class, sess, nil, nil, nil, nil))
	}

	live, _, _, err := resolve.ForSession(captureCtx(), h.K8s, sess)
	require.NoError(t, err, "resolve the session's bound Channel")

	inCh, _, err := steelthread.ReplayChannel(fixture, sess.Spec.InputChannel.Name)
	require.NoError(t, err, "the fixture must carry the Channel the session bound to")
	outCh, outSec, err := steelthread.ReplayChannel(fixture, live.Name)
	require.NoError(t, err, "the fixture must carry the Channel the session resolved outbound to")
	outKind, ok := chregistry.Get(outCh.Spec.Kind)
	require.True(t, ok, "the fixture's bound Channel declares an unregistered kind %q", outCh.Spec.Kind)

	binding := sess.Spec.InputChannel.DeepCopy()
	binding.Kind = inCh.Spec.Kind
	return asFixtureTools(assembleMetaTools(t, h, fixture.Class, sess, binding, outCh, outSec, outKind))
}

// asFixtureTools pairs each name with the external-surface fact, which is false
// for every tool here; see fixtureToolPrediction.
func asFixtureTools(names []string) []steelthread.FixtureTool {
	out := make([]steelthread.FixtureTool, 0, len(names))
	for _, n := range names {
		out = append(out, steelthread.FixtureTool{Name: n})
	}
	return out
}

// skipLogger routes capability.Assemble's skip lines into the test log.
func skipLogger(t *testing.T) logr.Logger {
	t.Helper()
	return funcr.New(func(prefix, args string) {
		if prefix != "" {
			t.Logf("capability assembly: %s: %s", prefix, args)
			return
		}
		t.Logf("capability assembly: %s", args)
	}, funcr.Options{})
}

// traceOf renders the authorization trace of a finished run, through the SAME
// bt.TraceLines call the driver's checkGoldenTrace and steelthread.GoldenTrace
// both use. Two renderings of the same logs is how a harness like this rots.
func traceOf(t *testing.T, h *e2e.Harness) []byte {
	t.Helper()
	ctx := captureCtx()
	ns, name := h.SessionRef()
	scope := memory.Scope{Kind: "session", ID: ns + "/" + name}

	gate, err := plangateaudit.List(ctx, h.Memory(), scope)
	require.NoError(t, err, "reading the plan-gate log of the replay")

	res, err := h.Memory().Query(ctx, memory.Query{Scope: scope, Kinds: []string{authzdecision.KindName}})
	require.NoError(t, err, "reading the authz-decision log of the replay")
	decisions := make([]authzdecision.Decision, 0, len(res.Entries))
	for _, e := range res.Entries {
		var d authzdecision.Decision
		require.NoError(t, json.Unmarshal(e.Content, &d), "decoding authz_decision %s", e.ID)
		decisions = append(decisions, d)
	}
	// Joined exactly the way steelthread.GoldenTrace and checkGoldenTrace both
	// join it; a third spelling of the separator would make the comparison
	// below fail on punctuation rather than on content.
	return []byte(strings.Join(bt.TraceLines(gate, decisions), "\n") + "\n")
}
