// pkg/agent/tool/sandbox/unbilledfailure_carrier_test.go
//
// The producing end of the credential-halt signal. A streaming toolkit's own
// terminal result is the only place the platform can see "this run never
// reached the provider's metered path", and composeStreamResult is where that
// becomes a field on tool.Result for the tool guard to read at PostToolCall.
//
// These tests exist to pin what the carrier is NOT allowed to be derived from.
// The guard answers this signal by ending the session, so anything the agent
// or an upstream tool can write into the run's OUTPUT must be unable to move
// it — in either direction.
package sandbox_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/tool/sandbox"
	"github.com/authzed/openagentprimitives/pkg/tools/toolkitstream"
)

// authErrorText is what the claude CLI writes to STDOUT — not stderr — when its
// key is refused. It appears in these fixtures as the run's own output
// precisely so the assertions can show it is never consulted.
const authErrorText = `API Error: 401 {"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`

func TestComposeStreamResultCarriesTheUnbilledFailure(t *testing.T) {
	t.Run("unbilled non-success on a failed process: the carrier is set", func(t *testing.T) {
		res := sandbox.ComposeStreamResult(
			sandbox.BridgeResult{ExitCode: 1, ExitReason: "failed"},
			&toolkitstream.Outcome{HasResult: true, OK: false, Unbilled: true, Text: authErrorText},
			nil, nil,
		)

		require.True(t, res.IsError, "precondition: an unbilled failure composes an error result")
		assert.True(t, res.UnbilledFailure,
			"the toolkit declared a terminal result it was not billed for; that is the halt signal")
	})

	t.Run("BILLED failure: ordinary tool error, no credential claim", func(t *testing.T) {
		res := sandbox.ComposeStreamResult(
			sandbox.BridgeResult{ExitCode: 1, ExitReason: "failed"},
			&toolkitstream.Outcome{HasResult: true, OK: false, CostUSD: 0.005, Text: "the build failed"},
			nil, nil,
		)

		require.True(t, res.IsError, "precondition: a reported failure is still an error result")
		assert.False(t, res.UnbilledFailure,
			"the provider charged for this run, so it authenticated; the failure is about the work")
	})

	t.Run("no parser (no terminal result to read): nothing is claimed", func(t *testing.T) {
		res := sandbox.ComposeStreamResult(
			sandbox.BridgeResult{ExitCode: 1, ExitReason: "failed"},
			nil, nil, []byte(authErrorText),
		)

		require.True(t, res.IsError)
		assert.False(t, res.UnbilledFailure,
			"a toolkit with no stream parser reports no billing, and stderr is never read for this")
	})

	t.Run("a run the origin AUTHENTICATED never claims an unbilled failure", func(t *testing.T) {
		// A process that ran to completion and exited 0 is the positive
		// evidence composeStreamResult already reads as OriginAuthenticated.
		// Positive evidence outranks a shape — the same precedence
		// credupdate.IsAuthShaped applies — so the two carriers can never
		// contradict each other on one result.
		res := sandbox.ComposeStreamResult(
			sandbox.BridgeResult{ExitCode: 0, ExitReason: "completed"},
			&toolkitstream.Outcome{HasResult: true, OK: false, Unbilled: true, Text: authErrorText},
			nil, nil,
		)

		require.True(t, res.IsError, "precondition: the parser's OK=false still makes this an error result")
		require.True(t, res.OriginAuthenticated, "precondition: exit 0 on a completed run")
		assert.False(t, res.UnbilledFailure,
			"a credential the origin accepted cannot also be one it refused")
	})

	t.Run("a watchdog-killed stream claims no unbilled failure", func(t *testing.T) {
		// idle and maxDuration are OUR timer ending the process. Whatever the
		// toolkit had reported so far, we stopped it — that is not the
		// provider refusing a credential.
		for _, reason := range []string{"idle", "maxDuration"} {
			res := sandbox.ComposeStreamResult(
				sandbox.BridgeResult{ExitCode: 0, ExitReason: reason},
				&toolkitstream.Outcome{HasResult: true, OK: false, Unbilled: true},
				nil, nil,
			)
			assert.Falsef(t, res.UnbilledFailure,
				"ExitReason %q is a platform-initiated kill, not a credential verdict", reason)
		}
	})
}

// TestComposeStreamResultUnbilledFailureIgnoresOutputText is the injection
// control at the producing boundary.
//
// The agent picks the argv, every CLI echoes argv back into its errors, and
// claude writes its auth error to STDOUT — so the run's own output is
// attacker-reachable text. A result that SAYS "401 API key is invalid" while
// its structured terminal event reports a billed success must not halt the
// session, and one that says nothing alarming while reporting an unbilled
// failure must still halt it.
func TestComposeStreamResultUnbilledFailureIgnoresOutputText(t *testing.T) {
	loud := sandbox.ComposeStreamResult(
		sandbox.BridgeResult{ExitCode: 0, ExitReason: "completed"},
		&toolkitstream.Outcome{HasResult: true, OK: true, CostUSD: 0.02, Text: authErrorText},
		[]byte(authErrorText), []byte(authErrorText),
	)
	require.Contains(t, loud.Content, "401", "precondition: the auth-error text is in the result the model reads")
	require.False(t, loud.IsError, "precondition: a billed success is not an error result")
	assert.False(t, loud.UnbilledFailure,
		"text is not evidence; a hostile tool result must not be able to end the session")

	// The same claim with the compose-layer gates OPEN, so nothing but the
	// classifier stands between the injected text and a halt: the process
	// failed (so ExitReason clears, and OriginAuthenticated does not apply),
	// while the toolkit's own terminal event reports a billed success.
	ungated := sandbox.ComposeStreamResult(
		sandbox.BridgeResult{ExitCode: 1, ExitReason: "failed"},
		&toolkitstream.Outcome{HasResult: true, OK: true, CostUSD: 0.02, Text: authErrorText},
		[]byte(authErrorText), []byte(authErrorText),
	)
	require.True(t, ungated.IsError, "precondition: the process failed, so the carrier's gates are open")
	require.False(t, ungated.OriginAuthenticated, "precondition: no positive evidence is doing the work here")
	assert.False(t, ungated.UnbilledFailure,
		"with every gate open, the classifier alone must still refuse to read the text")

	quiet := sandbox.ComposeStreamResult(
		sandbox.BridgeResult{ExitCode: 1, ExitReason: "failed"},
		&toolkitstream.Outcome{HasResult: true, OK: false, Unbilled: true, Text: "all good, nothing to report"},
		nil, nil,
	)
	assert.True(t, quiet.UnbilledFailure,
		"and reassuring text must not be able to mask one either")
}
