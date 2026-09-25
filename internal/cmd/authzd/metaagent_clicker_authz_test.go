// Tests for the metaagent approve/deny clicker authorization gate. Before
// authzd delivers a scope decision (DeliverDecision), it MUST confirm the
// clicker holds agentsession#manage_scope (= owner). Without this gate any
// thread viewer who can see the buttons could resolve the scope change.
package main

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// fakeManageScopeChecker records the canonical it was asked about and returns
// canned results, so the gate's allow/deny/error/nil handling is testable
// without a live SpiceDB.
type fakeManageScopeChecker struct {
	allowed  bool
	err      error
	gotNS    string
	gotName  string
	gotCanon identity.CanonicalUserID
	gotFC    bool
}

func (f *fakeManageScopeChecker) CheckManageScope(_ context.Context, ns, name string, canonicalID identity.CanonicalUserID, fc bool) (bool, error) {
	f.gotNS = ns
	f.gotName = name
	f.gotCanon = canonicalID
	f.gotFC = fc
	return f.allowed, f.err
}

func TestAuthorizeMetaagentClicker(t *testing.T) {
	cases := []struct {
		name    string
		checker authz.ManageScopeChecker
		want    bool
	}{
		{
			name:    "owner canonical: allowed → decision delivered",
			checker: &fakeManageScopeChecker{allowed: true},
			want:    true,
		},
		{
			name:    "non-owner canonical: denied → dropped",
			checker: &fakeManageScopeChecker{allowed: false},
			want:    false,
		},
		{
			name:    "check error: fail-closed denied → dropped",
			checker: &fakeManageScopeChecker{err: errors.New("spicedb down")},
			want:    false,
		},
		{
			// An absent checker must FAIL CLOSED here, matching what the
			// identical nil does in cold_start_pipeline's wiring
			// (hooks.MetaagentReceived denies). One condition must not yield
			// two opposite verdicts depending on wiring in another file. Not
			// reachable today — authzd os.Exit's without SpiceDB — but a
			// typed-nil interface compares != nil, so the disposition has to be
			// structural, not a property of how the checker was built.
			name:    "nil checker (gate not wired): denied fail-closed, matching the MetaagentReceived sibling",
			checker: nil,
			want:    false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := authorizeMetaagentClicker(context.Background(), tc.checker, "ns", "sess", "user:alice")
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestAuthorizeMetaagentClicker_PassesArgsThrough verifies the gate forwards
// the session ns/name + the clicker canonical to the checker, fully-consistent
// (a just-written owner tuple must be visible — same consistency the inbound
// manage_scope gate uses), and that it STRIPS the "user:" subject prefix
// channelsd publishes — CheckManageScope wants the bare canonical and prepends
// the type itself. Not stripping denies every legitimate owner.
func TestAuthorizeMetaagentClicker_PassesArgsThrough(t *testing.T) {
	fc := &fakeManageScopeChecker{allowed: true}
	got := authorizeMetaagentClicker(context.Background(), fc, "myns", "mysess", "user:owner-canon")
	assert.True(t, got)
	assert.Equal(t, "myns", fc.gotNS)
	assert.Equal(t, "mysess", fc.gotName)
	assert.Equal(t, identity.CanonicalFromTrusted("owner-canon", "test fixture"), fc.gotCanon, "must strip the user: prefix before CheckManageScope")
	assert.True(t, fc.gotFC, "manage_scope clicker check must be fully-consistent")
}
