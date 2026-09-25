package hooks_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/hooks"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
)

// TestInteract_Eval table-drives the interact outcomes. The hook holds a
// CheckInbound closure (wrapping the type-dispatching SpiceDB check) and
// decides Allow on true, Deny on false, and Deny (fail-closed) on a check
// error or an unwired checker.
func TestInteract_Eval(t *testing.T) {
	cases := []struct {
		name        string
		check       func(ctx context.Context, ns, name string, actor identity.Subject) (bool, error)
		wantVerdict pipeline.Verdict
		wantReason  bool // expect a non-empty reason
	}{
		{
			name:        "authorized: CheckInbound=true → Allow",
			check:       func(context.Context, string, string, identity.Subject) (bool, error) { return true, nil },
			wantVerdict: pipeline.Allow,
		},
		{
			name:        "not authorized: CheckInbound=false → Deny + reason",
			check:       func(context.Context, string, string, identity.Subject) (bool, error) { return false, nil },
			wantVerdict: pipeline.Deny,
			wantReason:  true,
		},
		{
			name: "backend error: → Deny (fail-closed) + reason",
			check: func(context.Context, string, string, identity.Subject) (bool, error) {
				return false, errors.New("spicedb down")
			},
			wantVerdict: pipeline.Deny,
			wantReason:  true,
		},
		{
			name:        "nil checker: → Deny (fail-closed)",
			check:       nil,
			wantVerdict: pipeline.Deny,
			wantReason:  true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := hooks.NewInteract(hooks.InteractDeps{CheckInbound: tc.check})
			dec := h.Eval(context.Background(), pipeline.Input{
				Point:     pipeline.InboundTurn,
				Session:   pipeline.SessionRef{Namespace: "ns", Name: "a"},
				Requester: identity.CanonicalFromTrusted("user:alice", "test fixture"),
				Turn:      &pipeline.TurnInfo{Text: "hello"},
			})
			assert.Equal(t, tc.wantVerdict, dec.Verdict)
			if tc.wantReason {
				assert.NotEmpty(t, dec.Reason)
			}
		})
	}
}

// TestInteract_PassesTheActorWithItsSubjectTypeIntact is the guard on the
// defect that made agent-to-agent inbound permanently undeliverable: the hook
// used to hand the check a bare canonical id, so a session acting subject
// arrived as an id with no type and the SpiceDB layer prefixed "user:" onto
// it — producing `user:agentsession:<ns>/<name>`, which SpiceDB rejects as
// InvalidArgument because an object id may not contain ':'.
//
// A bare human canonical must reach the check as "user:<canonical>", and an
// already-qualified session subject must reach it UNCHANGED.
func TestInteract_PassesTheActorWithItsSubjectTypeIntact(t *testing.T) {
	cases := []struct {
		name      string
		requester identity.CanonicalUserID
		wantActor identity.Subject
	}{
		{
			name:      "bare human canonical: qualified as a user subject",
			requester: identity.CanonicalFromTrusted("YWxpY2VAZXhhbXBsZS5jb20", "test fixture"),
			wantActor: "user:YWxpY2VAZXhhbXBsZS5jb20",
		},
		{
			name:      "delegating session: reaches the check as an agentsession subject",
			requester: identity.CanonicalFromTrusted("agentsession:default/parent-1", "test fixture"),
			wantActor: "agentsession:default/parent-1",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got identity.Subject
			h := hooks.NewInteract(hooks.InteractDeps{
				CheckInbound: func(_ context.Context, _, _ string, actor identity.Subject) (bool, error) {
					got = actor
					return true, nil
				},
			})
			dec := h.Eval(context.Background(), pipeline.Input{
				Point:     pipeline.InboundTurn,
				Session:   pipeline.SessionRef{Namespace: "default", Name: "child-1"},
				Requester: tc.requester,
				Turn:      &pipeline.TurnInfo{Text: "hi"},
			})
			require.Equal(t, pipeline.Allow, dec.Verdict, "the check answered true")
			assert.Equal(t, tc.wantActor, got)
		})
	}
}

// TestInteract_CheckErrorMarkedTransient verifies that a transient backend
// error is distinguishable from a definitive not-authorized deny so the
// channelsd pipeline can preserve its OutcomeInternalError semantics
// (drop/retry) rather than posting a permission request. The hook exposes the
// difference via Decision.Reason carrying a sentinel marker the host inspects.
func TestInteract_CheckErrorMarkedTransient(t *testing.T) {
	h := hooks.NewInteract(hooks.InteractDeps{
		CheckInbound: func(context.Context, string, string, identity.Subject) (bool, error) {
			return false, errors.New("spicedb unavailable")
		},
	})
	dec := h.Eval(context.Background(), pipeline.Input{
		Point:     pipeline.InboundTurn,
		Session:   pipeline.SessionRef{Namespace: "ns", Name: "a"},
		Requester: identity.CanonicalFromTrusted("user:alice", "test fixture"),
		Turn:      &pipeline.TurnInfo{Text: "hi"},
	})
	require.Equal(t, pipeline.Deny, dec.Verdict)
	assert.True(t, hooks.IsInteractCheckError(dec.Reason),
		"transient check-error deny must be distinguishable from a definitive not-authorized deny")

	denyOK := hooks.NewInteract(hooks.InteractDeps{
		CheckInbound: func(context.Context, string, string, identity.Subject) (bool, error) { return false, nil },
	}).Eval(context.Background(), pipeline.Input{Point: pipeline.InboundTurn, Turn: &pipeline.TurnInfo{Text: "hi"}})
	assert.False(t, hooks.IsInteractCheckError(denyOK.Reason),
		"a definitive not-authorized deny must NOT be marked as a transient check error")
}

// TestInteract_UnsupportedActorTypeDeniesDefinitivelyNotTransiently pins the
// one error the hook must NOT treat as retryable. An acting subject whose type
// has no inbound permission cannot be authorized by any tuple anyone could
// write, so marking it transient would make channelsd drop and re-deliver the
// same message forever — a per-message retry loop for a permanent condition.
func TestInteract_UnsupportedActorTypeDeniesDefinitivelyNotTransiently(t *testing.T) {
	h := hooks.NewInteract(hooks.InteractDeps{
		CheckInbound: func(context.Context, string, string, identity.Subject) (bool, error) {
			return false, fmt.Errorf("%w: %q", authz.ErrActorTypeUnsupported, "service:nightly")
		},
	})
	dec := h.Eval(context.Background(), pipeline.Input{
		Point:     pipeline.InboundTurn,
		Session:   pipeline.SessionRef{Namespace: "ns", Name: "a"},
		Requester: identity.CanonicalFromTrusted("service:nightly", "test fixture"),
		Turn:      &pipeline.TurnInfo{Text: "hi"},
	})
	require.Equal(t, pipeline.Deny, dec.Verdict)
	assert.False(t, hooks.IsInteractCheckError(dec.Reason),
		"an unsupported actor type is permanent, not a transient backend failure")
	assert.Contains(t, dec.Reason, "service:nightly", "the refusal must name the subject it refused")
}

// TestInteract_Points asserts the hook fires only at InboundTurn.
func TestInteract_Points(t *testing.T) {
	h := hooks.NewInteract(hooks.InteractDeps{})
	assert.Equal(t, []pipeline.Point{pipeline.InboundTurn}, h.Points())
	assert.Equal(t, "interact", h.Name())
}

var _ pipeline.Hook = hooks.NewInteract(hooks.InteractDeps{})
