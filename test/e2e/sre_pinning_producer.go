//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/pkg/agent/secretout"
	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/relwrites"
)

// srePinningProducerTool is a near-clone of secretOutputProducerTool that ALSO
// carries a writesRelationships pin block + a write-once fast-fail — mirroring
// what a real SRE `get-kubeconfig` SandboxTool does end-to-end
// (pkg/agent/tool/sandbox/sandbox_tool.go):
//
//   - It emits a fake kubeconfig out-of-band under a secretOutput (Content IS
//     the raw value; SecretOutput non-nil ⇒ the runner's applySecretOutput
//     diverts it), so the runner-side capture/scrub path is exercised
//     identically to secretOutputProducerTool.
//   - On a SUCCESSFUL emit it evaluates its writesRelationships pin block via
//     the REAL relwrites.Run + relwrites.SpiceDBWriter — the same Task 4 CEL
//     bindings + SpiceDB write the sandbox tool drives — so the pin tuple lands
//     in the harness's real SpiceDB container.
//   - Before doing anything it runs the Task 6 write-once fast-fail: it re-Gets
//     the AgentSession and refuses a second emit for the same secretOutput name.
//     This runs BEFORE the pin block so a bogus SECOND pin can never be written
//     — the ordering-critical property the e2e locks in.
//
// It exists (rather than a real SandboxTool) for the SAME reason
// secretOutputProducerTool does: the SpiceboxToolspec CRD does not yet mirror
// the toolspec library's secretOutput field, so the CRD-driven sandbox synthesis
// path can't carry a secretOutput end-to-end. The write-once + block-building
// glue mirrored here is unit-covered against the real code in
// pkg/agent/tool/sandbox/sandbox_tool_writeonce_test.go and
// sandbox_tool_relwrites_test.go; this e2e proves the three compose.
//
// StateImpact is Passthrough so the dispatcher runs no SpiceDB Check (the fake
// tool bypasses the ToolCall/sandbox debug-gate path — the cluster#debug gate is
// a separate concern, not what this chain-composition test exercises).
type srePinningProducerTool struct {
	name      string
	value     string
	spec      *secretout.Spec
	writes    []relwrites.Block
	relWriter relwrites.Writer
}

func (s *srePinningProducerTool) Name() string    { return s.name }
func (s *srePinningProducerTool) Kind() tool.Kind { return tool.KindSandbox }
func (s *srePinningProducerTool) Description() string {
	return "Test SRE producer: fetches a fake cluster kubeconfig as a secretOutput and JIT-pins the cluster to this session."
}

// InputSchema declares an argv array (argv[0] the CLI verb, argv[1] the cluster
// slug) plus an optional _reason string, matching the sandbox argv shape the
// writesRelationships CEL binds against (args.argv[1]).
func (s *srePinningProducerTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"argv":{"type":"array","items":{"type":"string"},"description":"argv, e.g. [\"get-kubeconfig\",\"fake-cluster-1\"]"},"_reason":{"type":"string"}}}`)
}

type sreProducerArgs struct {
	Argv   []string `json:"argv"`
	Reason string   `json:"_reason"`
}

func (s *srePinningProducerTool) Execute(ctx context.Context, raw json.RawMessage, sess *tool.SessionContext) (tool.Result, error) {
	var a sreProducerArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return tool.Result{Content: "sre-producer: invalid arguments: " + err.Error(), IsError: true}, nil
	}

	// Write-once fast-fail — mirrors sandbox.SandboxTool.ExecuteWithIDs
	// (sandbox_tool.go): re-Get the AgentSession and refuse a second emit for
	// the same secretOutput name. Ordering-critical: this runs BEFORE the pin
	// block so a bogus SECOND pin is never written. On the second call the first
	// call's runner-side applySecretOutput has already committed
	// status.satisfiedSecretOutputs (loop.go writes it synchronously before the
	// next LLM turn), so the fresh Get here observes it.
	if s.spec != nil {
		c, ok := sess.K8sClient.(client.Client)
		if !ok {
			return tool.Result{}, fmt.Errorf("sre-producer: SessionContext.K8sClient is not a controller-runtime client")
		}
		var current spiceboxv1alpha1.AgentSession
		if err := c.Get(ctx, client.ObjectKey{Namespace: sess.Namespace, Name: sess.Name}, &current); err != nil {
			return tool.Result{
				Content: fmt.Sprintf("sre-producer: secret-output %q: failed to verify write-once status: %v", s.spec.Name, err),
				IsError: true,
			}, nil
		}
		for _, sat := range current.Status.SatisfiedSecretOutputs {
			if sat.Name == s.spec.Name {
				return tool.Result{
					IsError: true,
					Content: fmt.Sprintf("secret-output %q already satisfied for this session (write-once): the session is pinned; start a new session to target a different cluster", s.spec.Name),
				}, nil
			}
		}
	}

	// Emit the secret out-of-band: Content IS the raw value; SecretOutput
	// non-nil tells the runner's applySecretOutput to divert it. Same shape as
	// secretOutputProducerTool + sandbox composeResult's stdout secret path.
	res := tool.Result{Content: s.value, SecretOutput: s.spec}

	// Evaluate the writesRelationships pin block via the REAL relwrites.Run +
	// SpiceDBWriter — mirrors sandbox.SandboxTool.evaluateWritesRelationships.
	// The CEL bindings deliberately exclude the secret value (result carries
	// only {"success": bool}); the pin sees only argv + session.
	s.evaluatePin(ctx, res, a.Argv, sess)
	return res, nil
}

// evaluatePin runs the declared writesRelationships blocks after a successful
// emit, mirroring sandbox.SandboxTool.evaluateWritesRelationships: skipped when
// no blocks/writer are wired or the result is an error; failures are logged
// (never silent) and never change the tool.Result.
func (s *srePinningProducerTool) evaluatePin(ctx context.Context, res tool.Result, argv []string, sess *tool.SessionContext) {
	if len(s.writes) == 0 || s.relWriter == nil || res.IsError {
		return
	}
	vars := map[string]any{
		"args":    map[string]any{"argv": toAnySliceSRE(argv)},
		"result":  map[string]any{"success": !res.IsError},
		"session": sess.Namespace + "/" + sess.Name,
	}
	if _, err := relwrites.Run(ctx, s.relWriter, s.writes, vars, nil, func(msg string, kv ...any) {
		slog.Default().Info(msg, append([]any{"tool", s.name, "session", sess.Name}, kv...)...)
	}); err != nil {
		slog.Default().Info("sre-producer relwrites failed", "tool", s.name, "session", sess.Name, "err", err.Error())
	}
}

func (s *srePinningProducerTool) Permission() authz.Permission {
	return authz.Permission{StateImpact: authz.Passthrough}
}

func (s *srePinningProducerTool) PermissionVariants() []authz.PermissionVariant { return nil }

var _ tool.Tool = (*srePinningProducerTool)(nil)

// toAnySliceSRE converts a []string argv into []any so it satisfies CEL's
// dyn-typed args.argv binding (mirrors sandbox_tool.go's toAnySlice).
func toAnySliceSRE(ss []string) []any {
	out := make([]any, len(ss))
	for i, v := range ss {
		out[i] = v
	}
	return out
}

// SetSREKubeconfigProducer installs the SRE fake producer on the in-process
// runner factory. When the LLM calls it, Execute emits secretValue out-of-band
// under a secretOutput named outputName AND (on success) JIT-writes the pin
// tuple `cluster:<argv[1]>#debug_target@agentsession:<ns>/<name>` via the real
// relwrites.SpiceDBWriter — the exact writesRelationships block a real
// sre-fetch-kubeconfig toolspec declares. A second call for a different cluster
// fast-fails write-once, so the pin fires at most once per session.
//
// The relWriter is derived from the harness's SpiceDB the same way
// buildSandboxTools/buildMCPTools derive theirs, so the tuple lands in the
// harness's real SpiceDB container. Call after Start and before the AgentSession
// is spawned (before SendUserMessage), mirroring SetSecretOutputProducer's
// lifecycle contract.
func (h *Harness) SetSREKubeconfigProducer(toolName, outputName, description, secretValue string) {
	h.runnerFactory.ExtraTools = append(h.runnerFactory.ExtraTools, &srePinningProducerTool{
		name:  toolName,
		value: secretValue,
		spec:  &secretout.Spec{Name: outputName, Description: description},
		// The canonical SRE pin block: on a successful fetch, pin the cluster
		// (argv[1]) to the calling session as debug_target. Mirrors the fixture
		// in pkg/agent/tool/sandbox/sandbox_tool_relwrites_test.go's relwritesSpec
		// and the example toolspec — Exclusive so the pin is atomically
		// write-once per session (Task 13).
		writes: []relwrites.Block{{
			When:      "result.success",
			Exclusive: true,
			Tuple: relwrites.Tuple{
				Resource: `"cluster:" + args.argv[1]`,
				Relation: `"debug_target"`,
				Subject:  `"agentsession:" + session`,
			},
		}},
		relWriter: h.NewSRERelWriter(),
	})
}

// NewSRERelWriter returns a relwrites.Writer bound to the harness's REAL
// SpiceDB, wired exactly as the runner and the SRE producer wire theirs
// (relwrites.Source, via (*spicedb.Client).Writer). Exposed so an e2e test can
// drive the atomic write-once (Exclusive) precondition directly against the
// real SpiceDB container — the deterministic proof of the atomic gate that a
// fake writer cannot model. Returns nil when the harness has no SpiceDB.
func (h *Harness) NewSRERelWriter() relwrites.Writer {
	if h.runnerFactory == nil || h.runnerFactory.SpiceDB == nil {
		return nil
	}
	return &relwrites.SpiceDBWriter{
		Client: h.runnerFactory.SpiceDB.Writer(relwrites.Source),
	}
}
