//go:build e2e

// Package threadrun hosts the whole-session replay driver shared by the
// bronzethread and steelthread suites.
//
// It lives outside a _test.go file for exactly one reason: two suites need it.
// The bundle types and dispositions stay in pkg/bronzethread, which imports no
// test machinery — bronze AUTHORS a transcript, steel CAPTURES one, and this
// package cannot tell the difference, which is the point.
package threadrun

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/llm"
	sidecartoolboxsynth "github.com/authzed/openagentprimitives/pkg/agent/tool/sidecartoolbox"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/plangate"
	bt "github.com/authzed/openagentprimitives/pkg/bronzethread"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	fakekind "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/fake"
	"github.com/authzed/openagentprimitives/pkg/controllers/artifactrender"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/authzdecision"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/infoleakageaudit"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/plangateaudit"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/pttag"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/userpreference"
	"github.com/authzed/openagentprimitives/pkg/platform/artifactstore"
	blobstore "github.com/authzed/openagentprimitives/pkg/platform/artifactstore/blob"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/tools/sandboxkinds/pod"
	"github.com/authzed/openagentprimitives/test/e2e"
)

// workshopSidecarToolboxRef is the SidecarToolbox CR name every
// workshop-driving bundle applies its fixture under — matching the CR name
// pkg/tools/workshopmcp/deploy/sidecartoolbox.yaml's header comment names
// ("SidecarToolbox/workshop") and the name plan-8b's own bundle fixture
// applies. Run's sidecar-probe routing closure matches on this to decide
// which stub a resolved sidecar dispatches to; see the closure's own comment.
const workshopSidecarToolboxRef = "workshop"

// Load reads and validates one scenario.
func Load(t *testing.T, dir string) bt.Bundle {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "bundle.json"))
	require.NoError(t, err, "reading bundle")

	var b bt.Bundle
	dec := json.NewDecoder(bytesReader(raw))
	dec.DisallowUnknownFields() // a typo'd key must fail, not be silently ignored
	require.NoError(t, dec.Decode(&b), "decoding bundle %s", dir)

	// Every structural precondition lives on the bundle itself, not here: the
	// steelthread capture's self-check calls the SAME method, so a capture
	// cannot emit a bundle this loader refuses while reporting zero findings.
	// Adding a check here instead of in bt.Bundle.Validate re-opens that gap.
	require.NoError(t, b.Validate(), "bundle %s", dir)
	return b
}

// Run boots the fixture, replays the transcript positionally, and checks the
// bundle's assertions. It returns the harness so a caller can inspect what the
// run produced; both discovery suites ignore it.
func Run(t *testing.T, dir string, b bt.Bundle) *e2e.Harness {
	t.Helper()

	// A bundle driving a real streaming toolkit needs the ToolCall reconciler,
	// the gateway and the fake exec binder; every other bundle is cheaper
	// without them, so this is opt-in by the bundle having a stream to emit.
	streamDriver := streamDriverFor(b)

	// A bundle whose fixture declares a SANDBOX toolkit needs the same three,
	// for the same reason: a sandbox call is a ToolCall CR the reconciler
	// executes through the binder, so with no reconciler running the call is
	// created and never answered — it dies on the tool's own 60s timeout, many
	// steps away from the thing that was actually wrong.
	//
	// Gated on the FIXTURE declaring a sandbox tool, not on the bundle having
	// recorded an output for one. Those differ, and the difference is the whole
	// point: with the narrower gate, deleting a bundle's sandbox output would
	// also silently switch off the controller, so the run would fail by timeout
	// instead of by the responder saying which tool had no recording.
	sandboxNames := sandboxToolNames(t, dir, b)
	sandboxOuts := sandboxOutputs(t, b, sandboxNames)

	// Both routes register through ProgramStreamFunc on the same pod key, so a
	// bundle asking for both would have one silently overwrite the other.
	// Checked here, on the test goroutine, rather than where the registration
	// happens — that runs on the stamping loop, where a Fatal cannot land.
	require.False(t, streamDriver != nil && recordsAStream(sandboxOuts),
		"bundle %q both drives a scripted streaming toolkit (wired by name in scenariohooks.go) and "+
			"records a streamResult of its own; one sandbox pod serves one stream", b.Name)

	// Artifact-delivery surfaces on the fake kind, for a bundle that asked.
	// BEFORE e2e.Start, because the capability set is read when the session's
	// binding is annotated and its meta tools are assembled. Restored on
	// cleanup: bundles share one process, and a flag left on would widen the
	// tool list of every scenario that ran after it.
	if b.FakeDeliverySurfaces {
		t.Cleanup(fakekind.EnableDeliverySurfaces())
	}

	// The ids the bundle says the run minted, wired to the components that mint
	// them. t.Errorf rather than t.Fatalf: the minter is called on whichever
	// goroutine dispatched the tool call, where Fatalf's Goexit would abandon
	// the runner's stack rather than the test's.
	//
	// A family the bundle pinned nothing for gets a nil minter and the component
	// mints its own, exactly as in production — so a bundle that names no
	// operation id makes no claim about how many operations the run opens.
	mintedIDs := bt.NewMintedIDSequence(b.MintedIDs, func(msg string) { t.Errorf("%s", msg) })

	// The ids the bundle says the PROVIDER minted, drawn by the stand-in
	// provider in the same order. A separate sequence from the one above, and
	// deliberately so: those are ids our own components mint through injectable
	// seams, these are a third party's, and the two are found and carried by
	// completely different routes. What they share is the assertion — draw in
	// order, report exhaustion, report leftovers.
	providerIDs := bt.NewMintedIDSequence(providerIDMap(b), func(msg string) { t.Errorf("%s", msg) })

	// The channel directory the fixture's fake kind serves, restored when the
	// bundle finishes. Before e2e.Start, because a kind's advertised lookups
	// decide which meta tools are assembled when the session's binding is
	// annotated — exactly like FakeDeliverySurfaces above.
	if m := standInMentions(b); m != nil {
		t.Cleanup(fakekind.EnableMentionLookups(mentionLookups(m), mentionUsers(m)))
	}

	// The tool set the bundle says the run was offered, per turn. t.Errorf for
	// the same reason the minters use it: the check runs on the turn loop's
	// goroutine, where Fatalf's Goexit would abandon the runner's stack.
	//
	// A bundle that pins no catalog gets a nil filter and nothing held back, so
	// the run offers whatever its fixture composes — the same compatibility
	// rule an unpinned minted-id family takes.
	toolCatalog := bt.NewToolCatalogCheck(b.ToolCatalogs, b.ExpectedExtraTools,
		func(msg string) { t.Errorf("%s", msg) })

	// The meta tools this bundle serves a recorded reply for instead of
	// running. nil for every bundle that cans none, which leaves the assembly
	// untouched.
	metaCanner := bt.NewMetaToolCanner(b.MetaToolReplies)

	h := e2e.Start(t, e2e.Options{
		AgentDir:               b.AgentDir,
		ExtraManifests:         loadManifests(t, dir, b),
		DefaultUser:            b.DefaultUser, // empty keeps the harness default
		WithToolCallController: streamDriver != nil || len(sandboxNames) > 0,
		// See planGateDenialStreakThresholdFor's doc: an operator-wide flag in
		// production, not a bt.Bundle field.
		PlanGateDenialStreakThreshold: planGateDenialStreakThresholdFor(b),
		NewOperationID:                mintedIDs.Minter(bt.FamilyOperation),
		NewArtifactID:                 mintedIDs.Minter(bt.FamilyArtifact),
		NewRenderName:                 renderNameMinter(mintedIDs),
		NewRevisionID:                 revisionIDMinter(mintedIDs),
		HoldToolsFromAssembly:         toolCatalog.HeldFromAssembly(),
		FilterOfferedTools:            toolCatalog.Filter(),
		ReplaceAssembledTool:          metaCanner.Replacer(),
	})

	// Canned outputs for the tools whose disposition is ReplayOutput. Keyed by
	// SERVER-side name, which is what h.MCP.OnTool takes.
	//
	// A SANDBOX tool's entry is skipped here and served by the fake exec binder
	// instead (see stampBundleSessionsReady): the two transports share this map
	// because a bundle replays black boxes, but they do not share a stub.
	// Registering a sandbox name with the MCP server as well would put a tool in
	// the stub's tools/list that no MCPServer declares and nothing ever calls.
	//
	// Served through bt.CannedHandler rather than decoded here, so the model is
	// handed the bytes the bundle recorded — key order and integer precision
	// included. See bt.served for what decoding into a map silently rewrote.
	for name, out := range b.ToolOutputs {
		if sandboxNames[name] {
			continue
		}
		if handler := bt.CannedHandler(out); handler != nil {
			h.MCP.OnTool(name, handler)
		}
	}

	// Tools whose successive calls returned different results. Registered after
	// the constant handlers; ValidateToolOutputs has already refused any tool
	// declared in both, so these cannot silently overwrite one.
	for name, seq := range b.ToolOutputSequence {
		if handler := bt.SequencedHandler(seq); handler != nil {
			h.MCP.OnTool(name, handler)
		}
	}

	// Tools whose UPSTREAM answered with a failure. OnToolError wins over
	// OnTool for the same name, so the order relative to the loops above does
	// not matter and the tool's ToolOutputs entry — which the bundle still
	// needs, so the tool appears in the stub's tools/list at all — is never
	// served.
	//
	// A call the PLATFORM refused is deliberately not here and has no bundle
	// field: a denied tool never reaches this stub, and the replay's own gate
	// refuses it again from the fixture's seed and gate configuration.
	for name, e := range b.ToolErrors {
		code := e.Code
		if code == 0 {
			code = bt.DefaultToolErrorCode
		}
		h.MCP.OnToolError(name, code, e.Message)
	}

	// A triggered bundle's INPUT Channel has to be credentialed before anything
	// waits on readiness: its webhook signature is verified against its own
	// Secret, and its status surface mints a token from the same one.
	var triggerChannel *spiceboxv1alpha1.Channel
	if b.Trigger != nil {
		triggerChannel = setupTrigger(t, h, b)
		// AFTER setupTrigger, which is what stands the fixture provider up.
		// nil when the bundle pinned none, leaving the fixture's own counter in
		// place — the compatibility path every authored bundle takes.
		h.FakeGitHub.SetCheckRunIDMinter(providerIDs.Minter(bt.FamilyTriggerStatus))
	}

	// Stand in for four controllers the harness does not run. A fixture with
	// sandbox or sidecar tools cannot resolve without them: the AgentClass
	// waits on toolspec validity AND on any referenced SidecarToolbox's own
	// SpiceboxClass validity (see Harness.StampSpiceboxClassesValid's doc for why the
	// REAL sidecartoolbox.Reconciler made this one load-bearing), and
	// buildSandboxTools skips a bundle whose SpiceboxSession carries no
	// ResolvedClass. A fixture referencing an AgentUI needs the fourth:
	// pkg/controllers/agentclass's validateAgentUI parks the class at
	// Valid=False/AgentUIInvalid until the referenced AgentUI's own Valid
	// condition is True, and pkg/controllers/agentui is not registered in this
	// harness either (see stampAgentUIsValid, and
	// test/e2e/scenarios/agentui/view_capability_test.go's markAgentUIValid,
	// which stood in for it ad hoc before any bundle needed the same thing).
	// Every hand-written scenario needing one of these does it inline; a
	// bundle is data, with nowhere to put an imperative step. All four are
	// no-ops for a fixture that declares none of these kinds.
	h.StampSpiceboxClassesValid()
	stampToolspecsValid(t, h)
	stampAgentUIsValid(t, h)
	stampBundleSessionsReady(t, h, streamDriver, sandboxOuts)

	// Point the runner's sidecar probe AND dispatch at the harness's MCP
	// stub(s), the sidecar analogue of the {{MCP_URL}} substitution
	// loadManifests applies to an MCPServer CR. A sidecar's endpoint is not a
	// manifest field a sentinel can be written into — it is derived at runtime
	// from the resolved pod IP and an operator-allocated port — so the
	// redirect has to happen through this seam instead.
	//
	// Routed by rt.Ref: the workshop toolbox (SidecarToolbox/workshop — see
	// pkg/tools/workshopmcp/deploy/sidecartoolbox.yaml's header) dispatches to
	// the REAL workshop server h.WorkshopMCPURL() mounts (see mountWorkshopMCP
	// in test/e2e/workshop_mcp.go for what that proves and what it deliberately
	// does not); every other toolbox keeps dispatching to the canned h.MCP
	// stub, byte-identical to before this switch existed.
	//
	// h.WorkshopMCPURL() is a bare origin (mountWorkshopMCP mounts the MCP
	// handler at "/mcp" only, unlike the canned MCPStub which answers at "/").
	// Appending sidecartoolboxsynth.EndpointPath(rt) — derived from the
	// resolved toolbox's OWN spec.transport.path, the same normalizer
	// buildSidecarTools' own probeURL construction uses — is required: without
	// it every probe/dispatch to the workshop route 404s at the mux, which the
	// go-sdk's streamable-HTTP client surfaces as an opaque "initialize: ...
	// session not found" rather than a 404, so the failure reads nothing like
	// a missing path. Found by running this bundle against this exact seam
	// (plan-8b Task 3's fail-first step).
	//
	// Unconditional, like SetArtifactStore above: a bundle whose fixture
	// declares no SidecarToolbox resolves no sidecar, so the closure is never
	// called and the bundle is unaffected.
	h.SetSidecarProbeURL(func(rt spiceboxv1alpha1.ResolvedSidecarToolbox) string {
		if rt.Ref == workshopSidecarToolboxRef {
			return h.WorkshopMCPURL() + sidecartoolboxsynth.EndpointPath(rt)
		}
		return h.MCP.URL()
	})

	// The artifact service backs every artifact_* meta tool; without a store
	// wired the artifacts capability skips and a class that granted it silently
	// gets no artifact tools at all. Wired for every bundle because granting
	// the capability is already the per-class opt-in — a class that does not
	// grant it is unaffected.
	artifactStore := blobstore.NewMem()
	t.Cleanup(func() { _ = artifactStore.Close() })
	h.SetArtifactStore(artifactStore)
	runArtifactRenders(t, h, artifactStore)

	h.WaitForAgentClassValid(b.AgentClass, 30*time.Second)
	// Every OTHER class the fixture declares, not just the one the bundle
	// starts. A delegated child's class has to be Valid before the delegation
	// reaches it, and nothing else waits for it: tool handlers are registered
	// AFTER the manifests are applied, so a class whose MCPServer's allowlist
	// only matches once the stub knows its tools is briefly invalid by
	// construction. The primary class gets a wait and rides it out; a child
	// class hit that window and the delegation failed with
	// AgentClassNotValid — a fixture problem that reads exactly like a broken
	// feature.
	for _, name := range h.AgentClassNames() {
		if name == b.AgentClass {
			continue
		}
		h.WaitForAgentClassValid(name, 30*time.Second)
	}

	// Wait for the fixture's seed relationships to land before any turn runs.
	//
	// Without this a bundle races the SpiceDBBootstrap reconcile: the first
	// permissioned call can be Checked against a datastore that does not yet
	// hold the tuples the fixture depends on, and the tool comes back denied.
	// The race was invisible while no bundle asserted on the authz outcome —
	// a scripted transcript replies the same whether its tool succeeded or
	// returned a permission error.
	h.WaitForSpiceDBBootstrap(60 * time.Second)

	// A SEPARATE readiness gap from the one above: guardian's async schema
	// composition (the slot_grant_<perm> relation ComposeSlots injects onto
	// each declared slot's resource definition) is its own reconcile loop,
	// independent of both AgentClass Valid=True and any SpiceDBBootstrap CR.
	// WaitForSpiceDBBootstrap's poll loop has been covering this by accident
	// for every bundle that ships its OWN bootstrap tuples; a bundle that (by
	// design) ships none — proving a slot binds with no standing tuple
	// anywhere — gets no wait at all and can race a slot-gated Check against a
	// not-yet-composed schema. See e2e.WaitForComposedSchema's doc.
	waitForComposedSlotSchema(t, h, b)

	// The base barrier under both waits above: `definition agentsession` being
	// live in the composed schema. channelsd's started_by write is the first
	// thing every session does, and against a not-yet-composed schema it dies
	// with Failed/AuthzWriteFailed — a session that never runs a turn at all.
	// WaitForAgentClassValid does NOT cover it (the guardian composes strictly
	// downstream of Valid=True), and the two waits above cover it only for a
	// bundle that happens to ship bootstrap tuples or declare slots. A bundle
	// that ships neither had no barrier.
	h.WaitForAuthzSchema(60 * time.Second)

	// A bundle's own seeded fixture state — a sole_user edge, a saved
	// preference value — lands AFTER the schema is live (both are ordinary
	// SpiceDB/memory writes, not bootstrap tuples) and BEFORE anything the
	// run does can read them: the trigger delivery below is the earliest
	// point a session — and so a get_preferences call — can exist.
	seedRelationships(t, h, b)
	seedUserPreferences(t, h, b)

	startAutoApprover(t, h, b)

	tr := registerTranscript(t, h, b)

	// Each bundle gets its OWN thread, and therefore its own session.
	//
	// The channel key maps to a session name, and it defaults to one shared
	// string — so every bundle booting the same fixture landed on the SAME
	// AgentSession and inherited the previous bundle's history. The tell was a
	// run where turn 1 of the second bundle opened with `messages=8`. Bundles
	// passed alone and failed in company, positionally: an inherited transcript
	// shifts which model call answers which step. Isolation belongs here rather
	// than in each bundle, so a new scenario cannot forget it.
	if triggerChannel != nil {
		deliverTrigger(t, h, dir, b, triggerChannel)
	}
	// Serve every attachment any turn declares, keyed by a synthetic external
	// ID. Installed ONCE for the whole bundle rather than per turn: the fetch
	// happens inline on channelsd's dispatch goroutine and can outlive the
	// SendUserMessage call that triggered it, so a per-turn install would race
	// its own teardown.
	bundleFiles := map[string]string{} // externalID -> fixture path
	bundleZips := map[string][]byte{}  // externalID -> archive built from ZipFrom
	for i, ut := range b.UserTurns {
		for j, a := range ut.Attachments {
			switch {
			case a.ZipFrom != "":
				// Built at load time from plain files in the fixture, so what the
				// archive contains stays reviewable in the repo.
				bundleZips[attachmentExternalID(i, j)] = buildFixtureZip(t, filepath.Join(dir, a.ZipFrom))
			default:
				bundleFiles[attachmentExternalID(i, j)] = filepath.Join(dir, a.File)
			}
		}
	}
	if len(bundleFiles) > 0 || len(bundleZips) > 0 {
		// The operator's /inbound-asset route has no HTTP surface in the
		// in-process harness, so a bundle carrying files needs a stand-in for
		// it before the first message is sent.
		wireInboundAssets(t, h)
		fakekind.SetAttachmentSource(func(externalID string) (io.ReadCloser, error) {
			if b, built := bundleZips[externalID]; built {
				return io.NopCloser(bytes.NewReader(b)), nil
			}
			path, ok := bundleFiles[externalID]
			if !ok {
				return nil, fmt.Errorf("bronzethread: no attachment for external ID %q", externalID)
			}
			return os.Open(path)
		})
		t.Cleanup(fakekind.ResetAttachmentSource)
	}

	// workshopSessionBound guards bindWorkshopSessionOnce, called after every
	// turn below: a bundle whose fixture routes through the real workshop MCP
	// mount (see mountWorkshopMCP) needs its SessionNamespace/SessionName
	// late-bound exactly once, as soon as a session exists to bind — see that
	// function's own doc for why this lives in the general turn loop rather
	// than a per-bundle hook, and for the no-op it is for every other bundle.
	workshopSessionBound := false
	for i, turn := range b.UserTurns {
		h.SendUserMessage(turn.Text, e2e.InThread("bt-"+b.Name), e2e.WithAttachments(bundleAttachments(t, dir, i, turn)...))
		// Contains and NotContains are positional and run on the SAME reply, so a
		// turn asserting both awaits its reply once and checks presence + absence
		// together. A turn with neither is not awaited (some turns produce no
		// respond_to_user, and ExpectAgentReply would block to the timeout).
		var preds []e2e.ReplyPredicate
		if i < len(b.Assert.AgentReplyContains) {
			preds = append(preds, e2e.Contains(b.Assert.AgentReplyContains[i]))
		}
		if i < len(b.Assert.AgentReplyNotContains) && b.Assert.AgentReplyNotContains[i] != "" {
			preds = append(preds, e2e.NotContains(b.Assert.AgentReplyNotContains[i]))
		}
		if len(preds) > 0 {
			h.ExpectAgentReply(preds...)
		}
		bindWorkshopSessionOnce(t, h, &workshopSessionBound)
	}

	checkTranscriptConsumed(t, b, tr)
	// The phase barrier runs FIRST among the state checks: for a bundle whose
	// run ends without an agent reply (a denied guest start, a forensic hold)
	// nothing above blocked, and every count read before the terminal phase
	// lands — approval prompts, plan-gate records — is a snapshot of a run
	// still in flight. Waiting here settles them all; for every other bundle
	// wantSessionPhaseFor is "" and this is a no-op.
	checkSessionPhase(t, h, b)
	checkApprovalPromptCount(t, h, b)
	checkSystemPrompt(t, h, b)
	checkTriggerStatus(t, h, b)
	checkChannelPosts(t, h, b)
	checkArtifactsDelivered(t, h, b)
	checkNoticesPublished(t, h, b)
	checkNoticeCounts(t, h, b)
	checkPlanGate(t, h, b)
	checkPtTags(t, h, b)
	checkAuthz(t, h, b)
	checkSpiceDB(t, h, b)
	checkTranscriptLength(t, h, b)
	checkToolsCalled(t, h, b)
	checkGoldenTrace(t, h, dir)
	checkMintedIDsConsumed(t, mintedIDs)
	checkProviderIDsConsumed(t, providerIDs)
	checkToolCatalogConsulted(t, toolCatalog)
	checkMetaRepliesApplied(t, metaCanner)

	return h
}

// checkMetaRepliesApplied proves every canned meta reply was actually installed.
//
// A canned reply that never reached the assembly is INVISIBLE in every other
// assertion: the real tool runs, answers out of an empty fixture, and the step
// fails downstream naming the tool rather than the wiring that dropped it —
// the same shape as a tool-catalog check nobody consulted, and caught the same
// way.
//
// Skipped once the test has already failed: after a divergence the run stopped
// early, so a tool the session never assembled is a consequence of the first
// failure rather than a finding of its own.
func checkMetaRepliesApplied(t *testing.T, c *bt.MetaToolCanner) {
	t.Helper()
	if t.Failed() || c == nil {
		return
	}
	if unapplied := c.Unapplied(); len(unapplied) > 0 {
		t.Errorf("the bundle cans a reply for %v and this run never assembled %s, so the canning did "+
			"nothing and the real tool answered out of the fixture's own empty state — check that the "+
			"class still offers the capability contributing it, and the wiring from threadrun.Run "+
			"through e2e.Options.ReplaceAssembledTool",
			unapplied, pluralTool(len(unapplied)))
	}
}

// pluralTool renders "it"/"them" for the message above, so the sentence reads
// correctly for one canned tool and for several.
func pluralTool(n int) string {
	if n == 1 {
		return "it"
	}
	return "them"
}

// providerIDMap keys the bundle's provider-minted ids for a MintedIDSequence.
//
// Returns nil for a bundle that pinned none, which is what makes Minter hand
// back nil and leave the stand-in's own counter in place.
func providerIDMap(b bt.Bundle) map[string][]string {
	if b.StandIn == nil || len(b.StandIn.TriggerStatusIDs) == 0 {
		return nil
	}
	return map[string][]string{bt.FamilyTriggerStatus: b.StandIn.TriggerStatusIDs}
}

// standInMentions is the bundle's seeded channel directory, or nil.
func standInMentions(b bt.Bundle) *bt.MentionStandIn {
	if b.StandIn == nil {
		return nil
	}
	return b.StandIn.Mentions
}

// mentionLookups and mentionUsers convert the bundle's plain strings into the
// channel kind's own types. The bundle format holds strings so no consumer of it
// drags a channel-kind import along; the conversion happens at each end.
func mentionLookups(m *bt.MentionStandIn) []channelkinds.MentionLookupKind {
	out := make([]channelkinds.MentionLookupKind, 0, len(m.Lookups))
	for _, l := range m.Lookups {
		out = append(out, channelkinds.MentionLookupKind(l))
	}
	return out
}

func mentionUsers(m *bt.MentionStandIn) []fakekind.MentionUser {
	out := make([]fakekind.MentionUser, 0, len(m.Users))
	for _, u := range m.Users {
		out = append(out, fakekind.MentionUser{
			Kind:        channelkinds.MentionLookupKind(u.Kind),
			Value:       u.Value,
			ExternalID:  u.ExternalID,
			DisplayName: u.DisplayName,
		})
	}
	return out
}

// checkProviderIDsConsumed is the mirror of checkMintedIDsConsumed, one layer
// out: ids the bundle recorded the PROVIDER minting that the stand-in was never
// asked for.
//
// Leftovers here mean the run opened FEWER statuses than the captured session
// did — a claim that no longer happens, or a conclusion that found an existing
// run where the recording created one. Nothing else notices, because every step
// downstream still resolves.
//
// Skipped once the test has already failed, for the reason its sibling is:
// after a divergence the run stopped early, so leftovers are a consequence of
// the first failure rather than a finding of their own.
func checkProviderIDsConsumed(t *testing.T, seq *bt.MintedIDSequence) {
	t.Helper()
	if t.Failed() {
		return
	}
	for family, left := range seq.Unused() {
		t.Errorf("the run left %d recorded provider-minted %s id(s) unused (%v): the stand-in "+
			"provider was asked to mint FEWER than the captured session saw the real one mint — "+
			"a status the run no longer opens, or a conclusion that found an existing one where "+
			"the recording created it",
			len(left), family, left)
	}
}

// checkToolsCalled asserts every tool the bundle says the run DISPATCHED was
// dispatched again.
//
// # What this catches that the positional Expect does not
//
// Each step's Expect.LastToolResult already pins which tool the model was
// answering, and it is the stronger check wherever it applies. It does not
// apply everywhere: a result the capture could not pin every byte of leaves the
// expectation narrowed, and a call that stops happening then shifts the
// transcript rather than failing by name. Reading the replayed session's own
// transcript back is what makes the call itself the claim.
//
// It is also the MANDATORY companion to any reply this format ever cans in place
// of running real code — a canned reply is served whether or not the call was
// made the way the recording made it, so without this the bundle would prove
// nothing about that call. Nothing cans a meta tool's reply today.
//
// Set membership, not order or count: order is Expect's job, positionally and
// precisely, and a second ordered claim would report the same reordering twice.
func checkToolsCalled(t *testing.T, h *e2e.Harness, b bt.Bundle) {
	t.Helper()
	// Skipped once the test has already failed, for the reason the minted-id
	// checks are: after a divergence the run stopped early, so every tool it had
	// not reached yet is missing as a CONSEQUENCE of the first failure. Reporting
	// them would bury the cause under a list of names.
	if t.Failed() || len(b.Assert.ToolsCalled) == 0 {
		return
	}
	called := dispatchedToolNames(t, h)
	for _, want := range b.Assert.ToolsCalled {
		assert.Contains(t, called, want,
			"the bundle records this tool being dispatched and this run never dispatched it; the "+
				"replay reached the same words by a different route, which is the divergence a "+
				"narrowed step expectation cannot see")
	}
}

// dispatchedToolNames is every tool this run dispatched, read off the REPLAYED
// SESSION'S OWN TRANSCRIPT.
//
// The transcript rather than h.LLM.Requests(), and the difference is not
// cosmetic. A request only ever carries the tool results handed BACK to the
// model, so the LAST call of a run can be invisible there — agent_work_complete
// ends the loop, and its result need never reach another request. Reading
// requests reported the one tool every finished session ends on as never
// dispatched.
//
// It is also the same record the CAPTURE derived the claim from, which is what
// makes the two comparable: one side reads a recorded transcript, the other the
// replayed one, through the same kind and the same block type.
func dispatchedToolNames(t *testing.T, h *e2e.Harness) []string {
	t.Helper()
	ns, name := h.SessionRef()
	ctx := memory.WithSystemApproval(context.Background(), "threadrun")
	res, err := h.Memory().Query(ctx, memory.Query{
		Scope: memory.Scope{Kind: "session", ID: ns + "/" + name},
		Kinds: []string{"turn"},
	})
	require.NoError(t, err, "reading the replayed session's transcript")

	seen := map[string]bool{}
	var out []string
	for _, e := range res.Entries {
		var turn memory.Turn
		if json.Unmarshal(e.Content, &turn) != nil {
			// A turn this walk cannot decode says nothing either way, and a
			// tool named in it would be reported missing rather than found —
			// which is the safe direction for an assertion about what ran.
			continue
		}
		for _, blk := range turn.Content {
			if blk.Type != "tool_use" || blk.ToolUse == nil || blk.ToolUse.Name == "" {
				continue
			}
			if seen[blk.ToolUse.Name] {
				continue
			}
			seen[blk.ToolUse.Name] = true
			out = append(out, blk.ToolUse.Name)
		}
	}
	slices.Sort(out)
	return out
}

// checkToolCatalogConsulted proves the tool-catalog assertion RAN.
//
// A correct bundle produces no catalog findings, so a check the driver failed
// to wire — into e2e.Options, through the factory, onto the Loop — is
// indistinguishable from a check that found nothing, and every scenario would
// keep passing with the assertion silently gone. Cutting any link in that chain
// is exactly the mutation this catches; nothing else in the run does.
//
// Skipped once the test has already failed: after a divergence the run stopped
// early, so "never consulted" is a consequence of the first failure rather than
// a finding of its own.
func checkToolCatalogConsulted(t *testing.T, c *bt.ToolCatalogCheck) {
	t.Helper()
	if t.Failed() || c.Filter() == nil {
		return
	}
	if !c.Consulted() {
		t.Errorf("the bundle pins a tool catalog and the runner never consulted it: the replay ran with " +
			"whatever tool set its fixture composed, and every catalog assertion in this bundle asserted " +
			"nothing — check the wiring from threadrun.Run through e2e.Options.FilterOfferedTools to " +
			"runner.Loop.ReplayToolCatalog")
	}
}

// checkMintedIDsConsumed is the second half of the minted-id assertion.
//
// Exhaustion — the run minting MORE ids than the capture recorded — reports
// itself from inside the minter, at the moment it happens. This is its mirror:
// ids left over mean the run minted FEWER, most often because a plan opened
// fewer operations than the captured one did. Nothing else notices that, because
// every recorded argument still resolves.
//
// Skipped once the test has already failed. After a divergence the run stops
// early, so leftovers are a CONSEQUENCE of the first failure rather than a
// finding of their own, and reporting them would bury the cause.
func checkMintedIDsConsumed(t *testing.T, seq *bt.MintedIDSequence) {
	t.Helper()
	if t.Failed() {
		return
	}
	for family, left := range seq.Unused() {
		t.Errorf("the run left %d recorded %s id(s) unused (%v): it minted FEWER %s ids than the "+
			"captured session did — a plan that opened fewer operations, or a step that no longer runs",
			len(left), family, left, family)
	}
}

// waitForComposedSlotSchema derives the (resourceType, slot_grant_<perm>)
// pairs the tested AgentClass's authz.slots declare, and blocks until
// SpiceDB's composed schema has all of them.
//
// Reads slots straight off the live AgentClass rather than the bundle JSON:
// the AgentClass is the thing guardian actually composes from (via the
// AgentSessionGrants CR the AgentClass controller derives from it — see
// pkg/controllers/agentclass/grants.go extractSlotPairs), so deriving the
// wait from the same source keeps it correct even if a future bundle's
// AgentClass declares slots the bundle.json's plan never mentions.
// authz.SlotGrantRelationName is the same helper the composer and the
// approval-time binder use, so the wait cannot name the relation differently
// than the code actually writing it.
func waitForComposedSlotSchema(t *testing.T, h *e2e.Harness, b bt.Bundle) {
	t.Helper()
	var cls spiceboxv1alpha1.AgentClass
	require.NoError(t, h.K8s.Get(context.Background(),
		client.ObjectKey{Namespace: h.Namespace(), Name: b.AgentClass}, &cls),
		"get AgentClass %q to derive its declared authz.slots", b.AgentClass)

	var want []e2e.SchemaRel
	for _, s := range cls.Spec.GetSlots() {
		if s.ResourceType == "" || s.Permission == "" {
			continue
		}
		want = append(want, e2e.SchemaRel{
			Definition: s.ResourceType,
			Relation:   authz.SlotGrantRelationName(s.Permission),
		})
	}
	h.WaitForComposedSchema(30*time.Second, want)
}

// checkGoldenTrace compares the run's authorization trace against a frozen
// golden, when the scenario has one.
//
// This is the promotion path the design names: once a bundle is verified by its
// property assertions, its emitted trace can be frozen, and from then on the
// scenario also catches changes nobody thought to assert. Property assertions
// say what the author claimed; a golden says everything that happened.
//
// Opt-in per bundle, by the file simply existing. A bundle without one keeps
// bronzethread's original mode exactly — which matters because a golden is only
// worth having once the property assertions have established the scenario is
// RIGHT. Freezing a trace first would pin whatever the code did on the day it
// was written, bug included.
//
// Regenerate with BRONZE_UPDATE_GOLDEN=1. Review that diff like any other: a
// golden that gets regenerated without being read is a test that asserts
// nothing, and it is the standard way this kind of harness rots.
func checkGoldenTrace(t *testing.T, h *e2e.Harness, dir string) {
	t.Helper()

	ns, name := h.SessionRef()
	ctx := memory.WithSystemApproval(context.Background(), "bronzethread")

	gate, err := plangateaudit.List(ctx, h.MemStore(), memory.Scope{Kind: "session", ID: ns + "/" + name})
	require.NoError(t, err, "reading the plan-gate log for the trace")

	res, err := h.Memory().Query(ctx, memory.Query{
		Scope: memory.Scope{Kind: "session", ID: ns + "/" + name},
		Kinds: []string{"authz_decision"},
		// Oldest-first, the same order checkAuthz asks for. TraceLines sorts
		// and dedupes, so the golden itself does not depend on it — but two
		// reads of one log asking for different orders is how the next person
		// copying this query inherits the bug checkAuthz had.
		OrderBy: memory.OrderBy{Field: "createdAt"},
	})
	require.NoError(t, err, "reading the authz_decision log for the trace")
	decisions := make([]authzdecision.Decision, 0, len(res.Entries))
	for _, e := range res.Entries {
		var d authzdecision.Decision
		require.NoError(t, json.Unmarshal(e.Content, &d), "decoding authz_decision %s", e.ID)
		decisions = append(decisions, d)
	}

	// The flow-control half. Without it a scenario about per-datum provenance
	// — a tag minted, a slot bound, a delegation refused for combining
	// untrusted input with the ability to act — produces an EMPTY golden,
	// because none of that reaches the plan-gate log or authz_decision.
	scope := memory.Scope{Kind: "session", ID: ns + "/" + name}
	flowAudit, aerr := infoleakageaudit.List(ctx, h.MemStore(), scope)
	require.NoError(t, aerr, "reading the info-leakage/trifecta audit log for the trace")
	flowTags, terr := pttag.List(ctx, h.MemStore(), scope)
	require.NoError(t, terr, "reading the pt-tag log for the trace")
	flow := bt.TraceFlow{Audit: flowAudit, Tags: flowTags}

	got := strings.Join(bt.TraceLinesWithFlow(gate, decisions, flow), "\n") + "\n"
	path := filepath.Join(dir, "trace.golden")

	if os.Getenv("BRONZE_UPDATE_GOLDEN") != "" {
		require.NoError(t, os.WriteFile(path, []byte(got), 0o644))
		t.Logf("wrote golden trace %s (%d gate, %d authz, %d flow, %d tags) — READ THE DIFF",
			path, len(gate), len(decisions), len(flowAudit), len(flowTags))
		return
	}

	want, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return // not promoted; property assertions only
	}
	require.NoError(t, err, "reading %s", path)

	assert.Equal(t, string(want), got,
		"the authorization trace changed. If the new behavior is correct, "+
			"regenerate with BRONZE_UPDATE_GOLDEN=1 and review the diff; if it is not, "+
			"this is the regression the golden exists to catch")
}

// canonUser turns a bundle-authored email into the canonical subject ID
// SpiceDB actually stores, so a bundle names people the way its own fixtures
// do rather than carrying base64 blobs.
func canonUser(t *testing.T, email string) string {
	t.Helper()
	c, err := identity.EmailReference(identity.Email(email)).Canonical()
	require.NoError(t, err, "canonicalizing %q", email)
	return c.String()
}

// checkPtTags evaluates the per-datum provenance assertions.
//
// Reader membership is resolved through SpiceDB rather than read off the
// record's DirectReaders. The record is the durable account of a mint; the
// `reader` permission is what an actual disclosure decision consults, and
// those two agree only if the minter wrote BOTH. Asserting on the record
// alone would pass on a tag whose tuples never landed — a tag that reads
// correctly and enforces nothing.
func checkPtTags(t *testing.T, h *e2e.Harness, b bt.Bundle) {
	t.Helper()
	pt := b.Assert.PtTags
	if pt == nil {
		return
	}
	require.NotNil(t, h.SpiceDB,
		"a pt-tag assertion needs SpiceDB; the reader set is a permission, not a field")

	ns, name := h.SessionRef()
	ctx := memory.WithSystemApproval(context.Background(), "bronzethread")
	tags, err := pttag.List(ctx, h.MemStore(), memory.Scope{Kind: "session", ID: ns + "/" + name})
	require.NoError(t, err)

	if pt.MintedNone {
		assert.Empty(t, tags, "no tag should have been minted")
		return
	}
	require.NotEmpty(t, tags,
		"nothing was minted; the mint path may not be reached at all")
	if pt.Minted > 0 {
		assert.Len(t, tags, pt.Minted, "one tag per declared read, no more")
	}

	var untrustedTags int
	for _, tag := range tags {
		// One integrity lookup per tag, feeding both assertions: the
		// per-tag claim and the count.
		if pt.CarriesUntrusted != nil || pt.UntrustedCount != nil {
			carries, uerr := h.SpiceDB.TagCarriesUntrusted(ctx, tag.ID)
			require.NoError(t, uerr, "resolving integrity of %s", tag.ID)
			if carries {
				untrustedTags++
			}
			if pt.CarriesUntrusted != nil {
				assert.Equal(t, *pt.CarriesUntrusted, carries,
					"carries_untrusted of %s", tag.ID)
			}
		}
		if len(pt.ReadersInclude) > 0 || len(pt.ReadersExclude) > 0 {
			readers, rerr := h.SpiceDB.TagReaders(ctx, tag.ID)
			require.NoError(t, rerr, "resolving readers of %s", tag.ID)
			for _, want := range pt.ReadersInclude {
				assert.Contains(t, readers, canonUser(t, want),
					"%s must be a reader of %s", want, tag.ID)
			}
			for _, notWant := range pt.ReadersExclude {
				assert.NotContains(t, readers, canonUser(t, notWant),
					"%s must NOT be a reader of %s — the source never disclosed to them",
					notWant, tag.ID)
			}
		}
	}
	if pt.UntrustedCount != nil {
		assert.Equal(t, *pt.UntrustedCount, untrustedTags,
			"exactly this many of the %d minted tags must carry an untrusted origin; "+
				"a run where every tag answers the same way cannot tell a read signal from a constant",
			len(tags))
	}
}

// checkAuthz evaluates the authz assertions against the recorded decision log.
//
// Decisions are compared in RECORD ORDER so "denied, then allowed after
// approval" is distinguishable from "allowed outright" — the difference between
// an approval gate that fired and one that was never reached.
//
// That order has to be ASKED FOR. The store only sorts when the query says so
// (or a Limit is about to throw entries away); without an OrderBy the entries
// come back in Go map order, randomized per call, and the last-write-wins read
// below picks an arbitrary one of a key's outcomes. It never fired only because
// no authored bundle asserts a key that was decided twice — and a CAPTURE of a
// real denied-then-approved session is exactly that bundle.
func checkAuthz(t *testing.T, h *e2e.Harness, b bt.Bundle) {
	t.Helper()
	az := b.Assert.Authz
	if az == nil {
		return
	}

	ns, name := h.SessionRef()
	ctx := memory.WithSystemApproval(context.Background(), "bronzethread")
	res, err := h.Memory().Query(ctx, memory.Query{
		Scope: memory.Scope{Kind: "session", ID: ns + "/" + name},
		Kinds: []string{"authz_decision"},
		// Oldest-first (Desc defaults false), so the LAST element of a key's
		// outcome slice is genuinely the final decision.
		OrderBy: memory.OrderBy{Field: "createdAt"},
	})
	require.NoError(t, err, "reading the authz_decision log")

	// key -> outcomes, in the order they were recorded. Messages are kept
	// alongside so a failure reports WHY the decision went the way it did —
	// "denied" on its own sends the reader back to the logs.
	got := map[string][]string{}
	why := map[string][]string{}
	for _, e := range res.Entries {
		var d authzdecision.Decision
		require.NoError(t, json.Unmarshal(e.Content, &d), "decoding authz_decision %s", e.ID)
		k := d.ResourceType + ":" + d.ResourceID + "#" + d.Permission
		got[k] = append(got[k], d.Outcome)
		if d.Message != "" {
			why[k] = append(why[k], d.Message)
		}
	}

	for key, want := range az.Decisions {
		outcomes := got[key]
		require.NotEmpty(t, outcomes,
			"no authz decision recorded for %q; the Check may not have been reached at all "+
				"(recorded: %v)", key, keysOf(got))
		assert.Equal(t, want, outcomes[len(outcomes)-1],
			"final outcome for %q (sequence: %v; messages: %v)", key, outcomes, why[key])
	}

	for _, key := range az.NeverAllowed {
		assert.NotContains(t, got[key], "allowed",
			"%q was allowed at some point; the permission boundary did not hold "+
				"(sequence: %v)", key, got[key])
	}
}

func keysOf(m map[string][]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// seedRelationships writes each bundle-declared SpiceDB relationship before
// the run starts. See bt.SeedRelationship's doc for the subject convention:
// a "user:" subject is named by RAW EMAIL and canonicalized here exactly as
// checkSpiceDB canonicalizes assert.spicedb's own "user:" subjects (via
// canonUser below) — the harness's one, sole convention for naming a person
// in a bundle, reused rather than re-invented for this second seeding path.
//
// github_user#sole_user is special-cased onto *spicedb.Client's own typed
// helper (TouchSoleIdentity) rather than the generic e2e.WriteRel: it is one
// of the four relations spicedb.TypedWritesSource CLAIMS specifically so no
// relsource-guarded writer — e2eharness (WriteRel's own source) included —
// may touch it; TouchSoleIdentity bypasses the guard by calling the
// underlying client directly, which is the ONLY legitimate way to write it
// (the same call the useridentity reconciler itself makes). Found by running
// this bundle against this exact seam: WriteRel's first attempt failed with
// "relsource: e2eharness cannot write github_user#sole_user: owned by
// spicedbtypedwrites: relsource: refused".
func seedRelationships(t *testing.T, h *e2e.Harness, b bt.Bundle) {
	t.Helper()
	for i, r := range b.SeedRelationships {
		rt, rid, ok := strings.Cut(r.Resource, ":")
		require.True(t, ok, "seedRelationships[%d].resource %q must be <type>:<id>", i, r.Resource)
		st, sid, ok := strings.Cut(r.Subject, ":")
		require.True(t, ok, "seedRelationships[%d].subject %q must be <type>:<id>", i, r.Subject)

		if rt == "github_user" && r.Relation == "sole_user" {
			require.Equal(t, "user", st, "seedRelationships[%d]: github_user#sole_user's subject must be a user", i)
			canon, err := identity.EmailReference(identity.Email(sid)).Canonical()
			require.NoError(t, err, "seedRelationships[%d]: canonicalizing %q", i, sid)
			require.NoError(t, h.SpiceDB.TouchSoleIdentity(context.Background(), rt, rid, canon),
				"seedRelationships[%d]: TouchSoleIdentity %s:%s#sole_user@user:%s", i, rt, rid, canon.String())
			continue
		}

		if st == "user" {
			sid = canonUser(t, sid)
		}
		e2e.WriteRel(t, h, rt, rid, r.Relation, st, sid, "")
	}
}

// seedUserPreferences writes each bundle-declared saved preference value
// directly into the named subject's own memory scope, before the run starts
// — bypassing set_preference's confirm round-trip entirely, which an
// authored bundle has no way to play (see bt.Bundle.SeedUserPreferences).
//
// Written through h.MemStore() under memory.SystemContext, the same door
// pkg/memory/httpsrv/preferences.go's own commit route writes the
// user_preference Kind through — user_preference is ComponentWritten, so a
// token-session context would be refused here exactly as it is in
// production; SystemContext is what makes this the platform writing on the
// fixture's behalf, not a session bearer bypassing the confirm.
func seedUserPreferences(t *testing.T, h *e2e.Harness, b bt.Bundle) {
	t.Helper()
	for i, s := range b.SeedUserPreferences {
		subject := s.Subject
		if addr, ok := strings.CutPrefix(subject, "email:"); ok {
			// The SAME primitive subjectresolve's own email resolver
			// canonicalizes through (identity.EmailReference(...).Canonical()) —
			// reused, not reimplemented, so a bundle naming "email:<addr>" here
			// and a get_preferences user-ref of the same form land on the
			// identical canonical id.
			c, err := identity.EmailReference(identity.Email(addr)).Canonical()
			require.NoError(t, err, "seedUserPreferences[%d]: canonicalizing %q", i, subject)
			subject = c.String()
		}
		scope, err := memory.UserScope(subject)
		require.NoError(t, err, "seedUserPreferences[%d]: user scope for %q", i, subject)

		content, err := json.Marshal(userpreference.Preference{
			ClassNamespace: h.Namespace(),
			ClassName:      b.AgentClass,
			Key:            s.Key,
			Value:          s.Value,
		})
		require.NoError(t, err, "seedUserPreferences[%d]: encode preference", i)

		ctx := memory.SystemContext(context.Background(), "bronzethread-seed")
		_, err = h.MemStore().Put(ctx, memory.Entry{
			Scope:   scope,
			Kind:    userpreference.KindName,
			ID:      userpreference.EntryID(h.Namespace(), b.AgentClass, s.Key),
			Content: content,
		})
		require.NoError(t, err, "seedUserPreferences[%d]: put", i)
	}
}

// checkSpiceDB issues a fully-consistent CheckPermission per assert.spicedb
// entry, straight against the harness's real SpiceDB. Unlike checkAuthz — which
// can only see permissions some dispatched call happened to Check — this reaches
// a relationship the transcript never exercised, which is the whole point: a
// grant written but never spent, or one that leaked across sessions, is
// invisible to the decision log and visible here.
func checkSpiceDB(t *testing.T, h *e2e.Harness, b bt.Bundle) {
	t.Helper()
	for _, c := range b.Assert.SpiceDB {
		subj := c.Subject
		// A `user:` subject is named by RAW email and canonicalized to the escaped
		// on-the-wire id SpiceDB actually stores (`@`/`.` are illegal in an object
		// id), exactly as the pt-tag reader assertions do via canonUser. Every
		// other subject type is passed through verbatim — its id is already on the
		// wire, the same as the resource id.
		if st, sid, ok := strings.Cut(subj, ":"); ok && st == "user" {
			// canonUser returns the base64 canonical id alone (no type prefix), so
			// re-attach "user:" to form the "<type>:<id>" AssertSpiceDBRaw wants.
			subj = "user:" + canonUser(t, sid)
		}
		h.AssertSpiceDBRaw(c.Resource, c.Permission, subj, c.Want)
	}
}

// registerTranscript turns the positional LLM list into ordered, single-use
// rules, and returns the record of which steps were actually reached.
//
// ScriptedLLM matches rules in registration order and consumes a non-repeating
// rule once, so N catch-all rules ARE a positional transcript — no harness
// change needed. Each rule's matcher doubles as the divergence check: if the
// nth request does not look like what the nth step expects, the test fails
// there rather than answering a question the system did not ask.
//
// One shared stream is right for one session and wrong for two. A step naming
// an agentClass is answered only by requests composed for THAT class's session
// (LLMStep.AgentClass), so a conversational delegation replays as two
// independent positional streams — the parent's and the child's — that cannot
// consume each other's rules. Which side a request came from is read off the
// request itself, never inferred from arrival order, because arrival order is
// the thing under test: a delegation that never conversed would otherwise let
// the parent walk the child's steps and pass.
func registerTranscript(t *testing.T, h *e2e.Harness, b bt.Bundle) *transcript {
	t.Helper()
	// One scoped stream per session named by the transcript, resolved before any
	// rule is registered so an unresolvable scope fails at setup rather than
	// mid-run on the runner's own goroutine.
	scopes := resolveTranscriptScopes(t, h, b)

	// Values a later step interpolates into its tool args, bound by an earlier
	// step's Expect.Capture. Written and read only from inside a matcher or a
	// ReplyFn, both of which ScriptedLLM.Send calls while holding its own mutex,
	// so the map needs no lock of its own. tr.mu guards only the crossing into
	// the test goroutine, which is what checkTranscriptConsumed does.
	vars := map[string]string{}
	tr := &transcript{consumed: make([]bool, len(b.LLM))}

	for i, step := range b.LLM {
		i, step := i, step
		// Parsed and compiled up front, on the test goroutine: a malformed
		// args blob or capture pattern is an authoring error and must fail the
		// test where require can, not from the matcher.
		parts := parseReplyParts(t, i, step.Reply)
		captures := compileCaptures(t, i, step.Expect.Capture)
		scope := scopes[step.AgentClass]
		h.LLM.On(func(req llm.Request) bool {
			// Scope FIRST: an unmatched scope must leave the step pending for
			// its own session, so a request from the other side falls through
			// to that side's next rule rather than consuming this one — and
			// checkExpect must not report on a request this step never claimed.
			if scope != "" && !strings.Contains(systemPromptOf(req), scope) {
				return false
			}
			checkExpect(t, i, step.Expect, req)
			applyCaptures(t, i, captures, req, vars)
			tr.markConsumed(i)
			return true
		}).ReplyFn(func() []e2e.ReplyPart { return renderReplyParts(t, i, parts, vars) })
	}

	// One trailing rule for the loop's own epilogue.
	//
	// After agent_work_complete the runner asks the model once more, handing it
	// a "session complete" tool result — it is checking for further work, not
	// continuing the conversation. That call belongs to the loop, not to the
	// authored transcript, and in a MIDDLE turn it is invisible because the next
	// turn's step absorbs it. On the LAST turn there is no next step, so whether
	// it lands before or after the assertions decides the run: bundles passed
	// alone and failed in company, with "no rule matched … rules pending: 0".
	//
	// Repeating and last, so every authored step is still matched first and in
	// order — this only catches what the bundle did not describe.
	h.LLM.On(func(llm.Request) bool { return true }).Reply(e2e.EndTurn()).Repeating()
	return tr
}

// transcript records which steps the run actually reached.
type transcript struct {
	mu       sync.Mutex
	consumed []bool
}

func (tr *transcript) markConsumed(i int) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.consumed[i] = true
}

// checkTranscriptConsumed asserts every SCOPED step was reached.
//
// Only the scoped ones, and that is the whole point of the check: an unscoped
// bundle may legitimately over-script (a branch it did not take, a trailing
// step the loop never needed), and demanding full consumption of those would
// fail scenarios that are correct today. A step that names a session is a claim
// that THAT session took THAT turn, and a claim nobody checked is how a
// delegation that never conversed produces a green run — the parent's own
// assertions can only see what its tool calls returned.
func checkTranscriptConsumed(t *testing.T, b bt.Bundle, tr *transcript) {
	t.Helper()
	tr.mu.Lock()
	defer tr.mu.Unlock()
	for i, step := range b.LLM {
		if step.AgentClass == "" {
			continue
		}
		assert.True(t, tr.consumed[i],
			"llm step %d was scoped to %q and never ran: that session did not take the turn the transcript says it did",
			i, step.AgentClass)
	}
}

// resolveTranscriptScopes maps each agentClass the transcript names to a string
// that appears in the composed system prompt of exactly that class's sessions.
//
// The discriminator is the class's own spec.systemPrompt.inline, read off the
// live AgentClass and matched verbatim: runner.ComposeSystem writes it into the
// composed prompt unchanged, and the runner sends that prompt as the request's
// system block. So the scope is derived from the same object the runner
// composed from, rather than transcribed into the bundle where the two could
// drift.
//
// Both failure modes are fatal rather than best-effort. A class with no inline
// prompt offers nothing to tell its requests apart, and two classes whose
// prompts contain one another would silently route one session's turns to the
// other's rules — which is precisely the confusion scoping exists to remove.
func resolveTranscriptScopes(t *testing.T, h *e2e.Harness, b bt.Bundle) map[string]string {
	t.Helper()
	scopes := map[string]string{}
	for _, step := range b.LLM {
		if step.AgentClass == "" || scopes[step.AgentClass] != "" {
			continue
		}
		var cls spiceboxv1alpha1.AgentClass
		require.NoError(t, h.K8s.Get(context.Background(),
			client.ObjectKey{Namespace: h.Namespace(), Name: step.AgentClass}, &cls),
			"get AgentClass %q to scope its transcript steps", step.AgentClass)
		inline := strings.TrimSpace(cls.Spec.SystemPrompt.Inline)
		require.NotEmpty(t, inline,
			"AgentClass %q declares no inline system prompt, so its requests cannot be told "+
				"from another session's; give it one before scoping steps to it", step.AgentClass)
		scopes[step.AgentClass] = inline
	}
	for outer, outerScope := range scopes {
		for inner, innerScope := range scopes {
			if outer == inner {
				continue
			}
			require.NotContains(t, outerScope, innerScope,
				"AgentClass %q's inline system prompt contains %q's, so a request for %q would "+
					"match both scopes; make the two prompts distinguishable", outer, inner, outer)
		}
	}
	return scopes
}

// systemPromptOf joins the request's system blocks. The runner sends one block,
// but joining is what keeps the scope check correct if that ever becomes
// several.
func systemPromptOf(req llm.Request) string {
	var sb strings.Builder
	for _, blk := range req.System {
		sb.WriteString(blk.Text)
		sb.WriteString("\n")
	}
	return sb.String()
}

// compileCaptures turns one step's Expect.Capture into compiled patterns,
// rejecting anything that could not bind a single value.
func compileCaptures(t *testing.T, idx int, in map[string]string) map[string]*regexp.Regexp {
	t.Helper()
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]*regexp.Regexp, len(in))
	for name, pattern := range in {
		re, err := regexp.Compile(pattern)
		require.NoError(t, err, "llm step %d: capture %q pattern", idx, name)
		require.Equal(t, 1, re.NumSubexp(),
			"llm step %d: capture %q must have exactly ONE capture group; it is what gets bound", idx, name)
		out[name] = re
	}
	return out
}

// applyCaptures binds this step's captures from the tool result it is
// answering, for later steps to interpolate.
//
// Runs on the runner's goroutine (inside the matcher), so it reports with
// assert rather than require — and it binds nothing on a miss, which leaves
// renderReplyParts to fail on the unresolved placeholder rather than
// substituting an empty string that would look like a well-formed call.
func applyCaptures(t *testing.T, idx int, captures map[string]*regexp.Regexp, req llm.Request, vars map[string]string) {
	t.Helper()
	if len(captures) == 0 {
		return
	}

	// A capture normally reads the tool result this step is answering. On the
	// FIRST step of a turn there is none, and the value a transcript cannot
	// know may still have arrived — an inbound-attachment manifest line names a
	// handle the cluster minted moments earlier, in the user text.
	//
	// Falling back to that text keeps the rule that matters intact: the value
	// comes from the RUN, never from the bundle. What it must not become is a
	// way to read a handle the agent was never shown, so the source is the same
	// text the model is looking at, and nothing wider.
	source, from := "", ""
	if blk := lastToolResultBlock(req); blk != nil {
		source, from = blk.Content, "tool result"
	} else if txt := lastUserText(req); txt != "" {
		source, from = txt, "user text"
	}
	if !assert.NotEmpty(t, source,
		"llm step %d: capture needs a tool result or user text to read from", idx) {
		return
	}

	for name, re := range captures {
		m := re.FindStringSubmatch(source)
		if !assert.Len(t, m, 2,
			"llm step %d: capture %q found nothing in the %s (content: %q)", idx, name, from, source) {
			continue
		}
		vars[name] = m[1]
	}
}

// parsedPart is one reply part with its args decoded, ready to be rendered
// against the captured values once they exist.
type parsedPart struct {
	part bt.ReplyPart
	args map[string]any
}

// parseReplyParts decodes each part's args up front so a malformed blob fails
// at authoring time, on the test goroutine.
func parseReplyParts(t *testing.T, idx int, in []bt.ReplyPart) []parsedPart {
	t.Helper()
	out := make([]parsedPart, 0, len(in))
	for _, p := range in {
		pp := parsedPart{part: p}
		if p.ToolUse != nil {
			pp.args = map[string]any{}
			if len(p.ToolUse.Args) > 0 {
				require.NoError(t, json.Unmarshal(p.ToolUse.Args, &pp.args),
					"llm step %d: toolUse %q args", idx, p.ToolUse.Name)
			}
		}
		out = append(out, pp)
	}
	return out
}

// renderReplyParts builds the harness reply parts, interpolating {{name}} into
// tool args from the values earlier steps captured.
//
// Rendering is the ONLY half that happens at match time, and that split is
// deliberate. A reply is PARSED at registration (parseReplyParts), on the test
// goroutine, where a malformed args blob is an authoring error require can
// still fail the test for; it is RENDERED here, because a capture binds its
// value out of the run and there is nothing to substitute until the matcher
// has seen the result. Args are parsed once and reused on every match, so the
// substitution runs on the FRESH map substituteArgs returns rather than on the
// parsed part itself, which would leak one match's values into the next.
//
// Minted ids need no such treatment and are deliberately absent: the run mints
// exactly what the bundle recorded, in order (bt.MintedIDSequence), so a
// recorded argument naming one is already correct as authored.
//
// Called at match time, so it reports with assert. An unresolved placeholder is
// a failure and the literal is left in place: sending the call anyway with the
// placeholder still in it is refused loudly by the tool, where blanking it
// would produce a plausible-looking refusal that hides the broken transcript.
func renderReplyParts(t *testing.T, idx int, in []parsedPart, vars map[string]string) []e2e.ReplyPart {
	t.Helper()
	out := make([]e2e.ReplyPart, 0, len(in))
	for _, pp := range in {
		switch {
		case pp.part.EndTurn:
			out = append(out, e2e.EndTurn())
		case pp.part.ToolUse != nil:
			out = append(out, e2e.ToolUse(pp.part.ToolUse.Name, substituteArgs(t, idx, pp.args, vars)))
		case pp.part.BareText != "":
			out = append(out, e2e.Text(pp.part.BareText))
		case pp.part.Text != "":
			out = append(out, e2e.RespondToUser(pp.part.Text))
		}
	}
	return out
}

// placeholderRe matches a {{name}} interpolation in a tool argument.
var placeholderRe = regexp.MustCompile(`\{\{([A-Za-z0-9_]+)\}\}`)

// substituteArgs walks the decoded args and interpolates captured values into
// every string it finds, at any depth.
func substituteArgs(t *testing.T, idx int, args map[string]any, vars map[string]string) map[string]any {
	t.Helper()
	var walk func(v any) any
	walk = func(v any) any {
		switch x := v.(type) {
		case string:
			return placeholderRe.ReplaceAllStringFunc(x, func(m string) string {
				name := placeholderRe.FindStringSubmatch(m)[1]
				val, ok := vars[name]
				if !assert.True(t, ok,
					"llm step %d: %s is not bound; no earlier step captured it", idx, m) {
					return m
				}
				return val
			})
		case map[string]any:
			m := make(map[string]any, len(x))
			for k, e := range x {
				m[k] = walk(e)
			}
			return m
		case []any:
			s := make([]any, len(x))
			for i, e := range x {
				s[i] = walk(e)
			}
			return s
		default:
			return v
		}
	}
	out := map[string]any{}
	for k, v := range args {
		out[k] = walk(v)
	}
	return out
}

// checkExpect enforces the divergence contract for one step.
//
// A failure here means the system took a different path than the transcript
// describes. For an authored bundle that is a bug in the bundle or in the code;
// for a captured one it is exactly the regression signal steelthread exists to
// produce. Either way, continuing would test nothing.
func checkExpect(t *testing.T, idx int, e bt.Expect, req llm.Request) {
	t.Helper()
	if e.UserTextContains != "" {
		assert.Contains(t, lastUserText(req), e.UserTextContains,
			"llm step %d: request diverged from the transcript", idx)
	}
	if e.LastToolResult != "" {
		assert.Equal(t, e.LastToolResult, lastToolResultName(req),
			"llm step %d: expected to be answering %q", idx, e.LastToolResult)
	}
	if e.LastToolResultContains != "" || e.LastToolResultNotContains != "" || e.LastToolResultIsError != nil {
		blk := lastToolResultBlock(req)
		if assert.NotNil(t, blk, "llm step %d: no tool result to inspect", idx) {
			if e.LastToolResultContains != "" {
				assert.Contains(t, blk.Content, e.LastToolResultContains,
					"llm step %d: the model was handed a different tool result than the transcript describes", idx)
			}
			if e.LastToolResultNotContains != "" {
				assert.NotContains(t, blk.Content, e.LastToolResultNotContains,
					"llm step %d: refused data reached the model anyway", idx)
			}
			if e.LastToolResultIsError != nil {
				assert.Equal(t, *e.LastToolResultIsError, blk.IsError,
					"llm step %d: tool result isError (content: %q)", idx, blk.Content)
			}
		}
	}
	if len(e.ToolResultCounts) > 0 {
		assert.True(t, toolResultCountsMatch(req, e.ToolResultCounts),
			"llm step %d: exact result counts: want=%v got=%v", idx,
			e.ToolResultCounts, latestToolResultCounts(req))
	}
	if e.ToolOffered != "" {
		assert.True(t, hasTool(req, e.ToolOffered),
			"llm step %d: tool %q must be offered", idx, e.ToolOffered)
	}
	if e.ToolNotOffered != "" {
		assert.False(t, hasTool(req, e.ToolNotOffered),
			"llm step %d: tool %q must NOT be offered", idx, e.ToolNotOffered)
	}
}

// checkApprovalPromptCount asserts the exact number of interaction approval
// prompts the run published, category-agnostic (Assertions.ApprovalPrompts).
// Runs after checkSessionPhase on purpose: for a replyless bundle the phase
// barrier is what settles the count before it is read.
func checkApprovalPromptCount(t *testing.T, h *e2e.Harness, b bt.Bundle) {
	t.Helper()
	if b.Assert.ApprovalPrompts == nil {
		return
	}
	assert.Len(t, h.PublishedApprovalPrompts(), *b.Assert.ApprovalPrompts,
		"published interaction approval prompts")
}

// checkPlanGate evaluates the plan-gate assertions against the recorded log.
func checkPlanGate(t *testing.T, h *e2e.Harness, b bt.Bundle) {
	t.Helper()
	pg := b.Assert.PlanGate
	if pg == nil {
		return
	}

	ns, name := h.SessionRef()
	ctx := memory.WithSystemApproval(context.Background(), "bronzethread")
	recs, err := plangateaudit.List(ctx, h.MemStore(), memory.Scope{Kind: "session", ID: ns + "/" + name})
	require.NoError(t, err)

	if pg.NoRecords {
		assert.Empty(t, recs, "the gate must have written nothing")
		return
	}
	require.NotEmpty(t, recs, "the gate recorded nothing; it may not be reached at all")

	var approvals []plangateaudit.Content
	for _, r := range recs {
		if r.Event == plangateaudit.EventPlanApproved {
			approvals = append(approvals, r)
		}
		if pg.PublishedNothing {
			assert.Equal(t, "logging", r.Mode,
				"a logging-mode scenario must not record an enforcing decision")
		}
	}

	if pg.FrozenPhases > 0 {
		require.Len(t, approvals, pg.FrozenPhases, "declared phases must all freeze")
	}
	if pg.OnePlanDigest && len(approvals) > 1 {
		for _, a := range approvals[1:] {
			assert.Equal(t, approvals[0].PlanDigest, a.PlanDigest,
				"every frozen phase must belong to ONE plan")
		}
	}

	var rebuiltDigest string
	if pg.RebuildableFromLog || pg.GatedAgainstFrozenPlan {
		rebuilt, ok := plangate.PlanFromRecords(recs)
		require.True(t, ok, "the frozen plan must rebuild from the log alone")
		rebuiltDigest = rebuilt.Digest()
		if pg.RebuildableFromLog {
			require.NotEmpty(t, approvals)
			assert.Equal(t, approvals[0].PlanDigest, rebuiltDigest,
				"a plan rebuilt from the log must BE the recorded plan, or every fold discards it")
		}
	}

	if pg.GatedAgainstFrozenPlan {
		var gated bool
		for _, r := range recs {
			if r.Handle == "" || r.Event == plangateaudit.EventPlanApproved {
				continue
			}
			gated = true
			assert.Equal(t, rebuiltDigest, r.PlanDigest,
				"call to %q was gated against a stale plan, not the frozen one", r.Tool)
		}
		assert.True(t, gated, "no permissioned call reached the gate")
	}

	for toolName, want := range pg.Outcomes {
		var seen bool
		for _, r := range recs {
			if r.Tool != toolName || r.Handle == "" {
				continue
			}
			seen = true
			assert.Equal(t, want, r.Outcome, "tool %q outcome", toolName)
		}
		assert.True(t, seen, "no gated record for tool %q", toolName)
	}

	if pg.PublishedNothing {
		assert.Empty(t, h.PublishedApprovalPrompts(), "logging must publish no approval")
	}

	if pg.ApprovalPrompts > 0 {
		assert.Len(t, h.PublishedApprovalPrompts(), pg.ApprovalPrompts,
			"a cleared phase must be asked about ONCE; a second card means the "+
				"decision never reached the log, so the fold re-asks per call")
	}

	assertCardWhat(t, h, pg)
}

// assertCardWhat checks the COMPUTED half of every published approval card.
//
// It reads the rendered field rather than the gate's audit record on purpose.
// The card was built and recorded correctly for a long time while the published
// prompt carried only its lead sentence — so a scenario asserting the record
// would have been green the whole time the human was seeing nothing.
func assertCardWhat(t *testing.T, h *e2e.Harness, pg *bt.PlanGateAssertions) {
	t.Helper()
	if len(pg.CardWhatContains) == 0 && len(pg.CardWhatOmits) == 0 {
		return
	}

	var whats []string
	var structured []string // the same fields' Items, flattened
	for _, p := range h.PublishedApprovalPrompts() {
		for _, f := range p.Payload.Fields {
			if f.Label != "What" {
				continue
			}
			whats = append(whats, f.Value)
			if len(f.Items) > 0 {
				structured = append(structured, flattenItems(f.Items))
			}
		}
	}
	require.NotEmpty(t, whats,
		"no published card carried a What field; the approver decided on the lead alone")

	joined := strings.Join(whats, "\n")
	for _, want := range pg.CardWhatContains {
		assert.Contains(t, joined, want, "no card's What described this")
	}
	for _, bad := range pg.CardWhatOmits {
		assert.NotContains(t, joined, bad,
			"the computed half claimed something it must not — either a resource "+
				"nobody declared, or text the agent authored")
	}

	// A card that also sent STRUCTURE has to say the same thing in it.
	//
	// Surfaces choose between the two halves — prose for a plain-text channel,
	// items for one with a layout — and which half a given reader gets is not
	// something the publisher controls. So a fact present in one and absent
	// from the other is a surface where the approval reads differently, which
	// is the same failure as the card that was built correctly and published
	// with only its lead: every assertion passed and the human saw nothing.
	//
	// Asserted as AGREEMENT rather than as a second list in the bundle, because
	// a bundle that restates its expectations twice is a bundle whose halves
	// drift apart — and it catches divergence the author never thought to check.
	//
	// Skipped when no card carried structure: a single-phase card is prose by
	// design, and demanding structure of it would fail every bundle predating
	// this.
	if len(structured) == 0 {
		return
	}
	joinedItems := strings.Join(structured, "\n")
	for _, want := range pg.CardWhatContains {
		assert.Contains(t, joinedItems, want,
			"the prose half of this card names %q and the structured half does not; "+
				"a surface rendering items would not show it", want)
	}
	for _, bad := range pg.CardWhatOmits {
		assert.NotContains(t, joinedItems, bad,
			"the structured half claimed %q, which the prose half is forbidden to", bad)
	}
}

// flattenItems renders a structured field as text so the same contains/omits
// claims can be made about it. Detail is included: that is where a resource's
// concrete instance lives, which is the part an approval is actually about.
func flattenItems(items []channelevents.InteractionItem) string {
	var b strings.Builder
	var walk func([]channelevents.InteractionItem)
	walk = func(in []channelevents.InteractionItem) {
		for _, it := range in {
			b.WriteString(it.Text)
			if it.Detail != "" {
				b.WriteString(" " + it.Detail)
			}
			b.WriteString("\n")
			walk(it.Items)
		}
	}
	walk(items)
	return b.String()
}

// loadManifests resolves each extraManifests entry to manifests/<name>.yaml
// beside the bundle.
//
// Files rather than inline YAML: an override must restate a whole object (see
// the note on Bundle.ExtraManifests), and a multi-hundred-byte YAML blob
// escaped into JSON is unreadable and unreviewable.
func loadManifests(t *testing.T, dir string, b bt.Bundle) []string {
	t.Helper()
	out := make([]string, 0, len(b.ExtraManifests))
	for _, name := range b.ExtraManifests {
		raw, err := os.ReadFile(filepath.Join(dir, "manifests", name+".yaml"))
		require.NoError(t, err, "extraManifests %q", name)
		out = append(out, string(raw))
	}
	return out
}

// startAutoApprover clears approval prompts in the background for the
// categories a bundle opted into.
//
// Scenarios that legitimately pause for a human — a phase awaiting approval —
// would otherwise block until the class's approval timeout and read as a hang.
// Approving in the background models the human being present, which is the case
// the scenario is actually about; a bundle that wants the REFUSAL path simply
// does not opt in.
func startAutoApprover(t *testing.T, h *e2e.Harness, b bt.Bundle) {
	t.Helper()
	if len(b.AutoApprove) == 0 && len(b.AutoDeny) == 0 {
		return
	}
	want := map[string]bool{}
	for _, k := range b.AutoApprove {
		want[k] = true
	}
	refuse := map[string]bool{}
	for _, k := range b.AutoDeny {
		refuse[k] = true
	}

	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })

	go func() {
		seen := 0
		for {
			select {
			case <-stop:
				return
			default:
			}
			prompts := h.PublishedApprovalPrompts()
			for i := seen; i < len(prompts); i++ {
				cat := string(prompts[i].Payload.Category)
				if !want[cat] && !refuse[cat] {
					continue
				}
				var opts []e2e.SendOption
				if b.AutoApproveAs != "" {
					opts = append(opts, e2e.AsUser(b.AutoApproveAs))
				}
				if refuse[cat] {
					h.DenyInteraction(prompts[i], opts...)
					continue
				}
				h.ApproveInteraction(prompts[i], opts...)
			}
			if len(prompts) > seen {
				seen = len(prompts)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}()
}

// stampToolspecsValid marks every SpiceboxToolspec Valid=True.
func stampToolspecsValid(t *testing.T, h *e2e.Harness) {
	t.Helper()
	var list spiceboxv1alpha1.SpiceboxToolspecList
	// A List that failed is not "this fixture declares none": swallowing it
	// leaves every toolspec unstamped, and the bundle then fails much later as
	// a class parked Valid=False, with nothing naming the read that failed.
	require.NoError(t, h.K8s.List(context.Background(), &list), "list SpiceboxToolspecs to stamp Valid=True")
	for i := range list.Items {
		ts := &list.Items[i]
		if apimeta.IsStatusConditionTrue(ts.Status.Conditions, spiceboxv1alpha1.SpiceboxToolspecConditionValid) {
			continue
		}
		ts.Status.Conditions = []metav1.Condition{{
			Type:               spiceboxv1alpha1.SpiceboxToolspecConditionValid,
			Status:             metav1.ConditionTrue,
			Reason:             "Resolved",
			LastTransitionTime: metav1.Now(),
		}}
		require.NoError(t, h.K8s.Status().Update(context.Background(), ts),
			"stamp toolspec %q Valid=True", ts.Name)
	}
}

// stampAgentUIsValid marks every AgentUI Valid=True — the fourth controller
// stand-in the Run comment above names.
//
// pkg/controllers/agentui is not registered in this harness (see the
// registrations in test/e2e/harness.go), so an AgentUI a fixture applies would
// otherwise sit with an empty Conditions slice forever, and
// pkg/controllers/agentclass's validateAgentUI reads exactly that condition
// before admitting any class referencing it — parking such a class at
// Valid=False/AgentUIInvalid regardless of how correct its own spec is. This
// is the same gap test/e2e/scenarios/agentui/view_capability_test.go's
// markAgentUIValid closes by hand for its own two tests; a bundle is data,
// with nowhere to put that same imperative step, so it belongs here instead,
// once, for every bundle that ships an AgentUI.
func stampAgentUIsValid(t *testing.T, h *e2e.Harness) {
	t.Helper()
	var list spiceboxv1alpha1.AgentUIList
	// Same as stampToolspecsValid: a failed List must fail the bundle here,
	// not silently become an AgentUI nobody stamped.
	require.NoError(t, h.K8s.List(context.Background(), &list), "list AgentUIs to stamp Valid=True")
	for i := range list.Items {
		aui := &list.Items[i]
		if apimeta.IsStatusConditionTrue(aui.Status.Conditions, spiceboxv1alpha1.AgentUIConditionValid) {
			continue
		}
		aui.Status.Conditions = []metav1.Condition{{
			Type:               spiceboxv1alpha1.AgentUIConditionValid,
			Status:             metav1.ConditionTrue,
			Reason:             spiceboxv1alpha1.ReasonAgentUISpecOK,
			LastTransitionTime: metav1.Now(),
		}}
		require.NoError(t, h.K8s.Status().Update(context.Background(), aui),
			"stamp AgentUI %q Valid=True", aui.Name)
	}
}

// stampBundleSessionsReady stands in for the SpiceboxSession provisioner.
// Idempotent; joined on cleanup so it cannot write status during teardown.
//
// streamDriver, when non-nil, is the scripted program a bundle's toolkit binary
// runs. It is installed onto the fake exec binder for the session's pod key
// BEFORE that session is stamped Ready, and never after: Ready is what lets the
// runner dispatch, and a dispatch that reaches an unprogrammed key fails the
// bridge on an EOF rather than running the scenario.
//
// sandboxOuts is the same thing for NON-streaming sandbox tools, and carries
// the one fact a static Response cannot: which tool an argv belongs to. It is
// installed at the same moment and under the same rule — before Ready, once per
// session — but it additionally waits for Status.ResolvedClass, because the
// tool catalog it dispatches on lives there.
func stampBundleSessionsReady(
	t *testing.T,
	h *e2e.Harness,
	streamDriver func(io.Reader, io.Writer, io.Writer) int32,
	sandboxOuts map[string][]bt.SandboxOutput,
) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	t.Cleanup(func() { cancel(); <-done })

	installed := map[string]bool{}
	installedSandbox := map[string]bool{}
	go func() {
		defer close(done)
		tick := time.NewTicker(150 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
			var list spiceboxv1alpha1.SpiceboxSessionList
			if err := h.K8s.List(ctx, &list); err != nil {
				if ctx.Err() != nil {
					return
				}
				continue
			}
			for i := range list.Items {
				sb := &list.Items[i]
				dirty := false
				if sb.Status.PodName == "" {
					sb.Status.PodName = sb.Name + "-pod" // mirrors podspec.PodNameFor
					// The handle the ToolCall reconciler resolves an executor
					// through. Only meaningful when the reconciler is running
					// (a streaming bundle); harmless status for every other.
					sb.Status.Sandbox = &spiceboxv1alpha1.SandboxHandle{
						Kind: pod.KindName,
						Ref:  sb.Namespace + "/" + sb.Status.PodName,
					}
					dirty = true
				}
				if streamDriver != nil && sb.Status.PodName != "" && !installed[sb.Name] {
					h.FakeExec().ProgramStreamFunc(sb.Namespace+"/"+sb.Status.PodName+":sandbox", streamDriver)
					installed[sb.Name] = true
				}
				if sb.Status.ResolvedClass == nil {
					var cls spiceboxv1alpha1.SpiceboxClass
					if err := h.K8s.Get(ctx, client.ObjectKey{Name: sb.Spec.Class}, &cls); err == nil {
						sb.Status.ResolvedClass = cls.Spec.DeepCopy()
						dirty = true
					}
				}
				// The sandbox responder needs the class tool catalog, so it goes
				// in only once ResolvedClass is populated — which the block
				// directly above does, in this same pass, before the Ready
				// stamp at the bottom. That ordering is what the whole function
				// is careful about: Ready is what lets the runner dispatch, and
				// a dispatch reaching an unprogrammed key fails the call.
				//
				// Installed whenever the class HAS tools, even with no recorded
				// outputs at all: an empty map is what makes the responder say
				// "the bundle recorded no toolOutputs entry for sandbox tool X",
				// which is a far better failure than the binder's "no program
				// for <pod key>". A streaming bundle is unaffected — its driver
				// is registered through ProgramStreamFunc, a separate map.
				if sb.Status.PodName != "" && sb.Status.ResolvedClass != nil &&
					len(sb.Status.ResolvedClass.Tools) > 0 && !installedSandbox[sb.Name] {
					// The bundle label is what the AgentSession reconciler
					// stamps on each per-bundle SpiceboxSession, and it is how a
					// class tool reached by two toolBundles gets each bundle's
					// OWN recorded output rather than one of them twice.
					bundleName := sb.Labels["agentprimitives.authzed.com/agentbundle"]
					key := sb.Namespace + "/" + sb.Status.PodName + ":sandbox"
					h.FakeExec().ProgramFunc(key,
						sandboxResponder(bundleName, sb.Status.ResolvedClass.Tools, sandboxOuts))
					// A recorded STREAMING result is answered by the binder's
					// OTHER half: the toolkit runs as a stream, so the call
					// never reaches Exec at all. Registered on the same pod key
					// and beside the responder above, because both need the
					// resolved class — and only here, since streamResultsFor
					// reads which class tool the recording belongs to.
					tool, results, err := streamResultsFor(bundleName, sb.Status.ResolvedClass.Tools, sandboxOuts)
					switch {
					case err != nil:
						// t.Errorf, never Fatalf: this runs on the stamping
						// goroutine, where Goexit would abandon the loop rather
						// than the test. Nothing is registered, so the call
						// itself then fails with the binder's own "no stream
						// program" and this says why.
						t.Errorf("%s", err.Error())
					case tool != "":
						h.FakeExec().ProgramStreamFunc(key, streamResponder(results))
					}
					installedSandbox[sb.Name] = true
				}
				if len(sb.Status.EffectiveToolspecs) == 0 && sb.Status.ResolvedClass != nil {
					var eff []string
					for _, tr := range sb.Spec.Toolspecs {
						eff = append(eff, tr.Name)
					}
					if len(eff) == 0 {
						for _, tr := range sb.Status.ResolvedClass.Toolspecs {
							eff = append(eff, tr.Name)
						}
					}
					if len(eff) > 0 {
						sb.Status.EffectiveToolspecs = eff
						dirty = true
					}
				}
				if !apimeta.IsStatusConditionTrue(sb.Status.Conditions, "Ready") {
					apimeta.SetStatusCondition(&sb.Status.Conditions, metav1.Condition{
						Type: "Ready", Status: metav1.ConditionTrue, Reason: "Stamped",
					})
					dirty = true
				}
				if dirty {
					_ = h.K8s.Status().Update(ctx, sb)
				}
			}
		}
	}()
}

// checkNoticeCounts asserts the EXACT number of cards published per category.
//
// The per-category companion to PlanGate.ApprovalPrompts, for the notice
// categories NoticesPublished can only test for membership. Polls until every
// named category has reached AT LEAST its expected count (notices deliver async:
// runner → NATS → relay → sender), then settles briefly and asserts EXACT
// equality — because a count HIGHER than expected is the regression this exists
// to catch (a card re-raised per retry), and stopping the instant expected was
// reached would miss a late duplicate.
func checkNoticeCounts(t *testing.T, h *e2e.Harness, b bt.Bundle) {
	t.Helper()
	if len(b.Assert.NoticeCounts) == 0 {
		return
	}
	count := func() map[string]int {
		m := map[string]int{}
		for _, p := range h.PublishedApprovalPrompts() {
			m[string(p.Payload.Category)]++
		}
		return m
	}
	cutoff := time.Now().Add(30 * time.Second)
	for {
		got := count()
		reached := true
		for cat, want := range b.Assert.NoticeCounts {
			if got[cat] < want {
				reached = false
				break
			}
		}
		if reached || time.Now().After(cutoff) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond) // let a late duplicate land before the exact check
	got := count()
	for cat, want := range b.Assert.NoticeCounts {
		assert.Equal(t, want, got[cat],
			"category %q published %d time(s), want exactly %d — a higher count is a card "+
				"re-raised per retry; a lower one, a notice that never reached the person",
			cat, got[cat], want)
	}
}

// checkNoticesPublished asserts the run posted each named interaction category
// to the bound channel.
//
// Reads the fake channel's own record of what it was asked to SHOW, not the
// bus: a notice that was published but never reached a surface told nobody
// anything, and this is the difference between the system deciding to speak and
// a person hearing it.
//
// Polls, because a notice travels runner → NATS → the channelsd relay → the
// channel sender, asynchronously with the tool call that raised it. The last
// assertion in a bundle can otherwise run while the envelope is still in
// flight.
func checkNoticesPublished(t *testing.T, h *e2e.Harness, b bt.Bundle) {
	t.Helper()
	if len(b.Assert.NoticesPublished) == 0 && len(b.Assert.NoticesNotPublished) == 0 {
		return
	}
	var seen []string
	cutoff := time.Now().Add(30 * time.Second)
	for {
		seen = seen[:0]
		for _, p := range h.PublishedApprovalPrompts() {
			seen = append(seen, string(p.Payload.Category))
		}
		missing := false
		for _, want := range b.Assert.NoticesPublished {
			if !slices.Contains(seen, want) {
				missing = true
				break
			}
		}
		if !missing || time.Now().After(cutoff) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	for _, want := range b.Assert.NoticesPublished {
		assert.Contains(t, seen, want,
			"the run must have told the user: no %q notice reached the channel (posted: %v)", want, seen)
	}
	// Checked after the positive set has settled, so a forbidden category that
	// would have arrived alongside an expected one is caught rather than
	// passing on timing.
	//
	// PAIR AN ABSENCE ASSERTION WITH A POSITIVE ONE. With an empty
	// NoticesPublished the loop above exits on its first pass, so absence would
	// be asserted against a channel nothing has reached yet and would pass
	// vacuously. Every notice for one message is published in the same pass, so
	// waiting for the one that should arrive is what makes "and not that one"
	// mean something.
	for _, forbidden := range b.Assert.NoticesNotPublished {
		assert.NotContains(t, seen, forbidden,
			"the run told the user the WRONG thing: a %q notice reached the channel (posted: %v)", forbidden, seen)
	}
}

// renderNameMinter adapts the render-handle family onto the artifact service's
// seam, which takes the session name.
//
// The argument is unused, and deliberately: a recorded handle is the WHOLE CR
// name, session segment included, and the replay recreates the session under
// the name the fixture pins — the same name the capture read the handle from.
// Re-composing "ar-" + session + recorded suffix would be a second spelling of
// artifacts.RenderName living here, which is exactly the drift that format's
// doc says to avoid; handing back the recorded value entire has no format in it
// at all.
func renderNameMinter(seq *bt.MintedIDSequence) func(string) string {
	mint := seq.Minter(bt.FamilyRenderHandle)
	if mint == nil {
		return nil // unpinned: the service mints its own, exactly as in production
	}
	return func(string) string { return mint() }
}

// revisionIDMinter wires the revision family onto the artifact service's seam.
//
// A named function rather than the one-liner it wraps, because WHICH minter it
// draws from is the load-bearing part and an inline call has nowhere to be
// tested. It must be KeyedMinter: a revision id is DERIVED from the render CR's
// UID, and artifact_await re-derives it for the SAME CR, so a plain Minter
// would satisfy the first finalize and hand the second a different id —
// finalizing a second revision of one render and double-counting the head. See
// artifacts.WithRevisionIDMinter, whose contract this has to honour.
func revisionIDMinter(seq *bt.MintedIDSequence) func(uid string) string {
	return seq.KeyedMinter(bt.FamilyArtifactRevision)
}

// runArtifactRenders runs the REAL ArtifactRender reconciler against the
// harness's API server, in a poll loop.
//
// A controller stand-in alongside stampToolspecsValid and
// stampBundleSessionsReady, in the same shape and for the same reason — a
// bundle is data with nowhere to put an imperative step, and artifact_prepare
// BLOCKS until the render reaches a terminal phase — but it is a stand-in for
// the manager's WIRING only, not for the controller's behaviour. The loop
// supplies the "something noticed this object" that a real manager's watch
// would, and Reconcile does all of the actual work.
//
// It used to STAMP a terminal status instead: Ready, MIME text/html, size =
// len(payload), no warnings. That was wrong in a way nothing could see. The
// real reconciler dispatches to the registered renderer, and the html renderer
// SANITIZES — it strips tags and attributes and reports what it did — so the
// bytes it emits are a different length from the bytes it was given and its
// warnings are part of the result artifact_prepare hands the model. A replay
// that stamped instead of rendering therefore diverged from production on every
// artifact, silently, because no scenario asserted on those fields. The first
// thing to look at them was a captured session, which diverged on `size` and on
// four warnings that simply were not there.
//
// Always on, like its two siblings: a bundle whose class never grants the
// artifacts capability creates no renders and the loop finds nothing to do.
func runArtifactRenders(t *testing.T, h *e2e.Harness, store artifactstore.Store) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	t.Cleanup(func() { cancel(); <-done })

	// The same reconciler internal/cmd/operator constructs, over the same store
	// the runner's artifact tools read through. MaxOutputAbsolute left zero
	// takes the controller's own 10 MiB default, as in production.
	r := &artifactrender.Reconciler{Client: h.K8s, Store: store}

	go func() {
		defer close(done)
		tick := time.NewTicker(50 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
			var list spiceboxv1alpha1.ArtifactRenderList
			if err := h.K8s.List(ctx, &list); err != nil {
				if ctx.Err() != nil {
					return
				}
				continue
			}
			for i := range list.Items {
				ar := &list.Items[i]
				if ar.Status.Phase == spiceboxv1alpha1.ArtifactRenderPhaseReady ||
					ar.Status.Phase == spiceboxv1alpha1.ArtifactRenderPhaseFailed {
					continue
				}
				if _, err := r.Reconcile(ctx, ctrl.Request{
					NamespacedName: types.NamespacedName{Namespace: ar.Namespace, Name: ar.Name},
				}); err != nil {
					if ctx.Err() != nil {
						return
					}
					// Not fatal and not swallowed: a manager retries a failed
					// reconcile and so does the next tick, and a render that
					// never reaches a terminal phase fails the run loudly at
					// artifact_prepare's own timeout. Logged so that failure is
					// diagnosable rather than a bare 30-second wait.
					t.Logf("artifactrender reconcile %s/%s: %v", ar.Namespace, ar.Name, err)
				}
			}
		}
	}()
}

// checkSystemPrompt asserts on the prompt the runner actually composed, both
// what it must carry (SystemPromptContains) and what it must not
// (SystemPromptNotContains).
//
// Reads it off the requests the scripted provider received, because that is the
// only place the COMPOSED prompt exists — the runner builds it from the class,
// its tools and their published status, and nothing else in the run reflects
// what came out. A bundle asserting on the agent's reply instead would pass
// with the prompt empty: the reply is scripted.
func checkSystemPrompt(t *testing.T, h *e2e.Harness, b bt.Bundle) {
	t.Helper()
	if len(b.Assert.SystemPromptContains) == 0 && len(b.Assert.SystemPromptNotContains) == 0 {
		return
	}
	reqs := h.LLM.Requests()
	require.NotEmpty(t, reqs, "no LLM request was made, so no prompt was composed")

	var sb strings.Builder
	for _, blk := range reqs[0].System {
		sb.WriteString(blk.Text)
		sb.WriteString("\n")
	}
	prompt := sb.String()
	for _, want := range b.Assert.SystemPromptContains {
		assert.Contains(t, prompt, want,
			"the composed system prompt must carry this; the class declared it and the runner is what renders it")
	}
	for _, unwanted := range b.Assert.SystemPromptNotContains {
		assert.NotContains(t, prompt, unwanted,
			"the composed system prompt must NOT carry this; the text is gated on something this class does not have")
	}
}

// attachmentExternalID is the synthetic handle the fake kind fetches by. It
// encodes the turn and slot so a failure names which attachment went missing
// rather than just "one of them".
func attachmentExternalID(turnIdx, slotIdx int) string {
	return fmt.Sprintf("bt-%d-%d", turnIdx, slotIdx)
}

// bundleAttachments turns one authored turn's declared files into the
// InboundAttachments a listener would have recorded.
//
// SizeBytes comes from stat'ing the fixture rather than from the bundle,
// because the pipeline's pre-fetch size check reads it and a hand-written
// number that disagreed with the file would make an oversize test pass for the
// wrong reason.
func bundleAttachments(t *testing.T, dir string, turnIdx int, turn bt.UserTurn) []channelkinds.InboundAttachment {
	t.Helper()
	if len(turn.Attachments) == 0 {
		return nil
	}
	out := make([]channelkinds.InboundAttachment, 0, len(turn.Attachments))
	for j, a := range turn.Attachments {
		// SizeBytes comes from the fixture, never from the bundle JSON: the
		// pipeline's pre-fetch size check reads it, and a hand-written number
		// that disagreed with the file would make an oversize test pass for the
		// wrong reason.
		var size int64
		if a.ZipFrom != "" {
			size = int64(len(buildFixtureZip(t, filepath.Join(dir, a.ZipFrom))))
		} else {
			fi, serr := os.Stat(filepath.Join(dir, a.File))
			require.NoError(t, serr, "bundle attachment %q is missing from the fixture directory", a.File)
			size = fi.Size()
		}
		out = append(out, channelkinds.InboundAttachment{
			ExternalID: attachmentExternalID(turnIdx, j),
			Filename:   a.Filename,
			MIME:       a.MIME,
			SizeBytes:  size,
		})
	}
	return out
}

// buildFixtureZip zips every regular file under srcDir, using paths relative
// to srcDir as member names.
//
// Built here rather than committed: a binary archive in testdata cannot be
// reviewed — nobody can tell whether it holds what its filename claims — and a
// bomb committed by accident is a landmine. The source files stay plain text.
func buildFixtureZip(t *testing.T, srcDir string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	err := filepath.Walk(srcDir, func(p string, info os.FileInfo, werr error) error {
		if werr != nil || info.IsDir() {
			return werr
		}
		rel, rerr := filepath.Rel(srcDir, p)
		if rerr != nil {
			return rerr
		}
		body, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		w, cerr := zw.Create(filepath.ToSlash(rel))
		if cerr != nil {
			return cerr
		}
		_, cerr = w.Write(body)
		return cerr
	})
	require.NoError(t, err, "building the fixture archive from %s", srcDir)
	require.NoError(t, zw.Close())
	return buf.Bytes()
}

// checkArtifactsDelivered asserts every artifact ID the bundle named actually
// travelled to the transport, reading the fake channel's own send record.
//
// The claim it defends is narrow and not covered elsewhere: respond_to_user
// answering "delivered" establishes that the handle resolved and the
// entitlement passed, not that a render reached the person. An empty
// attachment list satisfies every text assertion in a bundle while a report
// silently goes missing.
func checkArtifactsDelivered(t *testing.T, h *e2e.Harness, b bt.Bundle) {
	t.Helper()
	if len(b.Assert.ArtifactsDelivered) == 0 {
		return
	}
	got := h.DeliveredArtifactIDs()
	for _, want := range b.Assert.ArtifactsDelivered {
		found := false
		for _, id := range got {
			if strings.Contains(id, want) {
				found = true
				break
			}
		}
		assert.True(t, found,
			"no artifact whose ID contains %q reached the output channel; delivered artifact IDs were %v",
			want, got)
	}
}
