package toolcheck_test

// web_action_reprompt_test.go unit-tests Phase E's E10 task: distinguishing a
// browser-mutate CHANGED-PAGE RE-PROMPT (a web_action grant for this instance
// already exists; only its pagestate_matches caveat failed) from an ordinary
// FIRST-TIME ask (no grant at all), and the off-by-default model-escalation
// knob that may — ONLY when explicitly enabled — let a judge suppress a
// re-prompt it confirms is benign.
//
// The shared recordingSpiceDB fake (check_tool_call_test.go) cannot express
// this distinction: it answers purely by (resourceType, resourceID,
// permission), with no regard for whether the request carried a
// pagestate_matches Context. The caveated web_action leg always presents one;
// E10's reprompt-signal PROBE deliberately omits it (see
// webActionGrantExistsForReprompt's doc comment) — so telling "grant exists,
// caveat failed" apart from "no grant at all" needs a fake that answers
// differently depending on Context presence. contextAwareSpiceDB below does
// exactly that, and only for this file.

import (
	"context"
	"testing"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb/toolcheck"
	"github.com/authzed/openagentprimitives/pkg/tools/browserschema"
)

// contextAwareSpiceDB answers the web_host leg with one fixed verdict, and the
// web_action leg with ONE OF TWO verdicts depending on whether the request
// carries a pagestate_matches Context: WithContext for the caveated main
// check (checkOneSlotGrant always presents one for web_action), NoContext for
// E10's uncaveated reprompt-signal probe (which deliberately omits one). This
// is the one axis the shared recordingSpiceDB fake cannot express.
type contextAwareSpiceDB struct {
	webHost              v1.CheckPermissionResponse_Permissionship
	webActionWithContext v1.CheckPermissionResponse_Permissionship
	webActionNoContext   v1.CheckPermissionResponse_Permissionship
	calls                []string
}

func (f *contextAwareSpiceDB) CheckPermission(_ context.Context, in *v1.CheckPermissionRequest) (*v1.CheckPermissionResponse, error) {
	f.calls = append(f.calls, in.Resource.ObjectType+":"+in.Resource.ObjectId+"#"+in.Permission)
	switch in.Resource.ObjectType {
	case browserschema.WebHostType:
		return &v1.CheckPermissionResponse{Permissionship: f.webHost, CheckedAt: &v1.ZedToken{Token: "zt"}}, nil
	case browserschema.WebActionType:
		if in.GetContext() != nil {
			return &v1.CheckPermissionResponse{Permissionship: f.webActionWithContext, CheckedAt: &v1.ZedToken{Token: "zt"}}, nil
		}
		return &v1.CheckPermissionResponse{Permissionship: f.webActionNoContext, CheckedAt: &v1.ZedToken{Token: "zt"}}, nil
	default:
		return &v1.CheckPermissionResponse{Permissionship: v1.CheckPermissionResponse_PERMISSIONSHIP_NO_PERMISSION, CheckedAt: &v1.ZedToken{Token: "zt"}}, nil
	}
}

// fakeRepromptJudge is a scripted toolcheck.RepromptJudge: returns (benign,
// err) verbatim and counts how many times it was consulted, so a test can
// assert the off-by-default knob never calls it at all.
type fakeRepromptJudge struct {
	benign bool
	err    error
	calls  int
}

func (f *fakeRepromptJudge) ConfirmBenign(_ context.Context, _, _ string) (bool, error) {
	f.calls++
	return f.benign, f.err
}

// A web_action grant EXISTS for this instance (the probe, no Context, reads
// CONDITIONAL_PERMISSION) but its caveat no longer matches the live page (the
// main caveated check, Context present, reads NO_PERMISSION) → DENIED, and the
// denial is flagged ChangedPageReprompt: true. This is the re-prompt case.
func TestCheck_Reprompt_GrantExistsCaveatFailed_SignalsChangedPage(t *testing.T) {
	cli := &contextAwareSpiceDB{
		webHost:              v1.CheckPermissionResponse_PERMISSIONSHIP_HAS_PERMISSION,
		webActionWithContext: v1.CheckPermissionResponse_PERMISSIONSHIP_NO_PERMISSION,
		webActionNoContext:   v1.CheckPermissionResponse_PERMISSIONSHIP_CONDITIONAL_PERMISSION,
	}
	res := toolcheck.Checker{Cli: cli}.CheckToolCall(context.Background(), actingWebHostPerm(), mutateInputs())

	require.Equal(t, authz.OutcomeDenied, res.Outcome, "a diverged fingerprint must still deny")
	assert.True(t, res.ChangedPageReprompt,
		"a grant that EXISTS but whose caveat failed must be flagged as a changed-page re-prompt")
}

// NO web_action grant exists at all (the probe ALSO reads NO_PERMISSION,
// exactly like the main check) → DENIED, and the denial is NOT flagged as a
// changed-page re-prompt. This is the ordinary first-time-ask case, and it
// must read differently from the one above.
func TestCheck_Reprompt_NoGrantAtAll_IsAPlainFirstAsk(t *testing.T) {
	cli := &contextAwareSpiceDB{
		webHost:              v1.CheckPermissionResponse_PERMISSIONSHIP_HAS_PERMISSION,
		webActionWithContext: v1.CheckPermissionResponse_PERMISSIONSHIP_NO_PERMISSION,
		webActionNoContext:   v1.CheckPermissionResponse_PERMISSIONSHIP_NO_PERMISSION,
	}
	res := toolcheck.Checker{Cli: cli}.CheckToolCall(context.Background(), actingWebHostPerm(), mutateInputs())

	require.Equal(t, authz.OutcomeDenied, res.Outcome, "no grant at all must still deny")
	assert.False(t, res.ChangedPageReprompt,
		"a first-time ask (no grant has ever existed) must NOT be flagged as a changed-page re-prompt")
}

// THE ESCAPE-SAFE DEFAULT: with the escalation knob explicitly set to
// Enabled: false — a judge IS wired, it is simply turned off — a changed-page
// re-prompt ALWAYS denies, even though the judge WOULD confirm the change
// benign if it were ever asked. Setting Judge to a non-nil fake here (rather
// than leaving it nil, as an earlier version of this test did) is what makes
// this prove "wired but disabled judge is never consulted" rather than merely
// "an absent judge is never consulted" — the two are different claims, and
// only the former pins that Enabled is the thing gating the consult, not the
// presence of a Judge.
func TestCheck_Reprompt_EscalationOffByDefault_AlwaysReprompts(t *testing.T) {
	cli := &contextAwareSpiceDB{
		webHost:              v1.CheckPermissionResponse_PERMISSIONSHIP_HAS_PERMISSION,
		webActionWithContext: v1.CheckPermissionResponse_PERMISSIONSHIP_NO_PERMISSION,
		webActionNoContext:   v1.CheckPermissionResponse_PERMISSIONSHIP_CONDITIONAL_PERMISSION,
	}
	judge := &fakeRepromptJudge{benign: true} // would say benign, if ever asked
	checker := toolcheck.Checker{
		Cli:                cli,
		RepromptEscalation: toolcheck.RepromptEscalationConfig{Enabled: false, Judge: judge},
	}
	res := checker.CheckToolCall(context.Background(), actingWebHostPerm(), mutateInputs())

	assert.Equal(t, authz.OutcomeDenied, res.Outcome,
		"the knob defaults to off, so a benign-but-diverged page must still re-prompt")
	assert.True(t, res.ChangedPageReprompt)
	assert.Equal(t, 0, judge.calls, "a judge that is wired but DISABLED must never be consulted")
}

// Escalation explicitly ENABLED + a judge confirming the change is benign →
// the re-prompt is suppressed and the call is ALLOWED. This is the one
// knowingly-opted-in exception to "a caveat failure always re-prompts" — and
// it only ever fires for an instance a human has already approved once
// (ChangedPageReprompt true), never a first-time ask.
func TestCheck_Reprompt_EscalationEnabledAndJudgeConfirmsBenign_SuppressesReprompt(t *testing.T) {
	cli := &contextAwareSpiceDB{
		webHost:              v1.CheckPermissionResponse_PERMISSIONSHIP_HAS_PERMISSION,
		webActionWithContext: v1.CheckPermissionResponse_PERMISSIONSHIP_NO_PERMISSION,
		webActionNoContext:   v1.CheckPermissionResponse_PERMISSIONSHIP_CONDITIONAL_PERMISSION,
	}
	judge := &fakeRepromptJudge{benign: true}
	checker := toolcheck.Checker{
		Cli:                cli,
		RepromptEscalation: toolcheck.RepromptEscalationConfig{Enabled: true, Judge: judge},
	}
	res := checker.CheckToolCall(context.Background(), actingWebHostPerm(), mutateInputs())

	assert.Equal(t, authz.OutcomeAllowed, res.Outcome,
		"an enabled knob + a judge-confirmed-benign change must suppress the re-prompt")
	assert.Equal(t, 1, judge.calls)
}

// Escalation enabled but the judge ERRORS → fail CLOSED: the re-prompt still
// happens, exactly as if the knob were off. A judge failure must never read as
// evidence of benign-ness.
func TestCheck_Reprompt_EscalationEnabledButJudgeErrors_FailsClosedToReprompt(t *testing.T) {
	cli := &contextAwareSpiceDB{
		webHost:              v1.CheckPermissionResponse_PERMISSIONSHIP_HAS_PERMISSION,
		webActionWithContext: v1.CheckPermissionResponse_PERMISSIONSHIP_NO_PERMISSION,
		webActionNoContext:   v1.CheckPermissionResponse_PERMISSIONSHIP_CONDITIONAL_PERMISSION,
	}
	judge := &fakeRepromptJudge{err: assert.AnError}
	checker := toolcheck.Checker{
		Cli:                cli,
		RepromptEscalation: toolcheck.RepromptEscalationConfig{Enabled: true, Judge: judge},
	}
	res := checker.CheckToolCall(context.Background(), actingWebHostPerm(), mutateInputs())

	assert.Equal(t, authz.OutcomeDenied, res.Outcome, "a judge error must never suppress the re-prompt")
	assert.True(t, res.ChangedPageReprompt)
	assert.Equal(t, 1, judge.calls)
}

// Escalation enabled but the judge says NOT benign → the re-prompt still
// happens.
func TestCheck_Reprompt_EscalationEnabledButJudgeSaysNotBenign_StillReprompts(t *testing.T) {
	cli := &contextAwareSpiceDB{
		webHost:              v1.CheckPermissionResponse_PERMISSIONSHIP_HAS_PERMISSION,
		webActionWithContext: v1.CheckPermissionResponse_PERMISSIONSHIP_NO_PERMISSION,
		webActionNoContext:   v1.CheckPermissionResponse_PERMISSIONSHIP_CONDITIONAL_PERMISSION,
	}
	judge := &fakeRepromptJudge{benign: false}
	checker := toolcheck.Checker{
		Cli:                cli,
		RepromptEscalation: toolcheck.RepromptEscalationConfig{Enabled: true, Judge: judge},
	}
	res := checker.CheckToolCall(context.Background(), actingWebHostPerm(), mutateInputs())

	assert.Equal(t, authz.OutcomeDenied, res.Outcome)
	assert.Equal(t, 1, judge.calls)
}
