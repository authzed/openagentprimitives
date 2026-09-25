package agentclass_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/agentclass"
	"github.com/authzed/openagentprimitives/pkg/controllers/testfixtures"
)

// reconcileSessionAuthz builds a fake cluster holding a class with the given
// session authz block, reconciles once, and returns the persisted object.
func reconcileSessionAuthz(t *testing.T, name string, s *spiceboxv1alpha1.SessionAuthz) spiceboxv1alpha1.AgentClass {
	t.Helper()
	ac := newClass(name)
	ac.Spec.Authz = &spiceboxv1alpha1.AuthzBlock{Session: s}
	objs := []client.Object{
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "llm-creds", Namespace: "default"},
			Data:       map[string][]byte{"api-key": []byte("sk-test")},
		},
		ac,
	}
	c := fakeclient.NewClientBuilder().
		WithScheme(testfixtures.NewScheme(t)).
		WithObjects(objs...).
		WithStatusSubresource(&spiceboxv1alpha1.AgentClass{}).
		Build()
	r := &agentclass.Reconciler{Client: c, APIReader: c}
	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "default", Name: name},
	})
	require.NoError(t, err, "Reconcile must not error")
	var got spiceboxv1alpha1.AgentClass
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: name}, &got))
	return got
}

func TestReconcile_ValidatesStartGateFields(t *testing.T) {
	no := false
	cases := []struct {
		name        string
		sess        *spiceboxv1alpha1.SessionAuthz
		wantInvalid bool
		wantMsg     string
	}{
		{
			name:        "well-formed user and group entries: Valid",
			sess:        &spiceboxv1alpha1.SessionAuthz{AllowedStarters: []string{"user:abc123", "group:eng#member"}},
			wantInvalid: false,
		},
		{
			name:        "malformed entry: SpecInvalid naming the entry",
			sess:        &spiceboxv1alpha1.SessionAuthz{AllowedStarters: []string{"alice@example.com"}},
			wantInvalid: true,
			wantMsg:     `allowedStarters entry "alice@example.com"`,
		},
		{
			name:        "a service subject is not a starter: SpecInvalid",
			sess:        &spiceboxv1alpha1.SessionAuthz{AllowedStarters: []string{"service:cron"}},
			wantInvalid: true,
			wantMsg:     `allowedStarters entry "service:cron"`,
		},
		{
			// subjectRE's id charset admits '@' and '.', so this passes
			// ValidateSubjectSet syntactically. It must still be rejected: a
			// starter check always resolves against an identity.CanonicalUserID
			// (the platform's own base64url form), so a raw email would be
			// written to SpiceDB and then never match anyone, ever.
			name:        "raw email as a user id: SpecInvalid",
			sess:        &spiceboxv1alpha1.SessionAuthz{AllowedStarters: []string{"user:alice@example.com"}},
			wantInvalid: true,
			wantMsg:     "never a raw email",
		},
		{
			// The three shapes ValidateSubjectSet accepts and the schema
			// cannot hold (`relation starter: user | group#member`). Each
			// would be sent in the SAME atomic WriteRelationships as the
			// DELETEs for starters the class just dropped, so SpiceDB
			// rejecting the batch would leave a removed starter's standing in
			// place while the class still read Valid=True.
			name:        "group with no relation: SpecInvalid naming the entry",
			sess:        &spiceboxv1alpha1.SessionAuthz{AllowedStarters: []string{"group:eng"}},
			wantInvalid: true,
			wantMsg:     `allowedStarters entry "group:eng"`,
		},
		{
			name:        "group with a relation other than member: SpecInvalid naming the entry",
			sess:        &spiceboxv1alpha1.SessionAuthz{AllowedStarters: []string{"group:eng#owner"}},
			wantInvalid: true,
			wantMsg:     `allowedStarters entry "group:eng#owner"`,
		},
		{
			name:        "user carrying a #relation: SpecInvalid naming the entry",
			sess:        &spiceboxv1alpha1.SessionAuthz{AllowedStarters: []string{"user:abc123#member"}},
			wantInvalid: true,
			wantMsg:     `allowedStarters entry "user:abc123#member"`,
		},
		{
			name:        "platformAdminsMayStart false with no list: SpecInvalid (nobody could start)",
			sess:        &spiceboxv1alpha1.SessionAuthz{PlatformAdminsMayStart: &no},
			wantInvalid: true,
			wantMsg:     "platformAdminsMayStart: false requires",
		},
		{
			name:        "onlyStartersInteract with no list: SpecInvalid",
			sess:        &spiceboxv1alpha1.SessionAuthz{OnlyStartersInteract: true},
			wantInvalid: true,
			wantMsg:     "onlyStartersInteract requires",
		},
		{
			name: "onlyStartersInteract with interactPermission: SpecInvalid (mutually exclusive)",
			sess: &spiceboxv1alpha1.SessionAuthz{
				AllowedStarters: []string{"user:abc123"}, OnlyStartersInteract: true, InteractPermission: "group:all#member",
			},
			wantInvalid: true,
			wantMsg:     "mutually exclusive",
		},
		{
			name:        "onlyStartersInteract with a list and no widening: Valid",
			sess:        &spiceboxv1alpha1.SessionAuthz{AllowedStarters: []string{"user:abc123"}, OnlyStartersInteract: true},
			wantInvalid: false,
		},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := reconcileSessionAuthz(t, "sg-"+string(rune('a'+i)), tc.sess)
			cond := findCondition(got.Status.Conditions, spiceboxv1alpha1.AgentClassConditionValid)
			require.NotNil(t, cond, "Valid condition must be set")
			if tc.wantInvalid {
				assert.Equal(t, metav1.ConditionFalse, cond.Status)
				assert.Equal(t, spiceboxv1alpha1.ReasonSpecInvalid, cond.Reason)
				assert.Contains(t, cond.Message, tc.wantMsg)
				return
			}
			assert.Equal(t, metav1.ConditionTrue, cond.Status, "message: %s", cond.Message)
		})
	}
}

// TestReconcile_StartGateAndAUserlessInputCannotCoexist pins the combination
// that reads Valid=True and can never start a single session: every arm of the
// start gate resolves a PERSON, and an input Channel that carries none asserts
// a service subject, so EnforceStartGate refuses every session structurally,
// forever. It is a fact about the class's BINDINGS, not its spec alone, which
// is why the check runs below the Channel walk that derives status.userlessInput.
//
// The control case is the load-bearing half: the same allowlist over a
// human-attributed input must stay Valid, or the rule would simply have banned
// allowedStarters.
func TestReconcile_StartGateAndAUserlessInputCannotCoexist(t *testing.T) {
	gated := func(name string) *spiceboxv1alpha1.AgentClass {
		ac := newClass(name)
		ac.Spec.Authz = &spiceboxv1alpha1.AuthzBlock{Session: &spiceboxv1alpha1.SessionAuthz{
			AllowedStarters: []string{"user:abc123"},
		}}
		return ac
	}

	t.Run("userless input: Valid=False naming the contradiction", func(t *testing.T) {
		const name = "sg-userless"
		got := reconcileClass(t, gated(name), ghInput(name+"-gh", name), slackOut(name+"-slack", name, "C0DEMO123"))
		cond := findCondition(got.Status.Conditions, spiceboxv1alpha1.AgentClassConditionValid)
		require.NotNil(t, cond)
		assert.Equal(t, metav1.ConditionFalse, cond.Status, "message: %s", cond.Message)
		assert.Equal(t, spiceboxv1alpha1.ReasonSpecInvalid, cond.Reason)
		assert.Contains(t, cond.Message, "allowedStarters")
		assert.Contains(t, cond.Message, "carries no person")
		assert.True(t, got.Status.UserlessInput,
			"the fact the refusal turns on must be published, not merely consulted")
	})

	t.Run("human-attributed input: the same allowlist is Valid", func(t *testing.T) {
		const name = "sg-humanin"
		in := &spiceboxv1alpha1.Channel{
			ObjectMeta: metav1.ObjectMeta{Name: name + "-slack-in", Namespace: "default"},
			Spec: spiceboxv1alpha1.ChannelSpec{
				Kind: "slack", Role: spiceboxv1alpha1.ChannelRoleInput, AgentClass: name,
				Slack: &spiceboxv1alpha1.SlackChannelConfig{},
			},
		}
		got := reconcileClass(t, gated(name), in)
		cond := findCondition(got.Status.Conditions, spiceboxv1alpha1.AgentClassConditionValid)
		require.NotNil(t, cond)
		assert.Equal(t, metav1.ConditionTrue, cond.Status, "message: %s", cond.Message)
	})
}

// The real onlyStartersInteract-suppresses-derivation case — with a Channel
// pair that WOULD otherwise derive something — lives in
// TestReconcile_DerivesSessionInteractPermission
// (interact_permission_derivation_test.go), alongside the declared-
// interactPermission sibling it mirrors. A version of this test with no bound
// Channels was removed: derivedSessionInteractPermission already returns ""
// with no Channels regardless of the OnlyStartersInteract guard, so that
// shape could never fail if the guard were reverted.
