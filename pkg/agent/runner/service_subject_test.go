// pkg/agent/runner/service_subject_test.go
//
// Regression tests for the webhook-session tool-call subject: a session whose
// inbound carried no human (a GitHub webhook, a cron tick) has neither a
// current-requester annotation nor a started_by, so ResolveAuthSubjects used
// to leave l.authSubject EMPTY. An empty subject is not a denial — it is a
// MALFORMED SpiceDB request (`subject.object.object_id` must match
// `[a-zA-Z0-9/_|\-=+]{1,}`), which reads as an infrastructure fault in the
// log, tells an operator nothing about authorization, and defeats the
// permissive/enforcing distinction because the check never happens.
//
// The final fallback is the session's own service subject, carried from
// Channel.spec.authzSubject at session creation. See ResolveAuthSubjects.
package runner

import (
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/slack"
)

// fixture subjects. Made-up names: no test may carry a real person's or
// organization's identity (AGENTS.md).
const (
	fixtureServiceSubject = "service:demo-reviewbot-github"
	fixtureRequester      = "cmVxdWVzdGVyQGV4YW1wbGUudGVzdA"
	fixtureStarter        = "c3RhcnRlckBleGFtcGxlLnRlc3Q"
)

// classForSubjectResolution builds the AgentClass ResolveAuthSubjects reads:
// the tool-call subject MODE from spec, and the derived "this class's input
// carries no human" fact from status (the same bool the AgentClass reconciler
// publishes, never re-derived here).
func classForSubjectResolution(t *testing.T, mode string, userlessInput bool) *spiceboxv1alpha1.AgentClass {
	t.Helper()
	return &spiceboxv1alpha1.AgentClass{
		Spec: spiceboxv1alpha1.AgentClassSpec{
			Authz: &spiceboxv1alpha1.AuthzBlock{
				ToolCalls: &spiceboxv1alpha1.ToolCallsAuthz{Subject: mode},
			},
		},
		Status: spiceboxv1alpha1.AgentClassStatus{UserlessInput: userlessInput},
	}
}

// sessionWithAnnotations builds an AgentSession carrying exactly the
// annotations given, dropping empty values so a row can say "this one is
// absent" by leaving it "".
func sessionWithAnnotations(t *testing.T, currentRequester, startedBy, serviceSubject string) *spiceboxv1alpha1.AgentSession {
	t.Helper()
	ann := map[string]string{}
	if currentRequester != "" {
		ann[slack.LastInboundCanonicalIDAnnotationKey] = currentRequester
	}
	if startedBy != "" {
		ann[spiceboxv1alpha1.AnnotationStartedByCanonicalID] = "user:" + startedBy
	}
	if serviceSubject != "" {
		ann[spiceboxv1alpha1.AnnotationAuthzServiceSubject] = serviceSubject
	}
	return &spiceboxv1alpha1.AgentSession{ObjectMeta: metav1.ObjectMeta{
		Name: "demo-session", Namespace: "default", Annotations: ann,
	}}
}

// TestResolveAuthSubjects_ServiceSubjectIsTheFinalFallback pins the precedence
// the tool-call gate authorizes against: the live requester first, the
// session's human starter next, and only then the non-human subject the input
// Channel declared. Every row asserts the SAME field the SpiceDB check reads.
func TestResolveAuthSubjects_ServiceSubjectIsTheFinalFallback(t *testing.T) {
	cases := []struct {
		name             string
		mode             string
		userlessInput    bool
		currentRequester string
		startedBy        string
		serviceSubject   string
		want             string
	}{
		{
			name:           "webhook session (no human anywhere): subject is the Channel's service subject",
			mode:           "currentRequester",
			userlessInput:  true,
			serviceSubject: fixtureServiceSubject,
			want:           fixtureServiceSubject,
		},
		{
			name:             "human requester present: the requester still wins over the service subject",
			mode:             "currentRequester",
			userlessInput:    true,
			currentRequester: fixtureRequester,
			serviceSubject:   fixtureServiceSubject,
			want:             fixtureRequester,
		},
		{
			name:           "human starter present, no live requester: started_by wins over the service subject",
			mode:           "currentRequester",
			userlessInput:  true,
			startedBy:      fixtureStarter,
			serviceSubject: fixtureServiceSubject,
			want:           fixtureStarter,
		},
		{
			name:           "startedBy mode, no human starter: falls back to the service subject",
			mode:           "startedBy",
			userlessInput:  true,
			serviceSubject: fixtureServiceSubject,
			want:           fixtureServiceSubject,
		},
		{
			name:           "both mode, no human on either leg: falls back to the service subject",
			mode:           "both",
			userlessInput:  true,
			serviceSubject: fixtureServiceSubject,
			want:           fixtureServiceSubject,
		},
		{
			name:           "attributable input (a human was expected and is missing): stays empty, denies cleanly",
			mode:           "currentRequester",
			userlessInput:  false,
			serviceSubject: fixtureServiceSubject,
			want:           "",
		},
		{
			// THE DEFAULT, and the row whose expectation this decision changed.
			// It used to be "" — a userless session declaring no service
			// principal had no acting subject at all and denied every governed
			// call. It now acts as ITSELF, because the session is a principal
			// with a lifecycle where a wizard-typed service name was not.
			//
			// This grants nothing on its own: standing still requires someone
			// to write a slot grant naming this session. What it changes is
			// that such a grant is now WRITABLE, and that the principal it
			// names expires with the work it was issued for.
			name:          "no declared service: the session acts as ITSELF, the default",
			mode:          "currentRequester",
			userlessInput: true,
			want:          "agentsession:default/demo-session",
		},
		{
			// The override, stated as its own row so the precedence cannot be
			// inverted by a later edit without failing here. A declared
			// service is not a legacy path: several sessions of one class
			// sharing ONE durable principal is a real thing to want, and a
			// per-session principal cannot express it.
			name:           "a declared service subject OVERRIDES the session default",
			mode:           "currentRequester",
			userlessInput:  true,
			serviceSubject: fixtureServiceSubject,
			want:           fixtureServiceSubject,
		},
		{
			// The class gate still binds, and this is the row that proves the
			// flip did not weaken it. An ATTRIBUTABLE input whose requester
			// went missing means attribution was LOST — substituting the
			// session for the human who should have been there would forge an
			// actor, so it must still deny.
			name:             "attributable input, requester missing: still empty — a lost human is not replaced by the session",
			mode:             "currentRequester",
			userlessInput:    false,
			currentRequester: "",
			want:             "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := &Loop{AuthzCli: noopAuthzClient{}}
			l.ResolveAuthSubjects(
				sessionWithAnnotations(t, tc.currentRequester, tc.startedBy, tc.serviceSubject),
				classForSubjectResolution(t, tc.mode, tc.userlessInput),
			)
			assert.Equal(t, tc.want, l.AuthSubject(), "resolved tool-call auth subject")
		})
	}
}

// TestResolveAuthSubjects_ServiceSubjectDoesNotJoinTheBothModeSubjectList
// pins that the fallback stamps the ACTING principal only. "both" mode's
// authSubjects is the authorization SET, and every element of it is a human
// leg (current requester + started_by); a service subject appended there
// would make the set claim a human standing nobody granted. With the set
// empty the check reads the singular Subject, which is the fallback.
func TestResolveAuthSubjects_ServiceSubjectDoesNotJoinTheBothModeSubjectList(t *testing.T) {
	l := &Loop{AuthzCli: noopAuthzClient{}}
	l.ResolveAuthSubjects(
		sessionWithAnnotations(t, "", "", fixtureServiceSubject),
		classForSubjectResolution(t, "both", true),
	)
	assert.Equal(t, fixtureServiceSubject, l.AuthSubject(), "acting principal")
	assert.Empty(t, l.authSubjects, "both-mode subject SET carries only human legs")
}
