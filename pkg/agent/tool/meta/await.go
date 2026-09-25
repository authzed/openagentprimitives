package meta

import (
	"context"
	"encoding/json"
	"time"

	"k8s.io/utils/clock"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/authz"
)

// AwaitConfig wires await_user_message to the runner's NATS subscription
// and the configured idle TTL.
type AwaitConfig struct {
	// IdleTTL is how long we wait for an inbound message before returning a
	// terminal IdleExit result. Zero means "exit Idle immediately."
	IdleTTL time.Duration

	// InboundCh receives a struct{} per inbound NATS message. The runner's
	// NATS subscription handler writes here. The channel SHOULD be buffered
	// (the runner buffers ≥1) so a wake-up posted while the agent is between
	// turns is not lost.
	InboundCh <-chan struct{}

	// PresenceCh receives a struct{} per ui_presence heartbeat: someone is
	// looking at this session's agent-defined UI right now. Each one RESTARTS
	// the idle timer rather than waking the loop — nobody has said anything,
	// so there is no turn to take and no resume to signal.
	//
	// It exists because a declared data binding is answered with no conversation
	// at all, so a viewer can be actively driving a dashboard while the loop sees
	// a silent session and exits underneath them — after which every binding on
	// that open dashboard times out for want of a subscriber.
	//
	// Whether a heartbeat is sent at all is decided in the browser, which
	// publishes only while the tab is visible, focused and recently interacted
	// with. Nothing here interprets those conditions; this end knows only "a
	// viewer is still there". Nil (kubectl-driven sessions, tests) means no
	// extensions.
	//
	// A LIVENESS hint, never authorization: it cannot make the loop do anything
	// except wait longer.
	PresenceCh <-chan struct{}

	// OnYield is invoked exactly once, immediately before the tool blocks,
	// to signal the runner has yielded to the user. OnResume is invoked only
	// when InboundCh wakes the tool (a real user reply), NOT on TTL/ctx exit.
	// Both may be nil (kubectl-driven sessions / tests).
	OnYield  func(ctx context.Context)
	OnResume func(ctx context.Context)

	// Clock is injectable for deterministic tests. Nil → clock.RealClock{}.
	Clock clock.Clock
}

// NewAwait constructs the await_user_message meta-tool.
func NewAwait(cfg AwaitConfig) tool.Tool {
	return &awaitTool{cfg: cfg}
}

type awaitTool struct{ cfg AwaitConfig }

func (t *awaitTool) Name() string    { return "await_user_message" }
func (t *awaitTool) Kind() tool.Kind { return tool.KindMeta }
func (*awaitTool) Permission() authz.Permission {
	// await_user_message blocks the runner loop until the user sends a
	// message or the idle TTL expires. It touches no external resource.
	return authz.Permission{StateImpact: authz.Stateless}
}

// PermissionVariants returns nil — meta tools have no conditional
// variants today (only MCP-tooled AgentClasses use them).
func (*awaitTool) PermissionVariants() []authz.PermissionVariant { return nil }

func (t *awaitTool) Description() string {
	return "Yield control back to the user. The session pauses until a new message arrives on the channel. " +
		"Call this after respond_to_user when you've completed your reply and are waiting for the user's next input. " +
		"If no input arrives within the configured idle TTL, the session enters phase=Idle (the runner exits cleanly; " +
		"the next user message respawns it)."
}

func (t *awaitTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)
}

func (t *awaitTool) Execute(ctx context.Context, _ json.RawMessage, _ *tool.SessionContext) (tool.Result, error) {
	switch yieldAndWait(ctx, t.cfg) {
	case awaitDisabled:
		return tool.Result{
			Content:  "idle ttl disabled — exiting cleanly to phase=Idle",
			Terminal: true, IdleExit: true,
			Trusted: true,
		}, nil
	case awaitResumed:
		// A bare acknowledgment, and deliberately NOT an instruction to read
		// memory. await_user_message is purely a yield signal: Loop.drainInbox
		// promotes the newly-arrived inbox turn into a real "user" turn spliced
		// in right after this result, so the model already has the message.
		// Telling it to go look would make a missed drain catastrophic — the
		// model obeys literally and dumps the whole transcript via query_memory
		// until the token budget dies.
		return tool.Result{
			Content:      "acknowledged",
			Terminal:     false,
			AwaitResumed: true,
			Trusted:      true,
		}, nil
	case awaitTTL:
		return tool.Result{Content: "idle ttl expired", Terminal: true, IdleExit: true, Trusted: true}, nil
	default: // awaitCanceled
		return tool.Result{Content: "session canceled (SIGTERM); exiting cleanly to phase=Idle", Terminal: true, IdleExit: true, Trusted: true}, nil
	}
}

// awaitOutcome is why a yield ended. Every blocking meta tool that parks the
// session maps these onto its OWN result text — the outcomes are shared
// because the lifecycle effects are (a resume drains the inbox, a TTL or a
// cancellation yields to phase=Idle), while the words the model reads are
// each tool's own.
type awaitOutcome int

const (
	// awaitResumed: an inbound message arrived and OnResume has already run.
	awaitResumed awaitOutcome = iota
	// awaitTTL: the idle TTL expired with nothing arriving.
	awaitTTL
	// awaitCanceled: the run context ended (SIGTERM) while parked.
	awaitCanceled
	// awaitDisabled: IdleTTL <= 0, so there is nothing to wait ON — the yield
	// still happened, and the caller exits to Idle immediately.
	awaitDisabled
)

// yieldAndWait performs the yield half of a blocking meta tool: it signals the
// yield (OnYield), parks until something ends the wait, and signals the resume
// (OnResume) on — and only on — a real inbound message.
//
// Shared by await_user_message and ask_parent because the two park for the
// same reasons and must have the SAME lifecycle effects: the run clock has to
// stop, the silence watchdog has to disarm, and accrued run-time has to be
// flushed before a pod that may be reaped mid-wait goes quiet. Those all hang
// off OnYield/OnResume, so a second hand-rolled wait loop would be a second
// place for one of them to be forgotten.
func yieldAndWait(ctx context.Context, cfg AwaitConfig) awaitOutcome {
	if cfg.OnYield != nil {
		cfg.OnYield(ctx)
	}
	if cfg.IdleTTL <= 0 {
		return awaitDisabled
	}
	clk := cfg.Clock
	if clk == nil {
		clk = clock.RealClock{}
	}
	timer := clk.NewTimer(cfg.IdleTTL)
	defer timer.Stop()
	// A LOOP, not a bare select, so a presence heartbeat can restart the timer
	// and go back to waiting. Every other arm still returns on its first
	// firing — the loop exists solely for the arm that means "keep waiting".
	for {
		select {
		case <-cfg.PresenceCh:
			// Someone is watching. Restart the idle clock and keep waiting;
			// do NOT call OnResume — no user has spoken, so signalling a
			// resume would report a turn that is not happening.
			//
			// A fresh timer rather than Reset on the existing one: Reset's
			// contract requires the timer to be stopped and drained first,
			// which is exactly the sequence that races a fire already in
			// flight and is easy to get subtly wrong. Allocating one per
			// heartbeat costs nothing at this rate.
			timer.Stop()
			timer = clk.NewTimer(cfg.IdleTTL)
			continue
		case <-cfg.InboundCh:
			if cfg.OnResume != nil {
				cfg.OnResume(ctx)
			}
			return awaitResumed
		case <-timer.C():
			return awaitTTL
		case <-ctx.Done():
			return awaitCanceled
		}
	}
}
