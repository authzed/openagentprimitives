//go:build e2e

package threadrun

import (
	"context"
	"io"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	bt "github.com/authzed/openagentprimitives/pkg/bronzethread"
	"github.com/authzed/openagentprimitives/test/e2e"
)

// credentialHaltBundle is the one scenario driving a real streaming toolkit.
const credentialHaltBundle = "credential-halt-stops-the-session"

// unbilledFailureNDJSON is what the fixture's fake `codey` writes to stdout: a
// claude-stream-json transcript whose terminal result declares the run
// unsuccessful AND metered at zero.
//
// The `result` line is the whole fixture. Everything above it is scenery, and
// the auth-error text inside it is deliberately loud — the credential halt
// reads the subtype and the billing tally, never this string, and a bundle
// whose fixture said nothing alarming could not show that.
const unbilledFailureNDJSON = `{"type":"system","subtype":"init","cwd":"/work","tools":["Read","Edit","Bash"]}
{"type":"assistant","message":{"content":[{"type":"text","text":"I'll take a look at that."}]}}
{"type":"result","subtype":"error_during_execution","result":"API Error: 401 {\"type\":\"error\",\"error\":{\"type\":\"authentication_error\",\"message\":\"invalid x-api-key\"}}","total_cost_usd":0,"duration_ms":812}
`

// toolkitStreamFor returns the stdout a bundle's scripted toolkit binary emits,
// and the exit code it ends on; ("", 0) for every bundle that drives no
// streaming toolkit, which is all of them but one.
//
// Keyed by bundle NAME for the same reason planGateDenialStreakThresholdFor is:
// bt.Bundle carries no field for it, and adding exported schema for a single
// scenario buys nothing. Unlike a plan-gate threshold, though, this is not an
// operator flag — it is a stateful tool's canned output, which is exactly what
// Bundle.ToolOutputs already is for MCP. If a second streaming scenario ever
// appears, the honest move is to widen ToolOutputs rather than to add a second
// name here.
//
// The exit code matters as much as the bytes: composeStreamResult reads the
// credential observation only off a process that FAILED, since "completed" is
// what a run that reached its own end looks like and our watchdog owns the
// other two reasons. A fixture exiting 0 would produce a result the guard
// correctly ignores, and the bundle would fail for a reason that has nothing to
// do with the halt.
func toolkitStreamFor(b bt.Bundle) (stdout string, exitCode int32) {
	if b.Name == credentialHaltBundle {
		return unbilledFailureNDJSON, 1
	}
	return "", 0
}

// streamDriverFor builds the fake exec's program for a bundle, or nil when the
// bundle drives no streaming toolkit. Fire-and-forget: it writes its whole
// transcript and exits, taking no stdin, because a stream-mode subcommand is a
// one-shot run with a closed stdin.
func streamDriverFor(b bt.Bundle) func(io.Reader, io.Writer, io.Writer) int32 {
	out, code := toolkitStreamFor(b)
	if out == "" {
		return nil
	}
	return func(_ io.Reader, stdout, _ io.Writer) int32 {
		_, _ = stdout.Write([]byte(out))
		return code
	}
}

// exactTranscriptLengthBundles names the bundles whose run must make exactly
// the model calls their transcript describes. Keyed by name rather than by a
// bt.Bundle field for the same reason wantSessionPhaseFor is: it is a property
// of a handful of scenarios, not exported surface the format needs.
var exactTranscriptLengthBundles = map[string]bool{
	credentialHaltBundle: true,
	"completion-requirement-refuses-undelivered-artifact": true,
	// The reviewbot round needs it for a third reason: its last authored step
	// is an agent_work_complete the completion gate is meant to ALLOW. A gate
	// that refused it instead would push the round back for another turn, the
	// driver's repeating epilogue rule would answer, and every property
	// assertion below would still pass — the check run is concluded by then,
	// the report is delivered, the thread is rooted. Only the count says the
	// round ended where the transcript says it did.
	"reviewbot-claim-review-conclude": true,
	// The satisfied-by-attachment bundle is the reviewbot case in miniature,
	// and it is the ONLY thing that bundle proves. Its one property assertion
	// (`agentReplyContains`) is answered at step 2 of 4, two steps before the
	// agent_work_complete whose acceptance is the entire scenario. Break the
	// gate so it refuses a round that DID attach and the run overshoots into
	// the epilogue rule, which ends the turn — the reply had already matched,
	// so nothing else notices. Verified by breaking artifactdelivery.Check:
	// without this row the bundle passes in both states.
	"completion-requirement-satisfied-by-attachment": true,
}

// checkTranscriptLength asserts a bundle's run made exactly the model calls its
// transcript describes, for the bundles that opt in by name.
//
// It closes the one hole a phase assertion leaves. An unfired LLM step is
// SILENT — the driver registers each step as a single-use rule and never
// requires it to match — so a credential halt that fired one call too EARLY
// still reaches phase Failed, with the step whose `expect` pins the first
// call's tool result simply never running. The count is what tells "halted on
// the second refusal" from "halted on the first", and equally from "did not
// halt at all", where the loop runs on into the driver's repeating epilogue
// rule and overshoots.
//
// Opt-in because it is not true in general: a bundle that ends its turn
// normally takes one more model call than it authored (the loop's own
// post-completion check), which the epilogue rule exists to absorb. A bundle
// whose last authored step is a TERMINAL tool call takes no such extra call —
// the loop stops on the terminal result — which is what makes the count exact
// for the scenarios above.
//
// The two completion-requirement bundles need it for the same reason the
// credential halt does, one step further on, in both directions. In the refusal
// bundle, a gate that was off would let the FIRST agent_work_complete succeed
// and end the session, so the step asserting on the refusal — and the step that
// bypasses it — never fire. In the satisfied bundle, a gate that refused a
// round that DID attach would push it past its LAST authored step into the
// epilogue rule. Either way the unfired step is silent; the count is what turns
// that silence into a failure.
//
// The count is only trustworthy once the run has finished. checkSessionPhase
// runs first for exactly that reason — see wantSessionPhaseFor, which names the
// bundles whose reply lands before their last model call.
func checkTranscriptLength(t *testing.T, h *e2e.Harness, b bt.Bundle) {
	t.Helper()
	if !exactTranscriptLengthBundles[b.Name] {
		return
	}
	assert.Len(t, h.LLM.Requests(), len(b.LLM),
		"the run must make exactly the model calls the transcript describes: fewer means the "+
			"session stopped before a step the bundle asserts on, more means it did not stop at all")
}

// waitForSessionExists blocks until the pipeline has created the run's
// AgentSession, so a caller may then read it by ref.
//
// h.SessionRef t.Fatals on "none yet" rather than waiting, which is right for
// its usual caller (a scenario that has already awaited a reply) and wrong for
// one whose session never replies.
func waitForSessionExists(t *testing.T, h *e2e.Harness, deadline time.Duration) {
	t.Helper()
	cutoff := time.Now().Add(deadline)
	for time.Now().Before(cutoff) {
		var list spiceboxv1alpha1.AgentSessionList
		if err := h.K8s.List(context.Background(), &list, client.InNamespace(h.Namespace())); err == nil && len(list.Items) > 0 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("no AgentSession appeared within %s; the turn never reached the pipeline", deadline)
}

// bindWorkshopSessionOnce late-binds the real workshop MCP mount's
// SessionNamespace/SessionName (workshopmcp.Server.SetBuilderSession, via
// Harness.BindWorkshopSession) to the builder AgentSession's actual
// (namespace, name), the FIRST time it is called for a bundle whose fixture
// routes through that mount — a no-op for every other bundle in the suite.
//
// Called from Run's turn loop after EVERY user turn (guarded by *bound so it
// only does anything once): the mounted server has to exist before any
// session does (mountWorkshopMCP runs during Harness.Start — see that func's
// own doc), so SessionNamespace/SessionName start out wrong for every bundle,
// and eight workshop tools (test_tool, watch_test, test_sessions,
// request_credential, request_install, recommend_capability, close_others,
// export_draft's registry read) key a Workshop-CR or parent-session lookup
// on them — test_link does not: it Gets the candidate AgentClass by
// Identity.Namespace alone, which is frozen at mount time and needs no bind.
// A bundle that needs one of those eight tools to work must send an EARLIER
// user turn that does not need them
// (see testdata/workshop-recommend-capability-is-real's
// two-turn shape), so this hook has a synchronized session to bind by the
// time the later turn runs.
//
// Detected by the presence of a SidecarToolbox literally named
// workshopSidecarToolboxRef — the exact same name driver.go's own sidecar-
// probe routing closure matches on to decide a call reaches the real server
// at all, so "this bundle uses the mount" and "this bundle needs the bind"
// are the same question asked the same way. A bundle with no such toolbox
// gets a single cheap Get-not-found and nothing else — no poll, no
// SessionRef call — so ordinary bundles pay nothing for this existing.
//
// waitForSessionExists — not merely trusting that a prior ExpectAgentReply
// call already implies the session exists — because a bundle's first user
// turn is not required to carry an assert.agentReplyContains entry; the wait
// is the only universally-safe synchronization point. SendUserMessage itself
// only enqueues the inbound event (see its own doc): the pipeline that
// creates the AgentSession runs on other goroutines, so reading h.SessionRef
// immediately after SendUserMessage returns would be a bare race, not a
// synchronization point.
func bindWorkshopSessionOnce(t *testing.T, h *e2e.Harness, bound *bool) {
	t.Helper()
	if *bound {
		return
	}
	var sb spiceboxv1alpha1.SidecarToolbox
	key := client.ObjectKey{Namespace: h.Namespace(), Name: workshopSidecarToolboxRef}
	if err := h.K8s.Get(context.Background(), key, &sb); err != nil {
		if apierrors.IsNotFound(err) {
			return // this bundle mounts no "workshop" SidecarToolbox; nothing to bind.
		}
		// A transient API error read as NotFound would otherwise surface much
		// later as a confusing "not found" tool result on one of the six
		// tools sessionRef keys on, instead of failing here where the actual
		// cause is visible.
		require.NoError(t, err, "Get SidecarToolbox %q to decide whether this bundle needs a workshop-session bind", key)
	}
	waitForSessionExists(t, h, 30*time.Second)
	ns, name := h.SessionRef()
	h.BindWorkshopSession(ns, name)
	*bound = true
}
