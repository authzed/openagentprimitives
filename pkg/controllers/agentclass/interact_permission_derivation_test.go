// pkg/controllers/agentclass/interact_permission_derivation_test.go
//
// Untagged (unit tier): a class whose input carries no human must declare an
// interact policy, and a freshly installed webhook-driven agent never can — the
// only sensible value is the id of the Slack channel it posts into, which is
// per-install and so cannot live in a checked-in bundle.
//
// Observed twice, on two separate clusters: Valid=False with
// UserLessChannelMissingAuthz, fixed by hand with
// slack_channel:<the output channel's id>#member. The reconciler already walks
// this class's bound Channels and already asks the output one's kind for that
// exact subject-set (it is what spec.owner.ownerless.fromOutputChannel resolves
// through), so the value was derivable from what the install already knew.
//
// This file pins that it is derived, that a declared value is never overridden,
// and that with nothing to derive from the refusal stands in its existing words.
package agentclass_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"

	// The kinds the fixtures name, blank-imported so the table asks the REAL
	// Kind.UserAttributable / OwnerGroupRef answers: github is the webhook input
	// that carries no human, slack the destination that carries a membership.
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/github"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/slack"
)

const derivedInteract = "slack_channel:C0DEMO123#member"

// ghInput is the webhook half: a role=input Channel of a kind that names no
// human. authzSubject is the OTHER half of the userless rule and is supplied
// here so a case's outcome turns only on the interact permission.
func ghInput(name, acName string) *spiceboxv1alpha1.Channel {
	return &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: spiceboxv1alpha1.ChannelSpec{
			Kind:         "github",
			Role:         spiceboxv1alpha1.ChannelRoleInput,
			AgentClass:   acName,
			AuthzSubject: "service:demo-agent",
		},
	}
}

// slackOut is the role=output half. channelID "" omits outputDefaults, the
// state in which the kind supplies no membership subject-set at all.
func slackOut(name, acName, channelID string) *spiceboxv1alpha1.Channel {
	ch := &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: spiceboxv1alpha1.ChannelSpec{
			Kind:       "slack",
			Role:       spiceboxv1alpha1.ChannelRoleOutput,
			AgentClass: acName,
			Slack:      &spiceboxv1alpha1.SlackChannelConfig{},
		},
	}
	if channelID != "" {
		ch.Spec.Slack.OutputDefaults = &spiceboxv1alpha1.SlackOutputDefaults{ChannelID: channelID}
	}
	return ch
}

// channelObjects widens a table row's Channels for the shared reconcileClass
// helper, which takes any client.Object so a fixture can add MCPServers and
// toolspecs too.
func channelObjects(chs []*spiceboxv1alpha1.Channel) []client.Object {
	out := make([]client.Object, 0, len(chs))
	for _, ch := range chs {
		out = append(out, ch)
	}
	return out
}

// TestReconcile_DerivesSessionInteractPermission drives the derivation across
// every combination of what the class declared and what its bound Channels
// offer to derive from.
func TestReconcile_DerivesSessionInteractPermission(t *testing.T) {
	cases := []struct {
		name string
		// declared is spec.authz.session.interactPermission.
		declared string
		// onlyStartersInteract, when true, sets AllowedStarters + OnlyStartersInteract
		// instead of declared — the OTHER kind of presence that must suppress
		// derivation (a declared interactPermission is the first kind).
		onlyStartersInteract bool
		// channels are built per-case against the generated class name.
		channels  func(acName string) []*spiceboxv1alpha1.Channel
		wantValid metav1.ConditionStatus
		// wantDerived is status.derivedSessionInteractPermission; empty means
		// the derivation must not have fired.
		wantDerived string
		// wantEffective is what every consumer of the policy must read. Ignored
		// when wantEffectiveIsStarterSet is true (the value depends on the
		// generated class name, computed in the loop instead).
		wantEffective string
		// wantEffectiveIsStarterSet: EffectiveSessionInteractPermission() must
		// equal this class's own StarterSubjectSet() rather than a literal.
		wantEffectiveIsStarterSet bool
		// wantReason/wantMsg check a Valid=False case's refusal. Empty means
		// the default this file was written around: the userless-channel
		// refusal, naming the field a human has to fill in.
		wantReason string
		wantMsg    string
	}{
		{
			name: "userless input + a resolvable output channel: Valid with no hand-patching",
			channels: func(ac string) []*spiceboxv1alpha1.Channel {
				return []*spiceboxv1alpha1.Channel{ghInput(ac+"-gh", ac), slackOut(ac+"-slack", ac, "C0DEMO123")}
			},
			wantValid:     metav1.ConditionTrue,
			wantDerived:   derivedInteract,
			wantEffective: derivedInteract,
		},
		{
			// The regression direction. A declared policy is an intent; the
			// derivation fills an absence and never overrides one, and nothing
			// derived is published beside it to muddy the reading.
			name:     "declared interactPermission: untouched, and nothing is derived",
			declared: "group:demo-maintainers#member",
			channels: func(ac string) []*spiceboxv1alpha1.Channel {
				return []*spiceboxv1alpha1.Channel{ghInput(ac+"-gh", ac), slackOut(ac+"-slack", ac, "C0DEMO123")}
			},
			wantValid:     metav1.ConditionTrue,
			wantEffective: "group:demo-maintainers#member",
		},
		{
			// The second kind of presence that must suppress derivation: a
			// declared interactPermission fixes the value directly, but
			// onlyStartersInteract fixes it INDIRECTLY (to this class's own
			// starters). Both must win over a channel that WOULD otherwise
			// supply something to derive — this case's Channels are deliberately
			// the fully-derivable pair from the first case, so it would pass
			// whether the onlyStartersInteract guard existed or not if it derived
			// nothing to begin with.
			// Valid=False, and NOT for the derivation: onlyStartersInteract
			// requires an allowlist, an allowlist is a start gate, and a start
			// gate over a userless input is a class no session can ever start
			// (starters_test.go's TestReconcile_StartGateAndAUserlessInputCannot-
			// Coexist owns that rule). The input stays the userless one anyway,
			// because swapping it for a human-attributed Channel would make
			// derivedSessionInteractPermission return "" on the userlessInput
			// guard alone — the case would then pass whether the
			// onlyStartersInteract suppression existed or not, which is exactly
			// the trap the comment above warns about. Refused-and-derived-nothing
			// still proves the suppression; refused-and-nothing-to-derive would
			// not.
			name:                 "onlyStartersInteract with a derivable output channel: nothing derived, effective is the starter set, and the userless input refuses the class",
			onlyStartersInteract: true,
			channels: func(ac string) []*spiceboxv1alpha1.Channel {
				return []*spiceboxv1alpha1.Channel{ghInput(ac+"-gh", ac), slackOut(ac+"-slack", ac, "C0DEMO123")}
			},
			wantValid:                 metav1.ConditionFalse,
			wantReason:                spiceboxv1alpha1.ReasonSpecInvalid,
			wantMsg:                   "carries no person",
			wantEffectiveIsStarterSet: true,
		},
		{
			// Nothing to derive FROM: the output Channel carries no
			// destination, so its kind names no membership.
			name: "output channel with no channelId: still refused",
			channels: func(ac string) []*spiceboxv1alpha1.Channel {
				return []*spiceboxv1alpha1.Channel{ghInput(ac+"-gh", ac), slackOut(ac+"-slack", ac, "")}
			},
			wantValid: metav1.ConditionFalse,
		},
		{
			name: "no output channel at all: still refused",
			channels: func(ac string) []*spiceboxv1alpha1.Channel {
				return []*spiceboxv1alpha1.Channel{ghInput(ac+"-gh", ac)}
			},
			wantValid: metav1.ConditionFalse,
		},
		{
			// Two output Channels: which one's members would own it is not a
			// question this can answer, and picking either would be a guess.
			// The same ambiguity outputbind.Resolve refuses at session time.
			name: "two output channels: ambiguous, so nothing is derived",
			channels: func(ac string) []*spiceboxv1alpha1.Channel {
				return []*spiceboxv1alpha1.Channel{
					ghInput(ac+"-gh", ac),
					slackOut(ac+"-slack-a", ac, "C0DEMO123"),
					slackOut(ac+"-slack-b", ac, "C0DEMO456"),
				}
			},
			wantValid: metav1.ConditionFalse,
		},
		{
			// The over-derivation control: a class whose input names a human
			// needs no interact policy and must not acquire one. Publishing a
			// subject-set here would switch the interact gate on for a class
			// that never asked for it, and would hand everyone in the output
			// channel interact on sessions belonging to whoever started them.
			//
			// The output sibling is deliberately present and fully configured:
			// with nothing to derive from, this case would pass whether the
			// userless gate existed or not.
			name: "user-attributable input, output channel available anyway: nothing is derived",
			channels: func(ac string) []*spiceboxv1alpha1.Channel {
				return []*spiceboxv1alpha1.Channel{
					{
						ObjectMeta: metav1.ObjectMeta{Name: ac + "-slack-in", Namespace: "default"},
						Spec: spiceboxv1alpha1.ChannelSpec{
							Kind: "slack", Role: spiceboxv1alpha1.ChannelRoleInput, AgentClass: ac,
							Slack: &spiceboxv1alpha1.SlackChannelConfig{},
						},
					},
					slackOut(ac+"-slack-out", ac, "C0DEMO123"),
				}
			},
			wantValid: metav1.ConditionTrue,
		},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			acName := fmt.Sprintf("ac-interact-derive-%d", i)
			ac := newClass(acName)
			switch {
			case tc.declared != "":
				ac.Spec.Authz = &spiceboxv1alpha1.AuthzBlock{
					Session: &spiceboxv1alpha1.SessionAuthz{InteractPermission: tc.declared},
				}
			case tc.onlyStartersInteract:
				ac.Spec.Authz = &spiceboxv1alpha1.AuthzBlock{
					Session: &spiceboxv1alpha1.SessionAuthz{
						AllowedStarters:      []string{"user:abc123"},
						OnlyStartersInteract: true,
					},
				}
			}

			got := reconcileClass(t, ac, channelObjects(tc.channels(acName))...)

			cond := findCondition(got.Status.Conditions, spiceboxv1alpha1.AgentClassConditionValid)
			require.NotNil(t, cond, "Valid condition must be set")
			assert.Equal(t, tc.wantValid, cond.Status, "Valid status; reason=%s msg=%q", cond.Reason, cond.Message)
			assert.Equal(t, tc.wantDerived, got.Status.DerivedSessionInteractPermission,
				"status.derivedSessionInteractPermission")
			wantEffective := tc.wantEffective
			if tc.wantEffectiveIsStarterSet {
				wantEffective = got.StarterSubjectSet()
			}
			assert.Equal(t, wantEffective, got.EffectiveSessionInteractPermission(),
				"the value every consumer of the policy reads")

			if tc.wantValid == metav1.ConditionFalse {
				wantReason, wantMsg := tc.wantReason, tc.wantMsg
				if wantReason == "" {
					wantReason, wantMsg = spiceboxv1alpha1.ReasonAgentClassUserLessMissingAuthz, "interactPermission"
				}
				assert.Equal(t, wantReason, cond.Reason,
					"nothing to derive from must land on the EXISTING refusal")
				assert.Contains(t, cond.Message, wantMsg,
					"and it must still name the field a human has to fill in")
			}
		})
	}
}

// TestReconcile_DerivedInteractPermissionIsStampedEvenWhenInvalid pins that the
// derivation is independent of the OTHER half of the userless rule.
//
// A Channel missing spec.authzSubject keeps the class at Valid=False, and the
// message must still name that field — but the interact policy was derivable
// and is derived, so fixing the one field a human genuinely has to supply is
// enough. Stamping only on the way to Valid=True would make the two failures
// serialize into two round trips.
func TestReconcile_DerivedInteractPermissionIsStampedEvenWhenInvalid(t *testing.T) {
	const acName = "ac-interact-derive-no-subject"
	in := ghInput(acName+"-gh", acName)
	in.Spec.AuthzSubject = ""

	got := reconcileClass(t, newClass(acName), in, slackOut(acName+"-slack", acName, "C0DEMO123"))

	cond := findCondition(got.Status.Conditions, spiceboxv1alpha1.AgentClassConditionValid)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status, "msg=%q", cond.Message)
	assert.Contains(t, cond.Message, "authzSubject", "the refusal names the field still missing")
	assert.NotContains(t, cond.Message, "interactPermission",
		"the derived half is no longer missing and must not be reported as such")
	assert.Equal(t, derivedInteract, got.Status.DerivedSessionInteractPermission,
		"the derivation does not wait for the rest of the class to be valid")
}
