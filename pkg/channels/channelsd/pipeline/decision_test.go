// pkg/channels/channelsd/pipeline/decision_test.go
//
// Holds erroringNATS, the always-failing NATS fake portal_access_test.go uses
// to exercise publish-failure logging. Permission approve/deny/resubmit
// behavior is covered on decidePermission (permission_interaction_test.go),
// reached through the generic HandleInteractionDecision pipe
// (interaction_decision_test.go).
package pipeline

// erroringNATS is a NATS fake whose Publish always returns errPublish.
// Used to verify that publish failures bubble up through besteffort.Log
// instead of being silently dropped (the production incident the
// `pkg/x/besteffort` package was created in response to).
type erroringNATS struct {
	err   error
	calls int
}

func (e *erroringNATS) Publish(_ string, _ []byte) error {
	e.calls++
	return e.err
}
