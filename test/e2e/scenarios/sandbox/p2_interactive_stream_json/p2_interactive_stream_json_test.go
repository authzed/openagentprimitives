//go:build e2e

package p2_interactive_stream_json_test

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/tools/sandboxkinds/pod"
	"github.com/authzed/openagentprimitives/test/e2e"
)

// claudeStreamJSONHappyPath is the canonical Phase A happy-path fixture
// from pkg/tools/toolkitstream/claude/testdata/happy_path.ndjson, inlined so
// this test file stands alone. Three NDJSON lines: a system init the
// parser drops, an assistant text block ("Hello! I'll help with that."),
// and a result line declaring success + cost/duration metadata.
const claudeStreamJSONHappyPath = `{"type":"system","subtype":"init","cwd":"/work","tools":["Read","Edit","Bash"]}
{"type":"assistant","message":{"content":[{"type":"text","text":"Hello! I'll help with that."}]}}
{"type":"result","subtype":"success","result":"Hello! I'll help with that.","total_cost_usd":0.0123,"duration_ms":4321}
`

// TestP2_InteractiveStreamJSON_HappyPath captures the full Phase A
// stream-json pipeline end-to-end:
//
//   - The cluster carries a SpiceboxToolkit whose YAML declares
//     streamFormat=claude-stream-json. The sandbox tool's interactive
//     dispatch consults the field, resolves a Parser from
//     pkg/tools/toolkitstream/registry, and wraps the bridge's stdout chunks
//     through it.
//   - The scripted fake exec emits the canonical Claude stream-json
//     NDJSON fixture (system init / assistant text / result).
//   - The runner's InteractiveHooks.OnEvent — wired identically in
//     internal/cmd/runner/main.go and the e2e InProcessRunnerFactory — fires per
//     parsed event, publishing one KindToolSessionEvent envelope each
//     on the session's OUT subject.
//   - This test subscribes to ap.session.*.*.out.tool_session_event and
//     asserts text_delta + result events arrive in order.
//   - Critically, it ALSO asserts no stdout-stream KindToolSessionDelta
//     envelopes leak through when the parser is active. Only stderr
//     deltas and the terminal delta are permitted — proving the
//     "parser owns stdout, raw owns stderr+terminal" routing in
//     pkg/agent/tool/sandbox/interactive_tool.go's OnOutput is the
//     active code path.
//
// Inherits the authz.toolCalls.mode=disabled rationale from Scenario 2/3: no
// SpiceDB wired in the harness, so the default enforcing mode would
// deny on the "external" stateImpact without a guardian grant tuple to
// clear the Check.
func TestP2_InteractiveStreamJSON_HappyPath(t *testing.T) {
	manifests, err := os.ReadFile(filepath.Join("manifests.yaml"))
	require.NoError(t, err, "read manifests")

	h := e2e.Start(t, e2e.Options{
		WithToolCallController: true,
		ExtraManifests:         []string{string(manifests)},
		DefaultTimeout:         20 * time.Second,
	})

	// Subscribe to KindToolSessionEvent envelopes on the embedded NATS
	// server. The harness already captures KindToolSessionDelta (via the
	// subscription installed in toolcall_wiring.go) but does not capture
	// events — wire a per-test subscriber here rather than extending the
	// harness's API surface for one scenario. h.NATSURL is the published
	// dial target; per-test subscriptions drain via t.Cleanup so a stray
	// callback from another test cannot bleed through.
	nc, err := nats.Connect(h.NATSURL,
		nats.RetryOnFailedConnect(true),
		nats.MaxReconnects(-1),
	)
	require.NoError(t, err, "dial embedded NATS for tool_session_event subscriber")
	t.Cleanup(func() {
		if err := nc.Drain(); err != nil {
			t.Logf("tool_session_event subscriber: drain on cleanup: %v", err)
		}
	})

	var (
		eventsMu sync.Mutex
		events   []channelevents.ToolSessionEventPayload
	)
	eventSub, err := nc.Subscribe(channelevents.SubjectOut(channelevents.AnySessionPrefix(), channelevents.KindToolSessionEvent), func(m *nats.Msg) {
		var env channelevents.Envelope
		if err := json.Unmarshal(m.Data, &env); err != nil {
			t.Logf("tool_session_event: unmarshal envelope: %v", err)
			return
		}
		var pl channelevents.ToolSessionEventPayload
		if err := json.Unmarshal(env.Payload, &pl); err != nil {
			t.Logf("tool_session_event: unmarshal payload: %v", err)
			return
		}
		eventsMu.Lock()
		events = append(events, pl)
		eventsMu.Unlock()
	})
	require.NoError(t, err, "nats subscribe tool_session_event")
	t.Cleanup(func() { _ = eventSub.Drain() })

	// Side-channel KindToolSessionDelta subscriber: the harness already
	// captures deltas internally for ExpectToolSessionDelta, but the
	// only public accessor is the cursor-advancing Expect API. To assert
	// the anti-leak invariant ("no stdout deltas when parser is active")
	// we need a bulk view; subscribing here before the scenario runs
	// guarantees we observe every delta the runner publishes for the
	// session under test.
	var (
		deltasMu sync.Mutex
		deltas   []channelevents.ToolSessionDeltaPayload
	)
	deltaSub, err := nc.Subscribe(channelevents.SubjectOut(channelevents.AnySessionPrefix(), channelevents.KindToolSessionDelta), func(m *nats.Msg) {
		var env channelevents.Envelope
		if err := json.Unmarshal(m.Data, &env); err != nil {
			t.Logf("tool_session_delta: unmarshal envelope: %v", err)
			return
		}
		var pl channelevents.ToolSessionDeltaPayload
		if err := json.Unmarshal(env.Payload, &pl); err != nil {
			t.Logf("tool_session_delta: unmarshal payload: %v", err)
			return
		}
		deltasMu.Lock()
		deltas = append(deltas, pl)
		deltasMu.Unlock()
	})
	require.NoError(t, err, "nats subscribe tool_session_delta")
	t.Cleanup(func() { _ = deltaSub.Drain() })

	// Low-level stream driver: write the canonical Claude stream-json
	// NDJSON fixture to stdout in one chunk, then return 0. The bridge
	// observes the gateway's Exit message, calls OnTerminal, and a
	// terminal KindToolSessionDelta is published.
	//
	// The scripted-tool helper used by p2_interactive_happy_path drives
	// stdin via a bufio.Scanner that blocks until the channel writes
	// input — wrong shape for this scenario, which is fire-and-forget
	// (no test-side stdin). Drive ProgramStreamFunc directly here.
	codeyDriver := func(_ io.Reader, stdout, _ io.Writer) int32 {
		_, _ = stdout.Write([]byte(claudeStreamJSONHappyPath))
		return 0
	}

	stamperDone := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		<-stamperDone
	})

	// 1. Stamp the SpiceboxToolspec Valid=True so AgentClass binding-
	//    coverage accepts ts-codey-stream-json. (The harness doesn't
	//    wire the SpiceboxToolspec controller — same as Scenarios 1/2/3.)
	stampToolspecsValid(t, ctx, h.K8s, "ts-codey-stream-json")

	// 2. Stamper: marks each SpiceboxSession Ready=True + PodName +
	//    ResolvedClass + EffectiveToolspecs the moment it appears.
	//    CRITICAL ordering: install the driver onto fakeExec BEFORE
	//    flipping Ready=True so the controller's reconcileStreaming path
	//    (triggered by the Mode=interactive ToolCall the runner creates
	//    downstream) finds a programmed stream-func.
	go func() {
		defer close(stamperDone)
		stampBundleSessionsReady(ctx, t, h, codeyDriver)
	}()

	// 3. LLM script. Tool name is "<bundle.name>_<class-tool.name>" =
	//    "codey_codey-stream-json". Same operation_id round-trip pattern
	//    as Scenarios 2/3.
	var capturedOpID string
	h.LLM.OnUserMessage("please stream a claude session").Reply(e2e.ToolUse("new_operation", map[string]any{
		"description": "drive a streaming claude session",
	}))
	h.LLM.OnToolResult("new_operation", e2e.ResultMatches(func(got any) bool {
		if m, ok := got.(map[string]any); ok {
			if id, ok := m["operation_id"].(string); ok {
				capturedOpID = id
			}
		}
		return true
	})).ReplyFn(func() []e2e.ReplyPart {
		return []e2e.ReplyPart{e2e.ToolUse("codey_codey-stream-json", map[string]any{
			"operation_id": capturedOpID,
			"_reason":      "drive a streaming claude session",
			"args":         []string{"run"},
		})}
	})
	h.LLM.OnToolResult("codey_codey-stream-json", e2e.ResultMatches(func(got any) bool {
		s, ok := got.(string)
		// The streaming tool now returns the inner tool's own output as the
		// tool_result content (status header + Claude's final answer). matchToolResult
		// hands non-JSON tool content to the predicate as a raw string.
		return ok && strings.Contains(s, "Hello! I'll help with that.")
	})).Reply(e2e.RespondToUser("stream-json session finished cleanly"))
	h.LLM.OnToolResult("respond_to_user", e2e.AnyResult()).Reply(e2e.EndTurn())

	h.WaitForAgentClassValid("ac-codey-stream-json", 30*time.Second)
	h.SendUserMessage("please stream a claude session")

	// 4. Drive the round-trip. The bridge will fire the terminal delta
	//    on Exit(0); the test's primary signal is the KindToolSessionEvent
	//    stream, polled below.
	h.ExpectToolSessionDelta(e2e.Terminal("completed"))
	h.ExpectAgentReply(e2e.Contains("stream-json session finished cleanly"))

	// 5. Assert: at least one text_delta event, and a final result event
	//    with OK=true. The fixture's 3 lines decode to:
	//      - line 1 "system":   parser returns no events (dropped)
	//      - line 2 "assistant": one EventTextDelta with text="Hello! I'll help with that."
	//      - line 3 "result":    one EventResult with OK=true, DurationMs=4321
	require.Eventually(t, func() bool {
		eventsMu.Lock()
		defer eventsMu.Unlock()
		var sawText, sawResult bool
		for _, pl := range events {
			switch pl.EventType {
			case "text_delta":
				sawText = true
			case "result":
				sawResult = true
			}
		}
		return sawText && sawResult
	}, 30*time.Second, 100*time.Millisecond,
		"expected at least one text_delta AND a result KindToolSessionEvent")

	// Inspect the captured events for stronger assertions on event content.
	eventsMu.Lock()
	captured := append([]channelevents.ToolSessionEventPayload(nil), events...)
	eventsMu.Unlock()

	var (
		sawText, sawResult bool
		resultPayload      channelevents.ToolSessionEventPayload
	)
	for _, pl := range captured {
		switch pl.EventType {
		case "text_delta":
			sawText = true
			assert.Contains(t, pl.Text, "Hello", "text_delta carries the assistant block's text")
		case "result":
			sawResult = true
			resultPayload = pl
		}
	}
	assert.True(t, sawText, "expected a text_delta event")
	assert.True(t, sawResult, "expected a result event")
	assert.True(t, resultPayload.OK, "happy_path fixture → result.OK=true")

	// 6. Critical anti-leak assertion: when a parser is active, stdout
	//    chunks must flow through OnEvent (KindToolSessionEvent) and
	//    NOT through OnOutput (KindToolSessionDelta). Stderr deltas and
	//    the single terminal delta are the only legitimate
	//    KindToolSessionDelta envelopes for this scenario.
	deltasMu.Lock()
	capturedDeltas := append([]channelevents.ToolSessionDeltaPayload(nil), deltas...)
	deltasMu.Unlock()
	for _, pl := range capturedDeltas {
		if pl.Terminal || pl.Stream == "stderr" {
			continue
		}
		t.Fatalf("unexpected stdout KindToolSessionDelta leaked through with parser active: %+v", pl)
	}

	h.AssertAllRulesConsumed()
}

// stampToolspecsValid sets Status.Conditions[Valid]=True on each named
// SpiceboxToolspec. Lifted from Scenarios 2/3 — the canonical
// duplication TODO calls for a shared harness helper once the shape
// settles across enough scenarios.
func stampToolspecsValid(t *testing.T, ctx context.Context, c client.Client, names ...string) {
	t.Helper()
	for _, name := range names {
		var ts spiceboxv1alpha1.SpiceboxToolspec
		require.NoError(t, c.Get(ctx, client.ObjectKey{Name: name}, &ts),
			"get toolspec %q", name)
		ts.Status.Conditions = []metav1.Condition{{
			Type:               spiceboxv1alpha1.SpiceboxToolspecConditionValid,
			Status:             metav1.ConditionTrue,
			Reason:             "Resolved",
			LastTransitionTime: metav1.Now(),
		}}
		require.NoError(t, c.Status().Update(ctx, &ts), "stamp toolspec %q Valid=True", name)
	}
}

// stampBundleSessionsReady polls for SpiceboxSessions in the default
// namespace and stamps them with the fields the runner + ToolCall
// controller need. Critical ordering: the driver is installed onto
// fakeExec for the codey bundle's pod key BEFORE Ready=True is stamped
// (mirrors p2_interactive_happy_path's helper of the same name).
// Without this, a Ready bundle could race against an empty fakeExec
// key and yield an EOF-on-recv bridge failure.
func stampBundleSessionsReady(
	ctx context.Context,
	t *testing.T,
	h *e2e.Harness,
	driver func(stdin io.Reader, stdout, stderr io.Writer) int32,
) {
	t.Helper()
	c := h.K8s
	tick := time.NewTicker(150 * time.Millisecond)
	defer tick.Stop()
	installedFor := map[string]bool{}
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		var list spiceboxv1alpha1.SpiceboxSessionList
		if err := c.List(ctx, &list, client.InNamespace("default")); err != nil {
			if ctx.Err() != nil {
				return
			}
			t.Logf("stampBundleSessionsReady: list: %v", err)
			continue
		}
		for i := range list.Items {
			s := &list.Items[i]
			dirty := false

			if s.Status.PodName == "" {
				s.Status.PodName = s.Name + "-pod"
				s.Status.Sandbox = &spiceboxv1alpha1.SandboxHandle{
					Kind: pod.KindName,
					Ref:  s.Namespace + "/" + s.Status.PodName,
				}
				dirty = true
			}
			if s.Status.ResolvedClass == nil {
				var cls spiceboxv1alpha1.SpiceboxClass
				if err := c.Get(ctx, client.ObjectKey{Name: s.Spec.Class}, &cls); err != nil {
					if ctx.Err() != nil {
						return
					}
					t.Logf("stampBundleSessionsReady: get class %q for %s/%s: %v",
						s.Spec.Class, s.Namespace, s.Name, err)
					continue
				}
				rc := cls.Spec.DeepCopy()
				s.Status.ResolvedClass = rc
				dirty = true
			}
			if len(s.Status.EffectiveToolspecs) == 0 {
				if len(s.Spec.Toolspecs) > 0 {
					eff := make([]string, 0, len(s.Spec.Toolspecs))
					for _, tr := range s.Spec.Toolspecs {
						eff = append(eff, tr.Name)
					}
					s.Status.EffectiveToolspecs = eff
				} else if s.Status.ResolvedClass != nil {
					eff := make([]string, 0, len(s.Status.ResolvedClass.Toolspecs))
					for _, tr := range s.Status.ResolvedClass.Toolspecs {
						eff = append(eff, tr.Name)
					}
					s.Status.EffectiveToolspecs = eff
				}
				if len(s.Status.EffectiveToolspecs) > 0 {
					dirty = true
				}
			}

			// Install the driver onto fakeExec for the codey bundle's
			// pod key BEFORE flipping Ready=True. See p2_interactive_
			// happy_path's helper docstring for the race rationale.
			if s.Labels["agentprimitives.authzed.com/agentbundle"] == "codey" &&
				s.Status.PodName != "" && !installedFor[s.Name] {
				key := s.Namespace + "/" + s.Status.PodName + ":sandbox"
				h.FakeExec().ProgramStreamFunc(key, driver)
				installedFor[s.Name] = true
			}

			if !meta.IsStatusConditionTrue(s.Status.Conditions, spiceboxv1alpha1.SpiceboxSessionConditionReady) {
				s.Status.Conditions = append(s.Status.Conditions, metav1.Condition{
					Type:               spiceboxv1alpha1.SpiceboxSessionConditionReady,
					Status:             metav1.ConditionTrue,
					Reason:             spiceboxv1alpha1.ReasonPodReady,
					Message:            "stamped Ready by e2e harness helper",
					LastTransitionTime: metav1.Now(),
				})
				dirty = true
			}

			if !dirty {
				continue
			}
			if err := c.Status().Update(ctx, s); err != nil {
				if ctx.Err() != nil {
					return
				}
				t.Logf("stampBundleSessionsReady: update %s/%s: %v",
					s.Namespace, s.Name, err)
			}
		}
	}
}
