package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/guardian/approval"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/uiaction"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// uiActionApprovalTimeout is the fixture's approval-WAIT deadline. It is
// shared by the "lapsed approval" case (which deliberately never delivers a
// decision and needs this to elapse quickly) and the approve/deny cases
// (whose watcher goroutine delivers within single-digit milliseconds of the
// approval becoming pending, leaving ample margin before this fires).
const uiActionApprovalTimeout = 150 * time.Millisecond

// syncBuffer is a concurrency-safe io.Writer wrapping bytes.Buffer, used as
// the fixture's swapped-in slog handler sink so a detached goroutine's log
// write can never race the test goroutine's read.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// uiActionMemFake is a minimal memory.Memory standing in for a real backend
// in the lifecycle-recorder fixture. Query/Search/SendSignal are unused by
// anything this fixture exercises and return zero values. Put captures every
// entry verbatim (records() below filters to the uiaction Kind) and enforces
// the two properties the mutation tests depend on:
//
//   - it refuses a write against an already-expired context, the way any
//     real network-backed store would (this is what makes the settle
//     WithoutCancel mutation observable); and
//   - forceFail, when set, fails EVERY write unconditionally (for the
//     write-before-push ordering test).
type uiActionMemFake struct {
	forceFail bool

	mu      sync.Mutex
	entries []memory.Entry
}

func (m *uiActionMemFake) Put(ctx context.Context, e memory.Entry) (memory.Entry, error) {
	if err := ctx.Err(); err != nil {
		return memory.Entry{}, fmt.Errorf("uiActionMemFake: put on expired context: %w", err)
	}
	if m.forceFail {
		return memory.Entry{}, errors.New("uiActionMemFake: forced write failure")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.entries = append(m.entries, e)
	return e, nil
}

func (m *uiActionMemFake) Query(context.Context, memory.Query) (memory.QueryResult, error) {
	return memory.QueryResult{}, nil
}

func (m *uiActionMemFake) Search(context.Context, memory.SearchRequest) (memory.MergedSearchResult, error) {
	return memory.MergedSearchResult{}, nil
}

func (m *uiActionMemFake) SendSignal(context.Context, memory.Signal) error { return nil }

func (m *uiActionMemFake) records(t *testing.T) []uiaction.Content {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]uiaction.Content, 0, len(m.entries))
	for _, e := range m.entries {
		if e.Kind != uiaction.KindName {
			continue
		}
		var c uiaction.Content
		require.NoError(t, json.Unmarshal(e.Content, &c))
		out = append(out, c)
	}
	return out
}

// uiActionRig is the recorder-inspecting handle newUIActionFixture returns
// alongside its Loop: every ui_action memory write (mem), every
// ui_action_update push (pushedPayloads), and every log line the fixture's
// swapped-in slog default captured (logs) — the three surfaces the mutation
// tests in TestUIActionLifecycleRecording each isolate.
type uiActionRig struct {
	mem  *uiActionMemFake
	logs *syncBuffer

	mu             sync.Mutex
	pushedPayloads []channelevents.UIActionUpdatePayload
}

func (r *uiActionRig) recordPush(env channelevents.Envelope) {
	var pl channelevents.UIActionUpdatePayload
	if err := json.Unmarshal(env.Payload, &pl); err != nil {
		return // a malformed push means the assertion on its shape fails loudly instead
	}
	r.mu.Lock()
	r.pushedPayloads = append(r.pushedPayloads, pl)
	r.mu.Unlock()
}

func (r *uiActionRig) records(t *testing.T) []uiaction.Content {
	t.Helper()
	return r.mem.records(t)
}

func (r *uiActionRig) states(t *testing.T) []uiaction.State {
	t.Helper()
	recs := r.records(t)
	out := make([]uiaction.State, 0, len(recs))
	for _, c := range recs {
		out = append(out, c.State)
	}
	return out
}

func (r *uiActionRig) latest(t *testing.T) uiaction.Content {
	t.Helper()
	recs := r.records(t)
	require.NotEmpty(t, recs, "no ui_action record was ever written")
	return recs[len(recs)-1]
}

func (r *uiActionRig) recordInState(t *testing.T, state uiaction.State) uiaction.Content {
	t.Helper()
	for _, c := range r.records(t) {
		if c.State == state {
			return c
		}
	}
	t.Fatalf("no ui_action record observed in state %q", state)
	return uiaction.Content{}
}

func (r *uiActionRig) pushed(t *testing.T) []channelevents.UIActionUpdatePayload {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]channelevents.UIActionUpdatePayload(nil), r.pushedPayloads...)
}

func (r *uiActionRig) pushedInState(t *testing.T, state uiaction.State) channelevents.UIActionUpdatePayload {
	t.Helper()
	for _, p := range r.pushed(t) {
		if p.State == string(state) {
			return p
		}
	}
	t.Fatalf("no ui_action_update push observed in state %q", state)
	return channelevents.UIActionUpdatePayload{}
}

func (r *uiActionRig) logLines(t *testing.T) []string {
	t.Helper()
	raw := strings.TrimSpace(r.logs.String())
	if raw == "" {
		return nil
	}
	return strings.Split(raw, "\n")
}

// awaitTerminal polls until the most recently written ui_action record has
// reached a terminal state. The detached side-effecting exec settles on its
// own goroutine, so a bare read immediately after HandleUIAction returns
// would race it.
func (r *uiActionRig) awaitTerminal(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		recs := r.mem.records(t)
		if len(recs) > 0 && uiaction.IsTerminal(recs[len(recs)-1].State) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for a terminal ui_action state; records so far: %+v", recs)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// uiActionApprovalHook is a minimal PreToolCall hook standing in for the
// production ToolCallAuthz hook (pkg/authz/hooks), which this fixture
// deliberately doesn't wire (it needs a real SpiceDB engine). It fires the
// exact pipeline.ApprovalAsk shape the executor needs to drive
// PublishApproval/AwaitDecision for ONE named tool, so the lifecycle
// recorder's Asked/Resolved observation can be exercised end-to-end without
// standing up authz.
type uiActionApprovalHook struct{ toolName string }

func (h *uiActionApprovalHook) Name() string { return "ui_action_test_approval" }
func (h *uiActionApprovalHook) Points() []pipeline.Point {
	return []pipeline.Point{pipeline.PreToolCall}
}
func (h *uiActionApprovalHook) Eval(_ context.Context, in pipeline.Input) pipeline.Decision {
	if in.Tool == nil || in.Tool.Name != h.toolName {
		return pipeline.Decision{}
	}
	pl := ToolCallApprovalPayload{
		SessNS:     in.Session.Namespace,
		SessName:   in.Session.Name,
		ToolName:   in.Tool.Name,
		Permission: "write",
		// External short-circuits toolCallPostApprove's SessionGrantCheck
		// re-verify (host_approval.go's toolCallPostApprove) — this fixture
		// exercises the approval OBSERVATION seam, not the post-approval grant
		// re-check, so it doesn't stand up a SpiceDB engine.
		StateImpact:   string(authz.External),
		Perm:          authz.Permission{StateImpact: authz.External},
		Justification: "test",
		UseID:         in.Tool.UseID,
		Subject:       in.Requester.String(),
		Subjects:      in.Subjects,
	}
	return pipeline.Decision{Approval: &pipeline.ApprovalAsk{Kind: "tool_call", Summary: "approve test action?", Payload: pl.ToMap()}}
}

// newUIActionFixture builds a Loop wired for the lifecycle recorder's full
// approval flow: a real memory.Memory (uiActionMemFake), a real
// approval.Orchestrator + resolvable approver set, a short approval-WAIT
// timeout, and (for a side-effecting fixture) a background watcher that
// delivers the requested decision once the approval is pending. It builds on
// newAppToolFixture's appToolFixture shape (Task 5) rather than duplicating
// it, extending it with the fields newAppToolFixture itself ignores.
func newUIActionFixture(t *testing.T, f appToolFixture) (*Loop, InteractChecker, context.Context, *uiActionRig) {
	t.Helper()
	const ns, name = "demo-ns", "demo-session"

	toolName := "demo_readonly"
	perm := authz.Permission{StateImpact: authz.Readonly}
	if !f.readonly {
		toolName = "demo_mutating"
		perm = authz.Permission{StateImpact: authz.Readwrite}
	}
	resultText := "body"
	if f.resultText != "" {
		resultText = f.resultText
	}
	ft := &fakeAppTool{name: toolName, perm: perm, roHint: true, result: tool.Result{Content: resultText}}

	memFake := &uiActionMemFake{forceFail: f.memoryWriteFails}
	logs := &syncBuffer{}
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prevLogger) })

	rig := &uiActionRig{mem: memFake, logs: logs}

	approverSubs := []string{"user:someone-else"}
	if f.approverIsRequester {
		approverSubs = []string{"user:viewer"} // matches uiActionEnvelope's fixed Requester
	}

	l := &Loop{
		SessionKey: memory.NamespacedName{Namespace: ns, Name: name},
		AppTools:   map[string]tool.Tool{toolName: ft},
		Mem:        memFake,
		Approval:   approval.New(),
		SpiceDBLookupSubjects: lookupBy(map[string][]string{
			"agentsession:" + ns + "/" + name + "#approve": approverSubs,
		}),
		InteractionRequestPublish: func(context.Context, string, string, channelevents.Envelope) error {
			return nil
		},
		AgentClass: &spiceboxv1alpha1.AgentClass{
			Spec: spiceboxv1alpha1.AgentClassSpec{
				Authz: &spiceboxv1alpha1.AuthzBlock{
					ApprovalTimeout: &metav1.Duration{Duration: uiActionApprovalTimeout},
				},
			},
		},
	}
	if !f.noPublisher {
		l.UIPublish = func(_ context.Context, _, _ string, env channelevents.Envelope) error {
			rig.recordPush(env)
			return nil
		}
	}

	reg := pipeline.NewRegistry()
	if !f.readonly {
		reg.Register(&uiActionApprovalHook{toolName: toolName}, 10)
	}
	loopWithInjectedExecutor(t, l, reg)

	if !f.readonly && !f.approvalTimesOut {
		sessionRef := ns + "/" + name
		approve := f.approve
		go func() {
			deadline := time.Now().Add(2 * time.Second)
			for l.Approval.PendingForSession(sessionRef) == 0 {
				if time.Now().After(deadline) {
					return // best-effort; awaitTerminal's own deadline surfaces a stuck test
				}
				time.Sleep(time.Millisecond)
			}
			for _, id := range l.Approval.PendingRequestIDsForSession(sessionRef) {
				l.Approval.DeliverDecision(id, approval.Decision{Approved: approve, ApproverID: "tester"})
			}
		}()
	}

	ia := &fakeInteract{allow: true}
	execCtx := memory.WithSystemApproval(context.Background(), "test")
	return l, ia, execCtx, rig
}

func TestUIActionLifecycleRecording(t *testing.T) {
	const ns, name = "demo-ns", "demo-session"

	t.Run("a readonly action records exactly one terminal state", func(t *testing.T) {
		l, ia, execCtx, rec := newUIActionFixture(t, appToolFixture{readonly: true})
		_ = l.HandleUIAction(execCtx, ia, ns, name, uiActionEnvelope(t, "refresh", "demo_readonly", `{}`))

		got := rec.records(t)
		require.Len(t, got, 1, "a synchronous action's whole duration IS the request; one record is honest")
		assert.Equal(t, uiaction.StateSucceeded, got[0].State)
		assert.Equal(t, "refresh", got[0].Action, "the record is keyed by the DECLARED name, not the tool")
	})

	t.Run("a side-effecting action records submitted -> awaiting_approval -> running -> succeeded", func(t *testing.T) {
		l, ia, execCtx, rec := newUIActionFixture(t, appToolFixture{readonly: false, approve: true})
		_ = l.HandleUIAction(execCtx, ia, ns, name, uiActionEnvelope(t, "advance", "demo_mutating", `{}`))
		rec.awaitTerminal(t)

		assert.Equal(t, []uiaction.State{
			uiaction.StateSubmitted, uiaction.StateAwaitingApproval,
			uiaction.StateRunning, uiaction.StateSucceeded,
		}, rec.states(t))
	})

	// THE gap-1 test. Without settle-on-a-WithoutCancel-context this is the
	// case that fails, and it is the case the whole plan exists for.
	t.Run("a DETACHED terminal outcome lands in the record after the POST already returned", func(t *testing.T) {
		l, ia, execCtx, rec := newUIActionFixture(t, appToolFixture{readonly: false, approve: true})
		resp := l.HandleUIAction(execCtx, ia, ns, name, uiActionEnvelope(t, "advance", "demo_mutating", `{}`))
		require.Equal(t, string(uiaction.StateSubmitted), resp.State,
			"the synchronous answer cannot carry the outcome — that is the gap")

		rec.awaitTerminal(t)
		final := rec.latest(t)
		assert.Equal(t, uiaction.StateSucceeded, final.State)
		assert.Equal(t, resp.RequestID, final.RequestID, "the record is addressable by the id the browser holds")
	})

	t.Run("a LAPSED approval settles as expired, not denied, on an already-cancelled context", func(t *testing.T) {
		l, ia, execCtx, rec := newUIActionFixture(t, appToolFixture{readonly: false, approvalTimesOut: true})
		_ = l.HandleUIAction(execCtx, ia, ns, name, uiActionEnvelope(t, "advance", "demo_mutating", `{}`))
		rec.awaitTerminal(t)
		assert.Equal(t, uiaction.StateExpired, rec.latest(t).State,
			"the spec re-enables the control on expired and does not on denied; they cannot be merged")
	})

	t.Run("a denied approval settles as denied", func(t *testing.T) {
		l, ia, execCtx, rec := newUIActionFixture(t, appToolFixture{readonly: false, approve: false})
		_ = l.HandleUIAction(execCtx, ia, ns, name, uiActionEnvelope(t, "advance", "demo_mutating", `{}`))
		rec.awaitTerminal(t)
		assert.Equal(t, uiaction.StateDenied, rec.latest(t).State)
	})

	t.Run("addressedToViewer rides the awaiting_approval record and its push", func(t *testing.T) {
		l, ia, execCtx, rec := newUIActionFixture(t, appToolFixture{
			readonly: false, approve: true, approverIsRequester: true})
		_ = l.HandleUIAction(execCtx, ia, ns, name, uiActionEnvelope(t, "advance", "demo_mutating", `{}`))
		rec.awaitTerminal(t)

		awaiting := rec.recordInState(t, uiaction.StateAwaitingApproval)
		assert.True(t, awaiting.ApprovalAddressedToViewer)

		pushed := rec.pushedInState(t, uiaction.StateAwaitingApproval)
		assert.True(t, pushed.ApprovalAddressedToViewer,
			"the browser learns this ONLY from the push or the record; dropping it here is where chrome stops revealing")
	})

	t.Run("memory is written BEFORE the push, so live and reconnect cannot diverge", func(t *testing.T) {
		l, ia, execCtx, rec := newUIActionFixture(t, appToolFixture{readonly: true, memoryWriteFails: true})
		_ = l.HandleUIAction(execCtx, ia, ns, name, uiActionEnvelope(t, "refresh", "demo_readonly", `{}`))
		assert.Empty(t, rec.pushed(t),
			"a state no reconnecting browser can ever read must not be pushed to a live one")
		assert.NotEmpty(t, rec.logLines(t), "and the write failure must be logged, never swallowed")
	})

	t.Run("a nil UIPublish degrades to memory-only without panicking", func(t *testing.T) {
		l, ia, execCtx, rec := newUIActionFixture(t, appToolFixture{readonly: true, noPublisher: true})
		_ = l.HandleUIAction(execCtx, ia, ns, name, uiActionEnvelope(t, "refresh", "demo_readonly", `{}`))
		assert.Len(t, rec.records(t), 1)
	})

	// Every rejection inside the shared core must settle the record, not just
	// answer the POST. The synchronous response is the transient half; the
	// record is the durable half a reload and an `action:` data binding read.
	// The not-found and rate-limited paths already settled; the two
	// interact-denial paths did not, so an action denied at re-authorization
	// answered "denied" once and then resolved to the no-record copy forever.
	t.Run("an interact DENIAL settles the record, like every other rejection", func(t *testing.T) {
		l, _, execCtx, rec := newUIActionFixture(t, appToolFixture{readonly: true})
		resp := l.HandleUIAction(execCtx, &fakeInteract{allow: false}, ns, name,
			uiActionEnvelope(t, "refresh", "demo_readonly", `{}`))
		require.Equal(t, string(uiaction.StateDenied), resp.State)

		got := rec.records(t)
		require.Len(t, got, 1, "a denial nobody records is a control that stays blank on the next reload")
		assert.Equal(t, uiaction.StateDenied, got[0].State)
		assert.Equal(t, "refresh", got[0].Action)
	})

	t.Run("an interact ERROR settles the record too — indeterminate is still an outcome", func(t *testing.T) {
		l, _, execCtx, rec := newUIActionFixture(t, appToolFixture{readonly: true})
		resp := l.HandleUIAction(execCtx, &fakeInteract{err: errors.New("spicedb down")}, ns, name,
			uiActionEnvelope(t, "refresh", "demo_readonly", `{}`))
		require.Equal(t, string(uiaction.StateDenied), resp.State, "an error fails CLOSED, never open")
		require.Len(t, rec.records(t), 1)
	})

	t.Run("a nil interact checker settles the record — a wiring bug must not leave a blank lifecycle", func(t *testing.T) {
		l, _, execCtx, rec := newUIActionFixture(t, appToolFixture{readonly: true})
		resp := l.HandleUIAction(execCtx, nil, ns, name, uiActionEnvelope(t, "refresh", "demo_readonly", `{}`))
		require.Equal(t, string(uiaction.StateDenied), resp.State)
		require.Len(t, rec.records(t), 1)
	})

	// Malformed req.Args used to slip through unvalidated until step (6)'s
	// envelope build, where json.Marshal's RawMessage compaction rejected it
	// — a defensive arm no real wire input could ever reach, since an
	// envelope that already parsed guarantees req.Args is syntactically
	// valid JSON by the time it gets here.
	//
	// ResolvePermissionForArgs now parses req.Args at step (4), earlier, to
	// resolve the permission that governs auto-run — so the SAME `{not
	// json` input denies there instead, and the old step-(6) marshal-failure
	// arm is now unreachable through anything that can reach step (6) at
	// all: every Args value step (4) accepts is, by construction, already
	// valid JSON. What this test still pins is that the denial logs and
	// settles the record rather than returning a bare response — the
	// property the old test protected, on the new path that now owns it.
	t.Run("malformed args deny at the permission-resolution gate, logged and settled", func(t *testing.T) {
		l, ia, execCtx, rig := newUIActionFixture(t, appToolFixture{readonly: true})
		recorder := newUIActionRecorder(l, ns, name, "req-marshal", "refresh", uiaction.RequesterKey("user:viewer"))

		resp := l.handleAppToolCallReq(execCtx, ia, ns, name, channelevents.AppToolCallRequest{
			ToolName:  "demo_readonly",
			Args:      json.RawMessage(`{not json`),
			Requester: "user:viewer",
			RequestID: "req-marshal",
		}, appToolSurface{uiAction: recorder})

		require.Equal(t, channelevents.AppToolCallStatusDenied, resp.Status)
		assert.NotEmpty(t, resp.ViewerMessage, "the viewer gets human copy, never the Go unmarshal error")
		require.Len(t, rig.records(t), 1)
		assert.Equal(t, uiaction.StateDenied, rig.latest(t).State)
		assert.NotEmpty(t, rig.logLines(t), "the cause must be logged, never swallowed")
	})

	t.Run("the record never contains the raw tool result", func(t *testing.T) {
		l, ia, execCtx, rec := newUIActionFixture(t, appToolFixture{readonly: true, resultText: "SECRET-PAYLOAD"})
		_ = l.HandleUIAction(execCtx, ia, ns, name, uiActionEnvelope(t, "refresh", "demo_readonly", `{}`))
		raw, err := json.Marshal(rec.latest(t))
		require.NoError(t, err)
		assert.NotContains(t, string(raw), "SECRET-PAYLOAD",
			"the UI re-reads changed data through its bindings; the record is a lifecycle, not a data path")
	})
}
