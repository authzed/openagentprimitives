package steelthread_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	bt "github.com/authzed/openagentprimitives/pkg/bronzethread"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/authzdecision"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/triggerdelivery"
	"github.com/authzed/openagentprimitives/pkg/steelthread"
)

// fixedTime is the capture clock every determinism-sensitive case pins. A
// capture reads its timestamp from CaptureInput rather than from the wall
// clock precisely so this constant can exist.
var fixedTime = time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)

// liveModelKeyValue is the live credential the capture claims it read from the
// AgentClass's model API-key Secret. Distinctive on purpose: the leak scan is a
// literal byte match, so a value that could plausibly occur in an unrelated
// fixture would make a clean scan meaningless.
const liveModelKeyValue = "sk-fixture-only-9f8e7d6c5b4a"

// syntheticRecords is one whole session as durable records: a person asks, the
// model calls the one declared MCP tool, the call is allowed, and the agent
// answers. Everything a capture reads, and nothing a cluster has to supply.
func syntheticRecords(t *testing.T) steelthread.Records {
	t.Helper()
	return steelthread.Records{
		Session: "default/demo-session",
		Turns: []memory.Turn{
			userText(0, "list the widgets"),
			assistantCall(1, "tu_1", "demoforge_list_widgets", `{"operation_id":"op-1","_reason":"asked","args":{}}`),
			toolResult(2, "tu_1", `{"results":[]}`, false),
			assistantCall(3, "tu_2", "respond_to_user", `{"text":"no widgets found"}`),
		},
		Decisions: []authzdecision.Decision{
			{ResourceType: "widget_catalog", ResourceID: "wc1", Permission: "list", Outcome: authzdecision.OutcomeAllowed},
		},
	}
}

// captureInput is the CaptureInput every case starts from: the live manifests
// liveFixture builds, an Expander that answers with one pre-existing tuple, the
// meta tools the session was offered, and the live secret values the gather
// read (supplied for the sole purpose of proving their absence).
func captureInput(t *testing.T) steelthread.CaptureInput {
	t.Helper()
	return steelthread.CaptureInput{
		Name:       "demo-lists-widgets",
		Session:    "default/demo-session",
		Cluster:    "demo-cluster",
		OapVersion: "v0-test",
		Now:        fixedTime,
		Fixture:    liveFixture(t, nil),
		Expand: func(context.Context, string, string, string) ([]steelthread.Tuple, error) {
			return []steelthread.Tuple{
				{Resource: "widget_catalog:wc1", Relation: "reader", Subject: "user:alice"},
			}, nil
		},
		MetaTools: []string{"agent_work_complete", "new_operation"},
		// The same names again, because this fixture's Channels contribute no
		// meta tool the rewrite could destroy. The two lists agreeing is the
		// clean case; a case where they DIFFER is what checkFixtureTools exists
		// for, and lives in selfcheck_test.go.
		FixtureTools: []steelthread.FixtureTool{
			{Name: "agent_work_complete"}, {Name: "new_operation"},
		},
		// One entry per Secret the fixture's manifests name, because the leak
		// scan is gated per SECRET: the class's model key, both Channels'
		// credentials, and the identity's static credential. One value standing
		// in for four was what the coarse gate used to accept.
		LiveSecrets: []steelthread.LiveSecret{
			{Name: "demoforge-model-key/api-key", Value: liveModelKeyValue},
			{Name: "demo-chat-slack-creds/bot-token", Value: "xoxb-fixture-only-chat"},
			{Name: "demo-hooks-creds/webhook-secret", Value: "fixture-only-hooks-secret"},
			{Name: "demoforge-mcp-token/access_token", Value: "fixture-only-mcp-token"},
		},
	}
}

// fileNamed returns the emitted fixture file called name, failing with the full
// list when it is absent — a missing file must read as "04-bootstrap.yaml not
// among [...]" rather than as a nil dereference further down.
func fileNamed(t *testing.T, files []steelthread.FixtureFile, name string) steelthread.FixtureFile {
	t.Helper()
	var got []string
	for _, f := range files {
		if f.Name == name {
			return f
		}
		got = append(got, f.Name)
	}
	require.Failf(t, "expected fixture file missing", "%q not among %v", name, got)
	return steelthread.FixtureFile{}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return string(b)
}

// TestCapture_EndToEndFromSyntheticRecords proves the stages compose: records
// in, a complete bundle out, with no cluster and no LLM anywhere in the path.
func TestCapture_EndToEndFromSyntheticRecords(t *testing.T) {
	got, findings, err := steelthread.Capture(syntheticRecords(t), captureInput(t))
	require.NoError(t, err)
	assert.Empty(t, findings)

	require.NotNil(t, got.Bundle.Capture, "a captured bundle must declare its provenance")
	assert.Equal(t, "default/demo-session", got.Bundle.Capture.Session)
	assert.Equal(t, "demo-cluster", got.Bundle.Capture.Cluster)
	assert.Equal(t, fixedTime, got.Bundle.Capture.CapturedAt)
	assert.Equal(t, "v0-test", got.Bundle.Capture.OapVersion)
	assert.Equal(t, "demoprovider/demo-model-1", got.Bundle.Capture.ServedModel,
		"the served model is what makes a stale capture greppable")

	assert.Equal(t, "demo-lists-widgets", got.Bundle.Name)
	assert.Equal(t, "demoforge-agent", got.Bundle.AgentClass)
	assert.Equal(t, "testdata/demo-lists-widgets", got.Bundle.AgentDir)
	assert.Equal(t, []bt.UserTurn{{Text: "list the widgets"}}, got.Bundle.UserTurns)
	assert.NotEmpty(t, got.Bundle.LLM)
	assert.NotEmpty(t, got.Fixture)
	assert.NotEmpty(t, got.Golden)

	// The MCP output is keyed SERVER-side. Keyed LLM-facing it fails at replay
	// as "unknown tool", which mentions nothing about the mapping.
	assert.Contains(t, got.Bundle.ToolOutputs, "list_widgets")
	assert.Equal(t, []string{"no widgets found"}, got.Bundle.Assert.AgentReplyContains)
}

// TestCapture_ASidecarToolboxCallIsCapturedAsAnMCPCall is the end-to-end proof
// of the sidecar half, and the case that made a real capture unusable.
//
// It goes through Capture rather than SelfCheck because the fact under test
// lives in declaredTools: only Capture reads the session's resolved sidecar
// toolboxes and turns them into MCP prefixes. A test that set DeclaredTools by
// hand would assert the self-check's behaviour and prove nothing about the
// derivation that actually got it wrong.
//
// Three claims, because the sidecar has to survive all three to replay:
// the call is not refused as a sandbox call, its output is keyed SERVER-side
// (what h.MCP.OnTool registers), and the SidecarToolbox CR reaches the fixture
// so the replayed AgentClass's ref validation resolves.
func TestCapture_ASidecarToolboxCallIsCapturedAsAnMCPCall(t *testing.T) {
	recs := syntheticRecords(t)
	// The respond_to_user at turn 3 needs its own result before another
	// assistant turn, or the fold hands step 2 an empty Expect and the capture
	// is refused for THAT instead — a refusal that would pass this test's
	// sandbox assertion while proving nothing.
	recs.Turns = append(recs.Turns,
		toolResult(4, "tu_2", `{"delivered":true}`, false),
		assistantCall(5, "tu_3", "kube_pods", `{"operation_id":"op-2","_reason":"check","args":{}}`),
		toolResult(6, "tu_3", `{"pods":["a"]}`, false),
	)

	in := captureInput(t)
	in.Fixture = liveFixture(t, withSidecarToolbox)

	got, findings, err := steelthread.Capture(recs, in)
	require.NoError(t, err)

	report, _ := steelthread.FormatFindings(findings)
	assert.False(t, steelthread.HasHardFinding(findings),
		"a sidecar tool is an ordinary MCP tool at replay and must be capturable:\n%s", report)
	for _, f := range findings {
		assert.NotEqual(t, steelthread.CodeUnroutableToolCall, f.Code,
			"the sidecar prefix came from the resolved status, so this call resolves to a declared "+
				"prefix; calling it unroutable is the defect that refused a real capture")
	}

	// Asserted on the CONTENT, not on the key's presence.
	//
	// A presence check passes vacuously here: addPlaceholderToolOutputs
	// synthesizes an empty `{}` entry for every DECLARED tool the fold recorded
	// nothing for, so "pods" is a key even when the fold keyed the observed
	// output LLM-facing and the entry is invented. Mutation-proven — deriving
	// the prefix from rt.Ref instead of rt.Name left a presence check green.
	assert.JSONEq(t, `{"pods":["a"]}`, string(got.Bundle.ToolOutputs["pods"]),
		"toolOutputs is keyed server-side because that is what h.MCP.OnTool takes; an empty {} here "+
			"means the fold keyed the OBSERVED output somewhere else and this entry was synthesized")
	assert.NotContains(t, got.Bundle.ToolOutputs, "kube_pods",
		"an LLM-facing key is never registered on the stub, so the replay would fail as an unknown tool")

	// The sidecar's DECLARED-but-uncalled tool needs a placeholder for the same
	// reason a remote MCP server's does, and the consequence of missing one is
	// worse than a single absent tool: mcp.Synthesize returns an error when the
	// allowlist names a tool the live server does not expose, so `nodes` having
	// no entry (hence no OnTool registration, hence absent from the stub's
	// tools/list) fails synthesis for the WHOLE toolbox — `pods` included.
	assert.Contains(t, got.Bundle.ToolOutputs, "nodes",
		"a declared sidecar tool the transcript never called still needs a placeholder, or synthesis "+
			"of the entire toolbox fails at replay")

	assert.Contains(t, string(fileNamed(t, got.Fixture, "02a-sidecartoolbox.yaml").YAML), "kube-tb",
		"without the SidecarToolbox CR the replayed AgentClass never reaches Valid=True")
}

// TestCapture_EmitsTheDerivedSeedAsABootstrapManifest pins that the
// relationships the run's authorization depended on reach the fixture at all.
// Derived and then dropped, the replay denies where the real run allowed — and
// the bundle's own assertions would report it as a product bug.
func TestCapture_EmitsTheDerivedSeedAsABootstrapManifest(t *testing.T) {
	got, findings, err := steelthread.Capture(syntheticRecords(t), captureInput(t))
	require.NoError(t, err)
	require.Empty(t, findings)

	f := fileNamed(t, got.Fixture, "04-bootstrap.yaml")
	var bs spiceboxv1alpha1.SpiceDBBootstrap
	require.NoError(t, yaml.Unmarshal(f.YAML, &bs))
	assert.Equal(t, "SpiceDBBootstrap", bs.Kind)
	require.Len(t, bs.Spec.Relationships, 1)
	assert.Equal(t, "widget_catalog", bs.Spec.Relationships[0].Resource.Type)
	assert.Equal(t, "wc1", bs.Spec.Relationships[0].Resource.ID)
	assert.Equal(t, "reader", bs.Spec.Relationships[0].Relation)
	assert.Equal(t, "user", bs.Spec.Relationships[0].Subject.Type)
	assert.Equal(t, "alice", bs.Spec.Relationships[0].Subject.ID)
	assert.False(t, bs.Spec.Relationships[0].Subject.Canonicalize,
		"a recorded subject is ALREADY canonical; canonicalizing again would rewrite it")
}

// TestCapture_SuppressesTheTriggerSessionsSynthesizedFirstTurn is R4.
//
// A trigger-started session's transcript turn 0 is the prompt the runner
// SYNTHESIZED from the delivery, not something a person typed. Fold promotes it
// to UserTurns[0] and cannot know better — it never reads Records.Trigger.
// Left in, the bundle would BOTH fire the webhook and send a message nobody
// typed, and the loader refuses a bundle carrying both.
func TestCapture_SuppressesTheTriggerSessionsSynthesizedFirstTurn(t *testing.T) {
	recs := syntheticRecords(t)
	recs.Turns[0] = userText(0, "Pull request demo-org/demo-repo#4 was opened")
	recs.Trigger = &triggerdelivery.Content{
		Kind:       "demoforge",
		Event:      "pull_request",
		ChannelKey: "pr:demo-org/demo-repo#4",
		Body:       []byte(`{"action":"opened"}`),
	}
	in := captureInput(t)
	in.Fixture.TriggerChannel = "demo-hooks"

	got, findings, err := steelthread.Capture(recs, in)
	require.NoError(t, err)
	assert.Empty(t, findings)

	assert.Empty(t, got.Bundle.UserTurns,
		"a triggered bundle must not also send a message nobody typed")
	require.NotNil(t, got.Bundle.Trigger, "a triggered session's bundle needs something to start the run")
	assert.Equal(t, "demo-hooks", got.Bundle.Trigger.Channel)
	assert.Equal(t, "pull_request", got.Bundle.Trigger.Event)
	assert.Equal(t, "pr:demo-org/demo-repo#4", got.Bundle.Trigger.ChannelKey)
	assert.NotEmpty(t, got.Bundle.Trigger.Payload, "the driver reads the body from a file beside the bundle")
	assert.Equal(t, []byte(`{"action":"opened"}`), got.TriggerPayload,
		"the payload is re-signed VERBATIM; a reformatted body fails HMAC verification")

	// The divergence check on step 0 survives: the replay's runner synthesizes
	// its own prompt from the same delivery, so the Expect still describes the
	// request that step is the answer to. Only the typed turn is suppressed.
	require.NotEmpty(t, got.Bundle.LLM)
	assert.Equal(t, "Pull request demo-org/demo-repo#4 was opened", got.Bundle.LLM[0].Expect.UserTextContains)
}

// TestCapture_DerivesGuardConfiguredFromTheClass is half of R5.
//
// SelfCheckInput.GuardConfigured is what tells an empty TransformedToolUseIDs
// that means "no guard rewrote anything" from one that means "nobody gathered".
// Read from the class the capture already holds rather than passed in, so it
// cannot drift from the manifests the fixture is built out of.
func TestCapture_DerivesGuardConfiguredFromTheClass(t *testing.T) {
	in := captureInput(t)
	in.Fixture.Class.Spec.ToolGuard = &spiceboxv1alpha1.ToolGuardPolicy{
		Rules: []spiceboxv1alpha1.ToolGuardRule{{Match: spiceboxv1alpha1.ToolGuardMatch{Tool: "*"}}},
	}

	_, findings, err := steelthread.Capture(syntheticRecords(t), in)
	require.NoError(t, err)
	f := findByCode(t, findings, steelthread.CodeGuardScanSkipped)
	assert.Equal(t, steelthread.SeverityHard, f.Severity)
}

// TestCapture_RefusesWithoutAnExpander pins that a nil Expander is an error
// rather than a seed of zero tuples. Silently expanding to nothing would emit a
// fixture that denies where the run allowed, and the failure would surface
// minutes away in a replay pointing at the wrong layer.
func TestCapture_RefusesWithoutAnExpander(t *testing.T) {
	in := captureInput(t)
	in.Expand = nil

	_, _, err := steelthread.Capture(syntheticRecords(t), in)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Expand")
}

// TestCapture_IsDeterministic pins that capturing the same records twice yields
// byte-identical output. Without it a re-capture produces a diff of noise and
// nobody reads it — the same failure mode as a golden regenerated unread.
func TestCapture_IsDeterministic(t *testing.T) {
	a, _, err := steelthread.Capture(syntheticRecords(t), captureInput(t))
	require.NoError(t, err)
	b, _, err := steelthread.Capture(syntheticRecords(t), captureInput(t))
	require.NoError(t, err)

	assert.Equal(t, mustJSON(t, a.Bundle), mustJSON(t, b.Bundle))
	assert.Equal(t, a.Golden, b.Golden)
	require.Equal(t, len(a.Fixture), len(b.Fixture))
	for i := range a.Fixture {
		assert.Equal(t, a.Fixture[i].Name, b.Fixture[i].Name)
		assert.Equal(t, string(a.Fixture[i].YAML), string(b.Fixture[i].YAML))
	}
}

// resultFixture is a minimal Result, for the WriteResult cases that care about
// the on-disk layout rather than about what produced it.
func resultFixture(t *testing.T) steelthread.Result {
	t.Helper()
	got, _, err := steelthread.Capture(syntheticRecords(t), captureInput(t))
	require.NoError(t, err)
	return got
}

// TestWriteResult_ProducesTheLayoutTheDriverDiscovers pins the on-disk shape.
// The driver discovers a scenario by the presence of bundle.json, so a capture
// that wrote its files anywhere else produces a directory the suite silently
// skips — which reads as a passing suite.
func TestWriteResult_ProducesTheLayoutTheDriverDiscovers(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, steelthread.WriteResult(dir, resultFixture(t)))

	assert.FileExists(t, filepath.Join(dir, "bundle.json"))
	assert.FileExists(t, filepath.Join(dir, "trace.golden"))
	assert.FileExists(t, filepath.Join(dir, "03-agent.yaml"))

	raw, err := os.ReadFile(filepath.Join(dir, "bundle.json"))
	require.NoError(t, err)
	assert.Equal(t, byte('\n'), raw[len(raw)-1], "a file without a trailing newline diffs badly forever after")
	assert.Contains(t, string(raw), "\n  \"name\"", "the bundle is indented; a one-line bundle is unreviewable")
}

// TestWriteResult_WritesTheTriggerPayloadWhereTheBundleNamesIt pins the one
// file whose path the bundle itself states. Written elsewhere, the driver fails
// on a missing payload rather than on anything about the capture.
func TestWriteResult_WritesTheTriggerPayloadWhereTheBundleNamesIt(t *testing.T) {
	recs := syntheticRecords(t)
	recs.Trigger = &triggerdelivery.Content{
		Kind: "demoforge", Event: "pull_request", ChannelKey: "pr:demo-org/demo-repo#4",
		Body: []byte(`{"action":"opened"}`),
	}
	in := captureInput(t)
	in.Fixture.TriggerChannel = "demo-hooks"
	res, _, err := steelthread.Capture(recs, in)
	require.NoError(t, err)

	dir := t.TempDir()
	require.NoError(t, steelthread.WriteResult(dir, res))

	body, err := os.ReadFile(filepath.Join(dir, "payloads", res.Bundle.Trigger.Payload))
	require.NoError(t, err)
	assert.Equal(t, `{"action":"opened"}`, string(body))
}

// TestWriteResult_RefusesANonEmptyDirectoryUnlessOverwritten pins the guard on
// a mistyped --out. Merging two captures into one directory produces a
// scenario that is neither, and nothing about the resulting failure would point
// back at the typo.
func TestWriteResult_RefusesANonEmptyDirectoryUnlessOverwritten(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "bundle.json"), []byte("{}"), 0o644))

	err := steelthread.WriteResult(dir, resultFixture(t))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not empty")

	raw, err := os.ReadFile(filepath.Join(dir, "bundle.json"))
	require.NoError(t, err)
	assert.Equal(t, "{}", string(raw), "a refused write must leave the directory untouched")

	require.NoError(t, steelthread.WriteResult(dir, resultFixture(t), steelthread.Overwrite()))
	raw, err = os.ReadFile(filepath.Join(dir, "bundle.json"))
	require.NoError(t, err)
	assert.Contains(t, string(raw), "demo-lists-widgets")
}

// TestWriteResult_OverwriteReplacesRatherThanMerges is the other half of the
// same guard, and the half a flag named --overwrite has to honour.
//
// An overwrite that only skipped the emptiness check would LEAVE the previous
// capture's files. Re-capture a session whose class has since dropped an
// MCPServer and the old 02-mcpserver.yaml stays behind — and the harness
// applies EVERY *.yaml in an agentDir, so the replay boots a server the
// captured session never had. That is precisely the merge the refusal exists to
// prevent, arriving through the front door.
func TestWriteResult_OverwriteReplacesRatherThanMerges(t *testing.T) {
	dir := t.TempDir()
	// A PREVIOUS capture, marker included: replacing one is the whole use case,
	// and the marker is what tells this directory apart from a mistyped path
	// (see TestWriteResult_OverwriteRefusesADirectoryThatIsNotACapture).
	require.NoError(t, os.WriteFile(filepath.Join(dir, "bundle.json"), []byte(`{"name":"an-older-capture"}`), 0o644))
	stale := filepath.Join(dir, "02-mcpserver.yaml")
	require.NoError(t, os.WriteFile(stale, []byte("kind: MCPServer\n"), 0o644))
	stalePayloads := filepath.Join(dir, "payloads")
	require.NoError(t, os.MkdirAll(stalePayloads, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(stalePayloads, "delivery.json"), []byte("{}"), 0o644))

	// The fixture this capture emits has no MCPServer document and no trigger,
	// so both stale entries are files the new capture would never write.
	in := captureInput(t)
	in.Fixture.MCPServers = nil
	in.Fixture.Class.Spec.MCPServers = nil
	recs := syntheticRecords(t)
	recs.Turns = recs.Turns[:1]
	recs.Turns = append(recs.Turns, assistantCall(1, "tu_2", "respond_to_user", `{"text":"nothing to list"}`))
	res, _, err := steelthread.Capture(recs, in)
	require.NoError(t, err)

	require.NoError(t, steelthread.WriteResult(dir, res, steelthread.Overwrite()))

	assert.NoFileExists(t, stale,
		"a stale manifest survives into the replayed agentDir, which applies every *.yaml it finds")
	assert.NoDirExists(t, stalePayloads,
		"a stale delivery body would be signed and posted by a bundle that no longer names one")
	assert.FileExists(t, filepath.Join(dir, "bundle.json"))
	assert.FileExists(t, filepath.Join(dir, "03-agent.yaml"))
}

// TestWriteResult_OverwriteRefusesADirectoryThatIsNotACapture bounds what the
// flag is allowed to delete.
//
// Overwriting REPLACES a previous capture, which means removing what is there.
// Unbounded, that turns one mistyped word — `--output .` from a repo root, or a
// testdata PARENT instead of the bundle inside it — into a recursive delete of
// state the capture never created. The marker is bundle.json, the same file the
// suite discovers a scenario by: a non-empty directory without it is not a
// capture, and is exactly the typo the non-overwrite refusal already catches.
//
// The assertion that matters is the SECOND one. A refusal that had already
// deleted something would be no better than no refusal at all, so the test
// requires the pre-existing file to survive rather than only requiring an error.
func TestWriteResult_OverwriteRefusesADirectoryThatIsNotACapture(t *testing.T) {
	dir := t.TempDir()
	// Somebody's actual work, in a directory that is not a capture.
	notes := filepath.Join(dir, "NOTES.md")
	require.NoError(t, os.WriteFile(notes, []byte("do not delete me\n"), 0o644))
	sub := filepath.Join(dir, "some-other-bundle")
	require.NoError(t, os.MkdirAll(sub, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(sub, "bundle.json"), []byte("{}"), 0o644))

	err := steelthread.WriteResult(dir, resultFixture(t), steelthread.Overwrite())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a capture")
	assert.Contains(t, err.Error(), "bundle.json", "the error must name what it looked for")

	assert.FileExists(t, notes, "a refused overwrite must delete NOTHING")
	assert.FileExists(t, filepath.Join(sub, "bundle.json"),
		"a bundle.json one level DOWN does not make the parent a capture; the child is somebody's scenario")

	// And the same directory accepts the capture once it IS one, so the
	// predicate bounds the delete rather than blocking the use case.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "bundle.json"), []byte("{}"), 0o644))
	require.NoError(t, steelthread.WriteResult(dir, resultFixture(t), steelthread.Overwrite()))
	assert.NoFileExists(t, notes, "replacing a capture still replaces it whole")
}

// TestWriteResult_OverwriteRefusesADirectoryWhoseMarkerIsADirectory closes the
// one gap the sibling test above leaves open.
//
// That test's stray entries are named scenario-one, some-other-bundle and the
// like, so the plain name comparison in holdsACapture already excludes them —
// its !e.IsDir() guard never decides anything there. The guard matters for
// exactly one shape: an entry that IS named bundle.json and is a DIRECTORY.
//
// Narrow, and worth a test anyway because of where it sits. If a directory
// named bundle.json qualified its parent as a capture, the RemoveAll loop would
// run over a directory that is not one — and every other guard on this
// destructive path is covered.
func TestWriteResult_OverwriteRefusesADirectoryWhoseMarkerIsADirectory(t *testing.T) {
	dir := t.TempDir()
	// The marker NAME, but a directory. Nothing wrote a capture here.
	markerDir := filepath.Join(dir, "bundle.json")
	require.NoError(t, os.MkdirAll(markerDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(markerDir, "inside.txt"), []byte("kept\n"), 0o644))
	notes := filepath.Join(dir, "NOTES.md")
	require.NoError(t, os.WriteFile(notes, []byte("do not delete me\n"), 0o644))

	err := steelthread.WriteResult(dir, resultFixture(t), steelthread.Overwrite())
	require.Error(t, err, "a directory named bundle.json is not a capture's bundle.json")
	assert.Contains(t, err.Error(), "not a capture")

	assert.FileExists(t, notes, "a refused overwrite must delete NOTHING")
	assert.FileExists(t, filepath.Join(markerDir, "inside.txt"),
		"the marker-named DIRECTORY and its contents survive too")
}

// TestCapture_RefusesATriggeredSessionAPersonAlsoTypedInto pins the one shape
// that emitted a bundle the loader refuses while reporting ZERO findings.
//
// A webhook opens the session and someone then replies in the thread. The
// assembly step drops only the SYNTHESIZED index-0 turn, so UserTurns is still
// non-empty when Trigger is set — and a bundle starts one way or the other,
// never both. Nothing else in the self-check looks at the assembled bundle, so
// before checkBundle the capture wrote the file and the refusal surfaced
// minutes later in another suite, naming the bundle rather than the capture.
func TestCapture_RefusesATriggeredSessionAPersonAlsoTypedInto(t *testing.T) {
	recs := syntheticRecords(t)
	recs.Turns[0] = userText(0, "Pull request demo-org/demo-repo#4 was opened")
	// The human follow-up: a person read the agent's answer and asked for more.
	recs.Turns = append(recs.Turns,
		userText(4, "what about the archived ones?"),
		assistantCall(5, "tu_3", "respond_to_user", `{"text":"none archived either"}`),
	)
	recs.Trigger = &triggerdelivery.Content{
		Kind:       "demoforge",
		Event:      "pull_request",
		ChannelKey: "pr:demo-org/demo-repo#4",
		Body:       []byte(`{"action":"opened"}`),
	}
	in := captureInput(t)
	in.Fixture.TriggerChannel = "demo-hooks"

	got, findings, err := steelthread.Capture(recs, in)
	require.NoError(t, err)

	f := findByCode(t, findings, steelthread.CodeBundleInvalid)
	assert.Equal(t, steelthread.SeverityHard, f.Severity)
	assert.Contains(t, f.Message, "userTurns OR from a trigger, not both",
		"the finding must name WHICH precondition failed")

	// The bundle really is in the refused shape — the finding is about the
	// value that would have been written, not a hypothetical.
	assert.Equal(t, []bt.UserTurn{{Text: "what about the archived ones?"}}, got.Bundle.UserTurns)
	require.NotNil(t, got.Bundle.Trigger)
	require.Error(t, got.Bundle.Validate(),
		"the same method the replay driver loads through must refuse it")
}

// TestCapture_RefusesATriggerItCannotAttributeToAChannel covers the mirror of
// the no-trigger-record warning, and it is HARD where that one is a warning.
//
// With a delivery recorded but no input Channel identified, both halves of the
// trigger handling go missing at once: bt.Trigger cannot be emitted (the driver
// signs with the named Channel's own credentials), and the index-0 suppression
// is keyed off the same pair — so the prompt the RUNNER synthesized ships as a
// message the bundle claims a person typed. Neither is visible in the files.
func TestCapture_RefusesATriggerItCannotAttributeToAChannel(t *testing.T) {
	recs := syntheticRecords(t)
	recs.Trigger = &triggerdelivery.Content{
		Kind: "demoforge", Event: "pull_request", ChannelKey: "pr:demo-org/demo-repo#4",
		Body: []byte(`{"action":"opened"}`),
	}
	in := captureInput(t) // deliberately no Fixture.TriggerChannel

	got, findings, err := steelthread.Capture(recs, in)
	require.NoError(t, err)

	f := findByCode(t, findings, steelthread.CodeTriggerChannelUnknown)
	assert.Equal(t, steelthread.SeverityHard, f.Severity)
	assert.Contains(t, f.Message, "pr:demo-org/demo-repo#4")
	assert.Nil(t, got.Bundle.Trigger, "there is no Channel to sign the delivery with")
	assert.NotEmpty(t, got.Bundle.UserTurns,
		"the synthesized turn is NOT suppressed here — which is exactly what the finding is about")
}

// TestCapture_SynthesizesAPlaceholderForADeclaredToolNeverCalled is the fix for
// the measurement that mattered: ten of eleven bronze scenarios were refused,
// and this rule was the largest single reason.
//
// An MCP server exposes a catalogue and any one session touches part of it, so
// a fixture declaring a tool the transcript never called is the ORDINARY shape
// of a real session, not a defect. The entry still has to exist — class
// admission validates the allowlist and a missing handler surfaces as
// AllowlistDrift — which is why an authored bundle carries a hand-written
// placeholder in exactly this slot. The capture now writes the same thing, and
// says that it did.
func TestCapture_SynthesizesAPlaceholderForADeclaredToolNeverCalled(t *testing.T) {
	in := captureInput(t)
	in.Fixture = liveFixture(t, func(f *steelthread.FixtureInput) {
		f.MCPServers[0].Spec.Tools = append(f.MCPServers[0].Spec.Tools,
			spiceboxv1alpha1.MCPServerTool{Name: "archive_widget", Intent: "Archive a widget."})
	})

	got, findings, err := steelthread.Capture(syntheticRecords(t), in)
	require.NoError(t, err)

	require.False(t, steelthread.HasHardFinding(findings),
		"a session that simply did not use every tool its agent offers must still be capturable: %v", findings)
	assert.JSONEq(t, `{"results":[]}`, string(got.Bundle.ToolOutputs["list_widgets"]),
		"the OBSERVED output is untouched")
	assert.JSONEq(t, `{}`, string(got.Bundle.ToolOutputs["archive_widget"]),
		"an empty object, not something that looks like data: nothing is known about what this tool returns")

	f := findByCode(t, findings, steelthread.CodePlaceholderToolOutputs)
	assert.Equal(t, steelthread.SeverityWarn, f.Severity)
	assert.Contains(t, f.Message, "archive_widget",
		"a reader must be able to tell an invented entry from a recorded one")
	assert.NotContains(t, f.Message, "list_widgets",
		"naming a tool that WAS observed would make the notice useless")
}

// TestCapture_ARefusedSessionEmits is the flagship shape, and the one the
// capture used to refuse outright.
//
// A denied tool never reaches the MCP server, so the error the model saw was
// written by the gate — and the replay boots the same fixture, with the same
// derived seed and the same gate configuration, and writes it again. There is
// nothing to can and nothing to report. The declared tool still needs its
// placeholder entry, because the stub builds tools/list from its registered
// handlers and class admission validates the allowlist against that list; the
// value is never served, because the replay's gate stops the call first.
func TestCapture_ARefusedSessionEmits(t *testing.T) {
	const refusal = "permission denied: alice does not have list on widget_catalog:wc1"
	recs := syntheticRecords(t)
	recs.Turns[2] = toolResult(2, "tu_1", refusal, true)
	recs.DecisionsByToolCall = map[string][]authzdecision.Decision{
		"tu_1": {{
			Outcome: authzdecision.OutcomeDenied, Subject: "alice",
			ResourceType: "widget_catalog", ResourceID: "wc1", Permission: "list",
			Message: refusal,
		}},
	}

	got, findings, err := steelthread.Capture(recs, captureInput(t))
	require.NoError(t, err)

	assert.False(t, steelthread.HasHardFinding(findings),
		"a session whose evidence is a refusal is exactly what this feature is for: %v", findings)
	assert.Empty(t, got.Bundle.ToolErrors,
		"the upstream never answered; canning an error would dress a platform refusal up as one")
	assert.JSONEq(t, `{}`, string(got.Bundle.ToolOutputs["list_widgets"]),
		"the entry exists only so the tool appears in the stub's tools/list")
	assert.Equal(t, steelthread.SeverityWarn,
		findByCode(t, findings, steelthread.CodePlaceholderToolOutputs).Severity,
		"and the reader is told which outputs were invented rather than observed")
}

// TestCapture_AnUpstreamFailureEmitsAToolError is the other half of the
// classification: the stub WAS called and answered with a failure, so the
// replay has to be able to make it fail the same way.
//
// The tool gets BOTH a toolErrors entry and a placeholder output, and needs
// both. OnToolError wins over OnTool for the same name, so the placeholder is
// never served — but the stub's tools/list is built from OnTool registrations
// alone, so without it the tool is missing from the catalogue and the class
// fails admission on allowlist drift.
func TestCapture_AnUpstreamFailureEmitsAToolError(t *testing.T) {
	const upstream = `mcp: mcp call "list_widgets": upstream returned 503`
	recs := syntheticRecords(t)
	recs.Turns[2] = toolResult(2, "tu_1", upstream, true)

	got, findings, err := steelthread.Capture(recs, captureInput(t))
	require.NoError(t, err)

	assert.False(t, steelthread.HasHardFinding(findings),
		"one tool that failed the same way on every call is representable: %v", findings)
	assert.Equal(t, map[string]bt.ToolError{"list_widgets": {Message: upstream}}, got.Bundle.ToolErrors)
	assert.JSONEq(t, `{}`, string(got.Bundle.ToolOutputs["list_widgets"]),
		"without this the tool is absent from tools/list and the class fails admission")
}

// TestCapture_RefusesWhenAShadowedSecretsValueWasNotRead pins the WIRING
// between the rewrite and the check, which is a separate fact from either half
// working.
//
// Without it, Capture could stop passing RewriteResult.ShadowedSecrets to
// SelfCheck and every other test here would still pass: a correct capture
// produces no finding, so a gate nobody wired is indistinguishable from one that
// found nothing. Same fail-open shape as GuardConfigured and
// ToolCatalogCheck.Consulted — and it was a surviving mutant here before it was
// a test.
func TestCapture_RefusesWhenAShadowedSecretsValueWasNotRead(t *testing.T) {
	in := captureInput(t)
	// Drop exactly one — the identity's static credential. The other three are
	// still read, so a gate that merely counted "is anything scannable at all?"
	// would pass this; only a per-Secret gate refuses it.
	in.LiveSecrets = slices.DeleteFunc(slices.Clone(in.LiveSecrets), func(s steelthread.LiveSecret) bool {
		return strings.HasPrefix(s.Name, "demoforge-mcp-token/")
	})
	require.Len(t, in.LiveSecrets, 3, "exactly one entry must have been dropped, or this asserts nothing")

	_, findings, err := steelthread.Capture(syntheticRecords(t), in)
	require.NoError(t, err)

	f := findByCode(t, findings, steelthread.CodeSecretCheckSkipped)
	assert.Equal(t, steelthread.SeverityHard, f.Severity)
	assert.Contains(t, f.Message, "demoforge-mcp-token",
		"the finding must name the Secret whose live value nobody read")
}

// TestCapture_StampsTheDefaultUserTheFixtureAddressed closes the classic
// fail-open: the rewrite derives the user its emitted UserIdentity is named
// after, and Capture has to carry that onto the bundle.
//
// Nothing else notices if it does not. A correct capture emits no findings, so
// a bundle that simply omits defaultUser looks exactly like one that never
// needed it — and the replay then sends as the harness's own default, resolves
// an empty catalog, and parks in AwaitingCredentials until the credential-link
// deadline, minutes away from anything naming the cause. Same shape as
// TestCapture_RefusesWhenAShadowedSecretsValueWasNotRead.
func TestCapture_StampsTheDefaultUserTheFixtureAddressed(t *testing.T) {
	in := captureInput(t)
	in.Fixture = livePassthrough(t, nil)

	res, findings, err := steelthread.Capture(syntheticRecords(t), in)
	require.NoError(t, err)
	text, hard := steelthread.FormatFindings(findings)
	require.Zero(t, hard, "the passthrough fixture must capture cleanly, or this asserts nothing: %s", text)

	assert.Equal(t, "user@example.com", res.Bundle.DefaultUser,
		"the bundle must send as the user the emitted UserIdentity is addressed to")
}

// The negative control: an agent-identity session emits no UserIdentity, so the
// bundle names no user and the harness default stands. Without this, stamping a
// fixed user unconditionally would pass the test above.
func TestCapture_AnAgentIdentitySessionNamesNoDefaultUser(t *testing.T) {
	res, _, err := steelthread.Capture(syntheticRecords(t), captureInput(t))
	require.NoError(t, err)

	assert.Empty(t, res.Bundle.DefaultUser)
}

// TestCapture_AStreamingSandboxResultEmitsWithTheCoverageWarning is the
// end-to-end proof that a black-boxed streaming result reaches the emitted
// bundle AND that the capture says what it cost.
//
// It goes through Capture rather than SelfCheck because both halves are wiring
// only this function holds: the fold names the tool, the self-check turns that
// into the finding, and nothing else fails if the two are never connected. A
// correct capture emits no hard finding either way, so a warning that was
// derived and then dropped on the floor is indistinguishable from one that was
// never earned — the same fail-open shape as
// TestCapture_RefusesWhenAShadowedSecretsValueWasNotRead.
func TestCapture_AStreamingSandboxResultEmitsWithTheCoverageWarning(t *testing.T) {
	const recorded = "status: success (7.272s, $0.09)\nI reviewed the file and found no typos."

	in := captureInput(t)
	in.Fixture = liveFixture(t, withSandboxBundles)
	recs := steelthread.Records{
		Session: "default/demo-session",
		Turns: []memory.Turn{
			userText(0, "review the readme"),
			assistantCall(1, "tu_1", "demo-discovery_demo",
				`{"operation_id":"op-1","_reason":"asked","args":["review"]}`),
			toolResult(2, "tu_1", recorded, false),
			assistantCall(3, "tu_2", "respond_to_user", `{"text":"no typos found"}`),
		},
	}

	res, findings, err := steelthread.Capture(recs, in)
	require.NoError(t, err)
	text, hard := steelthread.FormatFindings(findings)
	require.Zero(t, hard, "a streaming session must EMIT now, not refuse: %s", text)

	raw, ok := res.Bundle.ToolOutputs["demo-discovery_demo"]
	require.True(t, ok, "the streaming result must reach the emitted bundle")
	so, err := bt.DecodeSandboxOutput(raw)
	require.NoError(t, err)
	assert.Equal(t, recorded, so.StreamResult)

	f := findByCode(t, findings, steelthread.CodeStreamResultServedVerbatim)
	assert.Equal(t, steelthread.SeverityWarn, f.Severity)
	assert.Contains(t, f.Message, "demo-discovery_demo")
	assert.Contains(t, f.Message, "NOT covered by this bundle")
}

// The negative control: an ordinary process result is re-composed by the code
// under test, so no coverage is lost and no warning is owed. Without this, a
// self-check that warned unconditionally would pass the test above.
func TestCapture_AnOrdinaryProcessResultOwesNoCoverageWarning(t *testing.T) {
	in := captureInput(t)
	in.Fixture = liveFixture(t, withSandboxBundles)
	recs := steelthread.Records{
		Session: "default/demo-session",
		Turns: []memory.Turn{
			userText(0, "list the widgets"),
			assistantCall(1, "tu_1", "demo-discovery_demo",
				`{"operation_id":"op-1","_reason":"asked","args":["list"]}`),
			toolResult(2, "tu_1",
				"widget-a\n[exit=0; artifacts: stdout=mem://a, stderr=mem://b]\n", false),
			assistantCall(3, "tu_2", "respond_to_user", `{"text":"one widget"}`),
		},
	}

	_, findings, err := steelthread.Capture(recs, in)
	require.NoError(t, err)
	for _, f := range findings {
		assert.NotEqual(t, steelthread.CodeStreamResultServedVerbatim, f.Code,
			"the replay re-composes a process result for real, so nothing went uncovered")
	}
}
