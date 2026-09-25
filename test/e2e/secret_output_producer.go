//go:build e2e

package e2e

import (
	"context"
	"encoding/json"

	"github.com/authzed/openagentprimitives/pkg/agent/secretout"
	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/authz"
)

// secretOutputProducerTool is a test seam: a Tool whose Execute returns a
// tool.Result carrying a SecretOutput, so the runner's applySecretOutput diverts
// the value out-of-band exactly as it would for a real sandbox producer whose
// SpiceboxToolspec declared a secretOutput.
//
// It exists because the SpiceboxToolspec CRD does not yet mirror the toolspec
// library's secretOutput field, so the in-process sandbox synthesis path can't
// carry a secretOutput end-to-end. This tool drives the same runner-side path
// (store Put → publisher Publish → status WriteSatisfiedSecretOutput → Content
// scrub) without that CRD gap.
//
// StateImpact is Passthrough so the dispatcher runs no SpiceDB Check (the value
// it emits is a fixed test fixture, not a real credential).
type secretOutputProducerTool struct {
	name  string
	value string
	spec  *secretout.Spec
}

func (s *secretOutputProducerTool) Name() string    { return s.name }
func (s *secretOutputProducerTool) Kind() tool.Kind { return tool.KindSandbox }
func (s *secretOutputProducerTool) Description() string {
	return "Test producer that emits a secret value out-of-band as a secretOutput."
}

// InputSchema declares a single optional string arg so the LLM has a valid
// schema to call against; the args are ignored by Execute.
func (s *secretOutputProducerTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"_reason":{"type":"string"}}}`)
}

func (s *secretOutputProducerTool) Execute(_ context.Context, _ json.RawMessage, _ *tool.SessionContext) (tool.Result, error) {
	// Content IS the raw secret value; SecretOutput non-nil tells the runner to
	// divert it. Mirrors sandbox composeResult's stdout-sourced secret shape.
	return tool.Result{
		Content:      s.value,
		SecretOutput: s.spec,
	}, nil
}

func (s *secretOutputProducerTool) Permission() authz.Permission {
	return authz.Permission{StateImpact: authz.Passthrough}
}

func (s *secretOutputProducerTool) PermissionVariants() []authz.PermissionVariant { return nil }

var _ tool.Tool = (*secretOutputProducerTool)(nil)

// SetSecretOutputProducer installs a producer Tool on the in-process runner
// factory that, when called by the LLM, emits secretValue out-of-band under a
// secretOutput named outputName with the given description. The runner diverts
// the value (never letting it reach the LLM), publishes it to the operator's
// /secret-output endpoint (so it lands in the per-session Secret), and records
// the handle on AgentSession.status.
//
// Call after Start and before the AgentSession is spawned (before
// SendUserMessage), mirroring SetSidecarProbeURL's lifecycle contract.
func (h *Harness) SetSecretOutputProducer(toolName, outputName, description, secretValue string) {
	h.runnerFactory.ExtraTools = append(h.runnerFactory.ExtraTools, &secretOutputProducerTool{
		name:  toolName,
		value: secretValue,
		spec: &secretout.Spec{
			Name:        outputName,
			Description: description,
		},
	})
}
