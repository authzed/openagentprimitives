package v1alpha1_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

func classWithSession(s *spiceboxv1alpha1.SessionAuthz) *spiceboxv1alpha1.AgentClass {
	return &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "demo-agent"},
		Spec: spiceboxv1alpha1.AgentClassSpec{
			Authz: &spiceboxv1alpha1.AuthzBlock{Session: s},
		},
	}
}

func TestStartGatePermission(t *testing.T) {
	no := false
	yes := true
	cases := []struct {
		name string
		sess *spiceboxv1alpha1.SessionAuthz
		want string
	}{
		{name: "no session block: no gate", sess: nil, want: ""},
		{name: "empty allowlist: no gate", sess: &spiceboxv1alpha1.SessionAuthz{}, want: ""},
		{name: "allowlist, admins may start (default): start_session",
			sess: &spiceboxv1alpha1.SessionAuthz{AllowedStarters: []string{"user:abc"}},
			want: spiceboxv1alpha1.AgentClassPermissionStartSession},
		{name: "allowlist, admins may start (explicit true): start_session",
			sess: &spiceboxv1alpha1.SessionAuthz{AllowedStarters: []string{"user:abc"}, PlatformAdminsMayStart: &yes},
			want: spiceboxv1alpha1.AgentClassPermissionStartSession},
		{name: "allowlist, admins may NOT start: start_explicit",
			sess: &spiceboxv1alpha1.SessionAuthz{AllowedStarters: []string{"group:eng#member"}, PlatformAdminsMayStart: &no},
			want: spiceboxv1alpha1.AgentClassPermissionStartExplicit},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, classWithSession(tc.sess).StartGatePermission())
		})
	}
	assert.Empty(t, (*spiceboxv1alpha1.AgentClass)(nil).StartGatePermission(), "nil receiver is no gate")
}

func TestStarterSubjectSet(t *testing.T) {
	ac := classWithSession(nil)
	assert.Equal(t, "agentclass:default/demo-agent#starter", ac.StarterSubjectSet())
}

// onlyStartersInteract WINS over both the declared and the derived interact
// permission: the whole point of the field is that the interact set cannot be
// widened by either.
func TestEffectiveSessionInteractPermission_OnlyStartersInteract(t *testing.T) {
	ac := classWithSession(&spiceboxv1alpha1.SessionAuthz{
		AllowedStarters:      []string{"user:abc"},
		OnlyStartersInteract: true,
		InteractPermission:   "group:everyone#member",
	})
	ac.Status.DerivedSessionInteractPermission = "slack_channel:C1#member"
	assert.Equal(t, "agentclass:default/demo-agent#starter", ac.EffectiveSessionInteractPermission())

	ac.Spec.Authz.Session.OnlyStartersInteract = false
	assert.Equal(t, "group:everyone#member", ac.EffectiveSessionInteractPermission(),
		"without the flag the declared value wins, unchanged from before")
}
