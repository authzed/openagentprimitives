//go:build e2e

package e2e

import (
	"github.com/authzed/openagentprimitives/pkg/agent/userprofile"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/plangate"
)

// DumpStateForTest exposes the unexported dumpState diagnostic to
// external _test.go files. Build-tag gated and named *ForTest to make
// the test-only intent obvious. Keeping dumpState unexported in the
// public API stops production callers from accidentally depending on
// its (intentionally human-readable, not parseable) output.
func DumpStateForTest(h *Harness) string {
	return h.dumpState()
}

// BuildLoopToolNamesForTest builds the per-session runner.Loop the factory
// would run and returns the Name() of every tool in its assembled tool set.
// Test-only seam (build-tag gated, *ForTest) that lets external _test.go files
// assert the factory routes tool assembly through capability.Assemble without
// exporting buildLoop. resolvedSidecars is passed empty — the parity test does
// not exercise sidecars.
func (f *InProcessRunnerFactory) BuildLoopToolNamesForTest(sess *spiceboxv1alpha1.AgentSession, class *spiceboxv1alpha1.AgentClass) ([]string, error) {
	loop, err := f.buildLoop(sess, class, nil)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(loop.Tools))
	for _, t := range loop.Tools {
		names = append(names, t.Name())
	}
	return names, nil
}

// BuildLoopUserProfileWiringForTest builds the per-session runner.Loop the
// factory would run and returns the two things userprofilegate.Offer's
// single decision is supposed to gate TOGETHER: the composed system prompt
// string ComposeSystem produced, and whether the Loop's FetchSpeakerProfile /
// SpeakerProfileFields fields got set. Test-only seam (build-tag gated,
// *ForTest) that lets external _test.go files prove the two consumers of one
// Offer call — the untrusted-profile prompt paragraph and the Loop field
// assignment — cannot diverge, without exporting buildLoop or the internal
// profileActive value itself. resolvedSidecars is passed empty; this seam
// does not exercise sidecars.
func (f *InProcessRunnerFactory) BuildLoopUserProfileWiringForTest(
	sess *spiceboxv1alpha1.AgentSession, class *spiceboxv1alpha1.AgentClass,
) (system string, fetchWired bool, fields []userprofile.Field, err error) {
	loop, err := f.buildLoop(sess, class, nil)
	if err != nil {
		return "", false, nil, err
	}
	return loop.System, loop.FetchSpeakerProfile != nil, loop.SpeakerProfileFields, nil
}

// GrantSchemaReadyForTest exposes the unexported grantSchemaReady predicate
// behind WaitForGrantSchema so its readiness rule can be covered without
// standing up a control plane.
func GrantSchemaReadyForTest(items []spiceboxv1alpha1.AgentSessionGrants) (bool, string) {
	return grantSchemaReady(items)
}

// BuildLoopPlanGateSlotTransformsForTest builds the per-session runner.Loop the
// factory would run and returns Loop.PlanGateSlotTransforms — the transform-
// chain map the plan gate's approval path (approverCanDelegateSlots,
// narrowToApproved in pkg/agent/runner/host_approval.go) derives object ids
// through.
//
// Test-only seam so an external _test.go file can drive THIS factory's own
// construction call site, not runner.SlotTransformsOf in isolation. A unit
// test that calls runner.SlotTransformsOf directly cannot fail if a future
// edit deletes "PlanGateSlotTransforms: runner.SlotTransformsOf(class)" at
// buildLoop's call site (or pastes a local hand-rolled copy back in) — it
// never exercises that line. This does.
func (f *InProcessRunnerFactory) BuildLoopPlanGateSlotTransformsForTest(sess *spiceboxv1alpha1.AgentSession, class *spiceboxv1alpha1.AgentClass) (map[string][]string, error) {
	loop, err := f.buildLoop(sess, class, nil)
	if err != nil {
		return nil, err
	}
	return loop.PlanGateSlotTransforms, nil
}

// BuildLoopPlanGateSlotStandingForTest is BuildLoopPlanGateSlotTransformsForTest's
// sibling for Loop.PlanGateSlotStanding — the per-type standing map
// approverCanDelegateSlots (pkg/agent/runner/host_approval.go) reads to decide
// whether a slot's approval needs a SpiceDB lookup at all. Same rationale: a
// unit test on runner.SlotStandingOf alone cannot fail if a future edit drops
// "PlanGateSlotStanding: runner.SlotStandingOf(class)" from this factory's own
// buildLoop call site; this seam drives that exact line.
func (f *InProcessRunnerFactory) BuildLoopPlanGateSlotStandingForTest(sess *spiceboxv1alpha1.AgentSession, class *spiceboxv1alpha1.AgentClass) (map[string]string, error) {
	loop, err := f.buildLoop(sess, class, nil)
	if err != nil {
		return nil, err
	}
	return loop.PlanGateSlotStanding, nil
}

// BuildLoopPlanGatePermissionTitlesForTest is
// BuildLoopPlanGateSlotTransformsForTest's sibling for
// Loop.PlanGatePermissionTitles — the "<resourceType>/<permission>" title
// lookup plangate.CardInput.PermissionTitles reads (via
// pkg/authz/hooks/plangate.go's PlanGateDeps.PermissionTitles). Same
// rationale: a unit test on runner.PermissionTitlesOf alone
// (pkg/agent/runner/slot_value_chain_test.go) cannot catch a regression where
// buildLoop stops calling it; this seam drives that exact line.
func (f *InProcessRunnerFactory) BuildLoopPlanGatePermissionTitlesForTest(sess *spiceboxv1alpha1.AgentSession, class *spiceboxv1alpha1.AgentClass) (map[string]string, error) {
	loop, err := f.buildLoop(sess, class, nil)
	if err != nil {
		return nil, err
	}
	return loop.PlanGatePermissionTitles, nil
}

// BuildLoopPlanGateResourceDisplaysForTest is
// BuildLoopPlanGatePermissionTitlesForTest's sibling for
// Loop.PlanGateResourceDisplays — the per-resourceType display lookup
// plangate.CardInput.ResourceDisplays reads (via
// pkg/authz/hooks/plangate.go's PlanGateDeps.ResourceDisplays). Same
// rationale: a unit test on runner.ResourceDisplaysOf alone
// (pkg/agent/runner/slot_value_chain_test.go) cannot catch a regression where
// buildLoop stops calling it; this seam drives that exact line.
func (f *InProcessRunnerFactory) BuildLoopPlanGateResourceDisplaysForTest(sess *spiceboxv1alpha1.AgentSession, class *spiceboxv1alpha1.AgentClass) (map[string]plangate.ResourceDisplay, error) {
	loop, err := f.buildLoop(sess, class, nil)
	if err != nil {
		return nil, err
	}
	return loop.PlanGateResourceDisplays, nil
}

// BuildLoopEngineWiredForTest builds the per-session runner.Loop the factory
// would run and reports whether Loop.Engine is non-nil. Test-only seam
// (build-tag gated, *ForTest) so an external _test.go file can drive THIS
// factory's own construction call site — the `if f.SpiceDB != nil` block in
// inprocess_runner_factory.go's buildLoop, which must wire loop.Engine exactly
// as production does at internal/cmd/runner/main.go.
//
// A nil Engine is a SILENT bug here: promoteObservedSlots
// (pkg/agent/runner/loop_autofill.go) returns at its first guard when
// loop.Engine == nil, so no fillFrom:[observed] slot ever binds and a fork
// bundle asserting on that binding passes while testing nothing. This seam is
// what turns that silent no-op into a failing assertion.
func (f *InProcessRunnerFactory) BuildLoopEngineWiredForTest(sess *spiceboxv1alpha1.AgentSession, class *spiceboxv1alpha1.AgentClass) (bool, error) {
	loop, err := f.buildLoop(sess, class, nil)
	if err != nil {
		return false, err
	}
	return loop.Engine != nil, nil
}

// SessionRefForTest returns the (namespace, name) of the one AgentSession in
// the harness namespace. Test-only seam (build-tag gated, *ForTest) so external
// _test.go files can address per-session memory without exporting singleSession
// into the harness public API.
func SessionRefForTest(h *Harness) (ns, name string) {
	return h.singleSession("SessionRefForTest")
}
