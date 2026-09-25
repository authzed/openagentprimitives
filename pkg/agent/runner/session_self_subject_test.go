package runner

// The session-as-principal default. service_subject_test.go pins the
// PRECEDENCE (requester, then starter, then the non-human principal); this
// file pins the principal's SHAPE, which is the half that fails silently.
//
// A self-subject derived a second way — bare name, a dash separator, a "user:"
// prefix — still looks right in a log and in an audit line, and matches no
// tuple anybody ever wrote. The symptom is a total deny that reads like a
// policy decision rather than a typo.

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

func selfSubjectSession(ns, name string) *spiceboxv1alpha1.AgentSession {
	return &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
	}
}

// TestTheSelfSubjectMatchesTheGrantItMustResolve is the load-bearing one.
//
// authz.SlotGrantRelation is what WRITES a per-resource grant to a session, and
// its subject id is "<ns>/<name>". The runner's acting subject has to be the
// same string or the grant can never resolve. Asserting against that function
// rather than against a literal is the point: if the grant's id format ever
// changes, this fails instead of the two drifting apart in production.
func TestTheSelfSubjectMatchesTheGrantItMustResolve(t *testing.T) {
	sess := selfSubjectSession("demo", "nightly-report")

	rel := authz.SlotGrantRelation("repository", "acme/widgets", "push",
		authz.SessionRef{Namespace: sess.Namespace, Name: sess.Name})
	got := sessionSelfSubject(sess)

	assert.Equal(t, "agentsession", rel.SubjectType,
		"precondition: a slot grant names an agentsession, which is why a session can hold one at all")
	assert.Equal(t, rel.SubjectType+":"+rel.SubjectID, got.String(),
		"the acting subject must be the exact principal a slot grant names, or every grant silently resolves to nothing")
}

// TestTheSelfSubjectSplitsIntoTheRightSpiceDBReference walks the value through
// the same accessor the tool-call gate uses (subjectObject cuts SubjectRef on
// the first colon), so the assertion is about what SpiceDB is actually asked.
func TestTheSelfSubjectSplitsIntoTheRightSpiceDBReference(t *testing.T) {
	got := sessionSelfSubject(selfSubjectSession("demo", "nightly-report"))

	ref := got.SubjectRef()
	assert.Equal(t, "agentsession", ref.ObjectType(),
		"identity.Subject must recognize this as a fully-qualified reference of its own type")

	typ, id, found := strings.Cut(string(ref), ":")
	assert.True(t, found)
	assert.Equal(t, "agentsession", typ)
	assert.Equal(t, "demo/nightly-report", id,
		"the namespace/name id, exactly as the lineage and slot-grant tuples spell it")
}

// TestTheSelfSubjectIsNeverTreatedAsAUser pins the trap check_tool_call.go's
// subjectObject comment exists to prevent.
//
// Prefixing "user:" unconditionally would produce object id
// "agentsession:demo/x", whose colon SpiceDB's object-id regex rejects — an
// InvalidArgument in place of an allow or a deny, which is unreadable to an
// operator and defeats the permissive/enforcing distinction entirely.
func TestTheSelfSubjectIsNeverTreatedAsAUser(t *testing.T) {
	got := sessionSelfSubject(selfSubjectSession("demo", "nightly-report"))

	_, err := got.SubjectRef().CanonicalUserID()
	assert.Error(t, err,
		"a session principal must fail the user-typed accessor rather than being silently unwrapped into one")
	assert.NotEqual(t, identity.Subject("user:"+got.String()), got.SubjectRef(),
		"it must never be prefixed as a user subject")
}

// TestASessionThatCannotNameItselfHasNoSubject.
//
// Empty means "no acting subject", which denies at dispatch. The forbidden
// outcome is a partial reference like "agentsession:demo/" — a principal that
// exists nowhere, which would deny anyway but send anyone reading the audit
// line hunting for grants on a subject that was never checked.
func TestASessionThatCannotNameItselfHasNoSubject(t *testing.T) {
	for _, sess := range []*spiceboxv1alpha1.AgentSession{
		nil,
		selfSubjectSession("", "nightly-report"),
		selfSubjectSession("demo", ""),
	} {
		assert.Empty(t, sessionSelfSubject(sess).String(),
			"an unnameable session must deny cleanly, never resolve to a partial principal")
	}
}

// TestTheClassGateStillBinds: the session default is reachable ONLY for a
// class whose input can legitimately be userless. An attributable input whose
// human went missing must still deny — substituting the session there would
// forge an actor to stand in for the person whose attribution was lost.
func TestTheClassGateStillBinds(t *testing.T) {
	sess := selfSubjectSession("demo", "nightly-report")

	attributable := &spiceboxv1alpha1.AgentClass{
		Status: spiceboxv1alpha1.AgentClassStatus{UserlessInput: false},
	}
	assert.Empty(t, serviceSubjectFor(sess, attributable).String(),
		"a lost human is not replaced by the session")

	userless := &spiceboxv1alpha1.AgentClass{
		Status: spiceboxv1alpha1.AgentClassStatus{UserlessInput: true},
	}
	assert.Equal(t, "agentsession:demo/nightly-report", serviceSubjectFor(sess, userless).String())

	assert.Empty(t, serviceSubjectFor(sess, nil).String(),
		"a nil class answers nothing, so it must not grant the session a principal")
}
