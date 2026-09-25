//go:build e2e

// Package authz_schema_barrier_test covers the harness's own authz-schema
// readiness barrier — the fence that keeps a scenario from driving traffic
// before the guardian has written the composed SpiceDB schema.
//
// The bug it pins down was a harness ordering gap, not a product defect. The
// AgentClass reconciler creates the class's "<class>-grants" AgentSessionGrants
// CR and stamps Valid=True in the SAME pass; the guardian's
// compose→WriteSchema is the downstream, independent reconcile that create
// triggers. So AgentClass Valid=True — the condition every scenario gates on —
// says nothing about whether `definition agentsession` exists yet. Measured on
// an idle machine the schema landed ~45 ms after Valid=True: invisible behind
// WaitForAgentClassValid's 250 ms poll on a quiet machine, and wide open under
// suite contention.
//
// When a scenario lost that race the first inbound message created an
// AgentSession, channelsd's TouchStartedBy hit
// `FailedPrecondition: object definition "agentsession" not found`, and the
// session died at Failed/AuthzWriteFailed — several layers away from the
// cause, and against a different test each time, so it read as generic
// contention flake.
//
// WaitForSpiceDBBootstrap did not cover it: it is a no-op for a fixture that
// ships no SpiceDBBootstrap CR, which is most of test/e2e/testdata.
//
// A 45 ms window cannot be asserted on, so the test widens it:
// Options.SchemaWriteDelay stalls every guardian WriteSchema, turning the race
// into a certainty. Without the barrier this test fails deterministically;
// with it, the send simply waits.
//
// The fixture is deliberately the MCPServer-free agent-interaction-roundtrip
// dir. A class WITH tool permission checks validates against the live schema
// before reporting Valid, which accidentally orders the two — exactly why the
// centerdot scenarios rarely saw this and the lighter fixtures did.
package authz_schema_barrier_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/test/e2e"
)

const (
	fixtureClass = "interaction-roundtrip-agent"
	ownerEmail   = "alice@example.com"

	// schemaWriteDelay has to outlast every incidental wait between
	// WaitForAgentClassValid returning and the probe below — the 250 ms
	// condition poll, an apiserver round trip — so that a missing fence cannot
	// be papered over by unrelated waiting. 6 s does, and still leaves the whole
	// test well inside a normal e2e budget. Verified: with the barrier removed
	// this test fails on the probe, reproducibly.
	schemaWriteDelay = 6 * time.Second
)

func TestE2E_AuthzSchemaBarrier_ClassValidImpliesSchemaLive(t *testing.T) {
	h := e2e.Start(t, e2e.Options{
		AgentDir:         "../../testdata/agent-interaction-roundtrip",
		DefaultTimeout:   60 * time.Second,
		DefaultUser:      ownerEmail,
		SchemaWriteDelay: schemaWriteDelay,
	})

	h.WaitForAgentClassValid(fixtureClass, 90*time.Second)

	// The invariant itself, checked the instant the wait returns: a
	// WaitForAgentClassValid that gated on the Valid condition alone comes back
	// mid-write and fails here. Asserted directly rather than via elapsed time,
	// because how much of the stalled write precedes Valid=True varies with how
	// many AgentClass passes it takes to converge.
	require.True(t, h.AuthzSchemaLive(),
		"WaitForAgentClassValid must not return before the guardian's composed SpiceDB schema is live")

	h.LLM.OnUserMessage("hi").Reply(e2e.RespondToUser("hi there"))
	h.LLM.OnToolResult("respond_to_user", e2e.AnyResult()).Reply(e2e.EndTurn()).Repeating()

	h.SendUserMessage("hi")
	h.ExpectAgentReply(e2e.Contains("hi there"))

	// The headline assertion. Before the barrier this was
	// AuthzWriteFailed / "object definition `agentsession` not found".
	var list spiceboxv1alpha1.AgentSessionList
	require.NoError(t, h.K8s.List(context.Background(), &list, client.InNamespace("default")),
		"list AgentSessions after the reply")
	require.NotEmpty(t, list.Items, "the inbound message should have created a session")
	for i := range list.Items {
		s := &list.Items[i]
		assert.Nil(t, s.Status.StartFailure,
			"session %s must not carry a startFailure — a set one means the session was created before the SpiceDB schema was live", s.Name)
	}

	h.AssertAllRulesConsumed()
}
