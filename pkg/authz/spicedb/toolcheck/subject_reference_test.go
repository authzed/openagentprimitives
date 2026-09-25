package toolcheck_test

import (
	"context"
	"testing"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb/toolcheck"
)

// readonlyRepoPermission is the shared Permission every case below checks:
// one Readonly resource whose id resolves straight out of the args.
func readonlyRepoPermission() authz.Permission {
	return authz.Permission{
		StateImpact: authz.Readonly,
		Check: &authz.PermissionCheck{
			ResourceType: "github_repo", ResourceIDTemplate: "{repo}", Permission: "read",
		},
	}
}

// TestCheck_NoActingSubject_DeniesWithoutReachingSpiceDB is the regression for
// the malformed-request class: a session with no resolved subject used to
// build a CheckPermissionRequest whose subject object_id was the empty string,
// which SpiceDB rejects with `subject.object.object_id: does not match regex`.
//
// That is strictly worse than a denial. It reads as an infrastructure fault,
// it names no principal an operator can grant anything to, and under
// permissive mode it defeats the whole point of the mode: the log says "would
// deny in enforcing mode" for a check that never evaluated, so it cannot say
// what enforcing WOULD have done.
func TestCheck_NoActingSubject_DeniesWithoutReachingSpiceDB(t *testing.T) {
	cases := []struct {
		name string
		in   authz.Inputs
	}{
		{
			name: "empty singular subject: denied, no SpiceDB call",
			in:   authz.Inputs{Args: map[string]any{"repo": "platform"}, Subject: ""},
		},
		{
			name: "subject list of empty strings: denied, no SpiceDB call",
			in:   authz.Inputs{Args: map[string]any{"repo": "platform"}, Subjects: []string{"", ""}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cli := &fakeSpiceDB{answer: v1.CheckPermissionResponse_PERMISSIONSHIP_HAS_PERMISSION}
			res := toolcheck.Checker{Cli: cli}.CheckToolCall(context.Background(), readonlyRepoPermission(), tc.in)

			assert.Equal(t, authz.OutcomeDenied, res.Outcome, "no subject must fail CLOSED")
			assert.Empty(t, cli.checkCalls,
				"a subject-less check must never reach SpiceDB — a malformed request is not an authorization answer")
			assert.Contains(t, res.Message, "no acting subject",
				"the denial must name the missing subject, not a regex")
		})
	}
}

// TestCheck_SubjectObjectTypeFollowsTheSubjectItself pins that a subject which
// already carries its own SpiceDB object type keeps it. The tool-call gate's
// subject is a CanonicalUserID, and a canonical that came from an inbound with
// no starting user is the input Channel's declared subject verbatim —
// "service:<id>", not a base64 user canonical. Prefixing "user:" onto that
// yields object_id "service:<id>", whose colon SpiceDB's object-id regex
// rejects: the same malformed request the empty subject produced, just with a
// different field named in the error.
//
// A bare canonical has no type of its own (base64url contains no colon) and
// must still be a user, which is every human subject in the system.
func TestCheck_SubjectObjectTypeFollowsTheSubjectItself(t *testing.T) {
	cases := []struct {
		name         string
		subject      string
		wantType     string
		wantObjectID string
	}{
		{
			name:         "base64 email canonical: user",
			subject:      "cmV2aWV3ZXJAZXhhbXBsZS50ZXN0",
			wantType:     "user",
			wantObjectID: "cmV2aWV3ZXJAZXhhbXBsZS50ZXN0",
		},
		{
			name:         "synthetic kind:team:externalID canonical: user",
			subject:      "c2xhY2s6VDAxNjpVMDE3WEpKUUQ3QQ",
			wantType:     "user",
			wantObjectID: "c2xhY2s6VDAxNjpVMDE3WEpKUUQ3QQ",
		},
		{
			name:         "webhook session's service subject: service, id without the type",
			subject:      "service:demo-reviewbot-github",
			wantType:     "service",
			wantObjectID: "demo-reviewbot-github",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cli := &fakeSpiceDB{answer: v1.CheckPermissionResponse_PERMISSIONSHIP_HAS_PERMISSION}
			res := toolcheck.Checker{Cli: cli}.CheckToolCall(context.Background(), readonlyRepoPermission(),
				authz.Inputs{Args: map[string]any{"repo": "platform"}, Subject: tc.subject})

			assert.Equal(t, authz.OutcomeAllowed, res.Outcome)
			require.Len(t, cli.checkCalls, 1, "expected exactly one SpiceDB call")
			got := cli.checkCalls[0].GetSubject().GetObject()
			assert.Equal(t, tc.wantType, got.GetObjectType(), "subject object type")
			assert.Equal(t, tc.wantObjectID, got.GetObjectId(), "subject object id")
		})
	}
}

// TestCheck_DenyMessageNamesTheSubjectAsSpiceDBSawIt keeps the denial readable
// for a non-user subject: rendering it as "user:service:<id>" would name a
// principal that does not exist, and an operator who then went looking for
// that subject's grants would find nothing and learn nothing.
func TestCheck_DenyMessageNamesTheSubjectAsSpiceDBSawIt(t *testing.T) {
	cli := &fakeSpiceDB{answer: v1.CheckPermissionResponse_PERMISSIONSHIP_NO_PERMISSION}
	res := toolcheck.Checker{Cli: cli}.CheckToolCall(context.Background(), readonlyRepoPermission(),
		authz.Inputs{Args: map[string]any{"repo": "platform"}, Subject: "service:demo-reviewbot-github"})

	assert.Equal(t, authz.OutcomeDenied, res.Outcome)
	assert.Contains(t, res.Message, "service:demo-reviewbot-github", "the denial names the acting subject")
	assert.NotContains(t, res.Message, "user:service:", "never double-prefix a subject that carries its own type")
}
