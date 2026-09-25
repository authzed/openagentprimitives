package runner

// preferences_deps.go implements the runner-side meta.PreferencesReader and
// meta.PreferenceSaver (contract types declared in pkg/agent/tool/meta) that
// Task 13's preference tools depend on.
//
// preferencesSaver deliberately NEVER writes memory: it publishes a
// preference_save ask through the host, blocks for the decision, and reports
// the outcome. The commit itself is channelsd's
// (pkg/channels/channelsd/pipeline/preference_commit.go), which runs only
// after the SAME DecideRequester standing check host_approval.go's
// buildPreferenceSavePending addressed the card to. Calling
// memory.CommitPreference (or any write) from here would let the runner
// commit a preference the addressee never actually confirmed, bypassing that
// check entirely.

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta"
	"github.com/authzed/openagentprimitives/pkg/memory/httpclient"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
	"github.com/authzed/openagentprimitives/pkg/platform/preferences"
)

// defaultPreferenceSaveTimeout is used only when neither an explicit
// preferencesSaver.timeout nor a bound host that can resolve the session's
// own tiered approval timeout (*runnerHost.ResolveTimeout, in production) is
// available. It matches resolvedApprovalTimeout's own fallback and
// contentInspectionTTL's, so an un-resolvable session still waits a sane
// window rather than 0.
const defaultPreferenceSaveTimeout = 10 * time.Minute

// preferencesReader implements meta.PreferencesReader over the operator's
// memory HTTP client, scoped to a turn index the loop tracks.
type preferencesReader struct {
	client    *httpclient.Client
	ns, name  string
	turnIndex func() int // -1 when unknown; captures Loop.CurrentUserTurnIndex
}

// NewPreferencesReader constructs the runner's meta.PreferencesReader.
// turnIndex is captured, not called eagerly, so a read always reflects the
// CURRENT turn at call time rather than whatever turn was current when the
// tool table was assembled (assembly happens once per session, long before
// most turns run).
func NewPreferencesReader(client *httpclient.Client, ns, name string, turnIndex func() int) meta.PreferencesReader {
	return &preferencesReader{client: client, ns: ns, name: name, turnIndex: turnIndex}
}

// Current implements meta.PreferencesReader.
func (r *preferencesReader) Current(ctx context.Context) (preferences.SnapshotResponse, error) {
	idx := -1
	if r.turnIndex != nil {
		idx = r.turnIndex()
	}
	return r.client.GetPreferences(ctx, r.ns, r.name, idx)
}

// ForRef implements meta.PreferencesReader: a thin forward to the operator's
// ?user-ref= route. No turn index — that query param is the CURRENT-author
// mode's own, mutually exclusive with ?user-ref= server-side.
func (r *preferencesReader) ForRef(ctx context.Context, ref string) (preferences.SnapshotResponse, error) {
	return r.client.GetPreferencesForUserRef(ctx, r.ns, r.name, ref)
}

// approvalHost is the narrow slice of runnerHost a preferencesSaver needs:
// publish the ask, then block for the decision. Satisfied directly by
// *runnerHost in production and by a small fake in tests, so Save is
// unit-testable without a real Loop/AgentSession/orchestrator.
type approvalHost interface {
	PublishApproval(ctx context.Context, ask pipeline.ApprovalAsk) (string, error)
	AwaitDecision(ctx context.Context, reqID string, timeout time.Duration) (bool, string, bool, error)
}

// hostBinder yields the ONE approvalHost an entire Save round-trip runs
// against. This is an interface, not a plain field, only because production
// needs late binding (the tool table is assembled before the Loop exists —
// see lateBoundHost); the contract that actually matters is the ONE:
// pendingApprovals lives on the concrete *runnerHost value
// (host_approval.go's one-host-per-dispatch invariant), so PublishApproval
// and AwaitDecision MUST be called on the SAME host. A binder that handed
// out a fresh host per method call stranded every pending entry — every
// production Save failed with "host: no pending approval" before the card
// was ever published, which is the bug this seam's shape now makes
// unrepresentable: Save binds once, then holds the result.
type hostBinder interface {
	BindHost() (approvalHost, error)
}

// staticHost is the hostBinder for a host that already exists (the test
// fakes; any future caller that holds a live *runnerHost). BindHost returns
// the same value every time, which trivially satisfies the one-host
// contract.
type staticHost struct{ h approvalHost }

func (s staticHost) BindHost() (approvalHost, error) {
	if s.h == nil {
		return nil, fmt.Errorf("preference save unavailable (session not ready)")
	}
	return s.h, nil
}

// timeoutResolver is an OPTIONAL capability the BOUND approvalHost may
// additionally satisfy: the consolidated, tiered approval timeout every
// other host-driven approval kind waits on (Loop.resolvedApprovalTimeout —
// "one window, one source", per contentInspectionTTL's doc). preferencesSaver
// uses it when present so preference_save rides the same tier a
// cluster/class/session tightened, rather than a private constant;
// *runnerHost implements it (ResolveTimeout below), and a test fake need
// not, in which case defaultPreferenceSaveTimeout is used instead.
type timeoutResolver interface {
	ResolveTimeout() time.Duration
}

// ResolveTimeout implements timeoutResolver on the concrete host: the same
// consolidated 4-tier approval window every other kind in host_approval.go
// stamps into its ExpiresAt. Guarded like contentInspectionTTL, so a
// degenerate resolution can never produce a zero-length await.
func (h *runnerHost) ResolveTimeout() time.Duration {
	ttl := h.l.resolvedApprovalTimeout()
	if ttl <= 0 {
		return defaultPreferenceSaveTimeout
	}
	return ttl
}

// preferencesSaver implements meta.PreferenceSaver by binding ONE
// approvalHost per Save and publishing/awaiting through it. It never
// writes — see this file's package doc.
type preferencesSaver struct {
	binder  hostBinder
	timeout time.Duration // resolved via the bound host's timeoutResolver, else defaultPreferenceSaveTimeout, when <= 0
}

// NewPreferenceSaver constructs the runner's meta.PreferenceSaver over an
// already-live host. timeout <= 0 defers to the host's timeoutResolver (if
// it implements one) or defaultPreferenceSaveTimeout.
func NewPreferenceSaver(host approvalHost, timeout time.Duration) meta.PreferenceSaver {
	return &preferencesSaver{binder: staticHost{h: host}, timeout: timeout}
}

// Save implements meta.PreferenceSaver: bind the ONE host this round-trip
// runs against, publish the confirm addressed to the current turn author
// (host_approval.go's buildPreferenceSavePending does the addressing), then
// block for the decision ON THAT SAME HOST — pendingApprovals lives on the
// host value, so splitting the two calls across hosts loses the pending
// entry (see hostBinder's doc for the production failure that pinned this).
// Save itself never writes; on approve, channelsd has already committed the
// preference by the time AwaitDecision returns.
func (s *preferencesSaver) Save(ctx context.Context, key string, value *apiextv1.JSON, display string) (meta.SaveOutcome, error) {
	if s.binder == nil {
		return meta.SaveOutcome{}, fmt.Errorf("preference save unavailable (session not ready)")
	}
	host, err := s.binder.BindHost()
	if err != nil {
		return meta.SaveOutcome{}, err
	}

	// Wire contract (pinned by Task 11's channelsd decoder,
	// preferenceConfirmDetails): "key" and "display" always present; "value"
	// OMITTED entirely (never a JSON null) when clearing, so a clear and an
	// explicit null are distinguishable downstream.
	payload := map[string]any{"key": key, "display": display}
	if value != nil {
		payload["value"] = json.RawMessage(value.Raw)
	}

	reqID, err := host.PublishApproval(ctx, pipeline.ApprovalAsk{
		Kind:    "preference_save",
		Summary: display,
		Payload: payload,
	})
	if err != nil {
		return meta.SaveOutcome{}, err
	}

	timeout := s.timeout
	if timeout <= 0 {
		if tr, ok := host.(timeoutResolver); ok {
			timeout = tr.ResolveTimeout()
		} else {
			timeout = defaultPreferenceSaveTimeout
		}
	}

	approved, by, timedOut, err := host.AwaitDecision(ctx, reqID, timeout)
	if err != nil {
		return meta.SaveOutcome{}, err
	}
	return meta.SaveOutcome{
		Approved:  approved,
		Denied:    !approved && !timedOut,
		TimedOut:  timedOut,
		DecidedBy: by,
	}, nil
}

// lateBoundHost is the production hostBinder: it defers *runnerHost
// construction to Save time, because of a real ordering constraint in
// internal/cmd/runner/main.go — the tool table (and therefore
// capability.RunnerEnv.PreferenceSaver) is assembled BEFORE the session's
// Loop is built, so nothing host-shaped can exist yet at wiring time. loopFn
// is the SAME late-binding idiom main.go already uses for
// LeakageGate/PinAttachment/ViewerCanInteract et al. (an
// atomic.Pointer[Loop] stored once the Loop exists, read at call time —
// well after that point, since Save only ever runs from a live tool call).
//
// It deliberately does NOT implement approvalHost itself: exposing
// PublishApproval/AwaitDecision that each resolved their own fresh
// *runnerHost is exactly the pending-entry-stranding bug hostBinder's doc
// describes. The only thing a caller can do with a lateBoundHost is bind —
// once — and use the result.
type lateBoundHost struct {
	loopFn func() *Loop
}

// BindHost implements hostBinder: build the ONE real *runnerHost this Save
// will run against, or refuse while the Loop is not yet stored.
func (h *lateBoundHost) BindHost() (approvalHost, error) {
	l := h.loopFn()
	if l == nil {
		return nil, fmt.Errorf("preference save unavailable (session not ready)")
	}
	return newRunnerHost(l, hostSession{Namespace: l.SessionKey.Namespace, Name: l.SessionKey.Name, Class: l.AgentName}), nil
}

// NewLateBoundPreferenceSaver constructs the production meta.PreferenceSaver:
// a preferencesSaver over a lateBoundHost, so internal/cmd/runner can wire it
// into capability.RunnerEnv before the session's Loop exists. loopFn is the
// atomic-pointer accessor main.go already threads through
// LeakageGate/PinAttachment/etc. (e.g. `func() *runner.Loop { return
// loopRef.Load() }`). The bound *runnerHost's own ResolveTimeout supplies
// the tiered approval window (timeout 0 here).
func NewLateBoundPreferenceSaver(loopFn func() *Loop) meta.PreferenceSaver {
	return &preferencesSaver{binder: &lateBoundHost{loopFn: loopFn}}
}
