package subagentrequest

// Unit tests for Task 1's ONE relaxation of the roster gate (R1): a
// workshop child's class is authored inside W at build time, so it can
// never be a literal member of the builder's fixed roster in B. These run
// against a fake client, not envtest -- unlike
// controller_crossnamespace_integration_test.go, admitCrossNamespaceParent's
// own admission (proven against real SpiceDB there) is not what this test
// exercises. Here the WorkshopBuildChecker is a trivial fixture always
// returning true, so the roster gate itself -- childClassInWorkshop's own
// Get against sr.Namespace -- is what's under test.

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
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// alwaysAllowWorkshopBuild is a WorkshopBuildChecker fixture standing in for
// a genuine workshop:<W>#build SpiceDB tuple. admitCrossNamespaceParent's
// own admission decision is proven against the real backend in
// controller_crossnamespace_integration_test.go; this file's fixture only
// needs that outcome held so a workshop-admitted request reaches the roster
// gate this task changes -- childClassInWorkshop reads nothing SpiceDB owns.
type alwaysAllowWorkshopBuild struct{}

func (alwaysAllowWorkshopBuild) CheckWorkshopBuild(_ context.Context, _, _, _ string) (bool, error) {
	return true, nil
}

// wsRosterNamespace builds the workshop namespace W, labeled for the
// session (sessNS/sessName) it was provisioned for. Mirrors
// controller_crossnamespace_integration_test.go's xnsNamespace under a
// distinct name: that file's helpers are only in scope with
// -tags=integration, and this test must also build without it.
func wsRosterNamespace(name, sessNS, sessName string) *corev1.Namespace {
	return &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				v1.LabelWorkshopSessionNamespace: sessNS,
				v1.LabelWorkshopSessionName:      sessName,
			},
		},
	}
}

// wsRosterClassInNamespace builds a leaf AgentClass in an arbitrary
// namespace. childClass (controller_test.go) always places it in "ns",
// which cannot stand in for a class authored inside the workshop namespace
// W -- the whole fact this task's relaxation depends on.
func wsRosterClassInNamespace(ns, name string) *v1.AgentClass {
	return &v1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec:       v1.AgentClassSpec{IdentityMode: v1.IdentityModeAgent},
	}
}

// wsRosterCrossNamespaceRequest builds a SubagentRequest IN ns naming a
// parent that may live in a different namespace entirely. request
// (controller_test.go) hardcodes both the request and its parent into "ns",
// which cannot express a workshop cross-namespace shape.
func wsRosterCrossNamespaceRequest(ns, name, parentNS, parentName, class string) *v1.SubagentRequest {
	return &v1.SubagentRequest{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: v1.SubagentRequestSpec{
			Parent: v1.NamespacedRef{Namespace: parentNS, Name: parentName},
			Class:  class,
			Task:   "run the workshop's test",
		},
	}
}

// TestRosterWorkshop pins Task 1's relaxation: the roster check must admit
// a workshop-admitted request whose class genuinely resolves as an
// AgentClass in W (case A), still deny one whose class does not exist there
// (case B), and never leak the relaxation to an ordinary same-namespace
// request that is genuinely off its parent's roster (case C) -- the
// relaxation is scoped to a request already admitted through
// admitCrossNamespaceParent, not to "any class that happens to exist
// somewhere."
func TestRosterWorkshop(t *testing.T) {
	const (
		builderNS    = "ns"               // B
		builderName  = "ws-roster-parent" // X
		builderClass = "ws-roster-builder-class"
		wsNS         = "ws-roster-workshop" // W
		childClsName = "ws-roster-child-class"
	)

	cases := []struct {
		name             string
		sameNamespace    bool // case C: request and parent share a namespace
		childClassInW    bool // whether to create the child AgentClass in W
		wantDeniedReason string
	}{
		{
			name:             "workshop child, class in W: passes the roster gate",
			childClassInW:    true,
			wantDeniedReason: "",
		},
		{
			name:             "workshop child, class absent from W: OffRoster",
			childClassInW:    false,
			wantDeniedReason: "OffRoster",
		},
		{
			name:             "same-namespace off-roster request: still OffRoster",
			sameNamespace:    true,
			wantDeniedReason: "OffRoster",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sch := newScheme(t)
			require.NoError(t, corev1.AddToScheme(sch))

			// The builder's own roster is deliberately EMPTY: a workshop
			// child's class is authored inside W at build time, so it can
			// never be a literal member of this fixed list.
			objs := []client.Object{
				parentClass(t, builderClass),
				parentSession(t, builderName, builderClass),
			}

			var reqNS, reqName string
			if tc.sameNamespace {
				reqNS = builderNS
				reqName = "req-roster-same-ns"
				objs = append(objs,
					childClass(t, childClsName), // exists, but never on the roster
					request(t, reqName, builderName, childClsName),
				)
			} else {
				reqNS = wsNS
				reqName = "req-roster-workshop"
				objs = append(objs, wsRosterNamespace(wsNS, builderNS, builderName))
				if tc.childClassInW {
					objs = append(objs, wsRosterClassInNamespace(wsNS, childClsName))
				}
				objs = append(objs, wsRosterCrossNamespaceRequest(wsNS, reqName, builderNS, builderName, childClsName))
			}

			c := fake.NewClientBuilder().
				WithScheme(sch).
				WithObjects(objs...).
				WithStatusSubresource(&v1.SubagentRequest{}, &v1.AgentSession{}).
				Build()
			r := &Reconciler{
				Client:             c,
				Scheme:             sch,
				Authz:              &fakeAuthz{},
				WorkshopBuild:      alwaysAllowWorkshopBuild{},
				MaxDelegationDepth: 3,
			}

			_, err := r.Reconcile(context.Background(), ctrl.Request{
				NamespacedName: types.NamespacedName{Namespace: reqNS, Name: reqName},
			})
			require.NoError(t, err, "a policy refusal or admission is a recorded result, not a reconcile error")

			var got v1.SubagentRequest
			require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: reqNS, Name: reqName}, &got))

			if tc.wantDeniedReason == "" {
				// This task owns the roster gate only -- the request may
				// still be denied at a LATER gate (mode/ceiling), so only
				// the roster reason is asserted away here, not full success.
				assert.NotEqual(t, "OffRoster", got.Status.FailureReason,
					"a workshop child whose class genuinely resolves in W must pass the roster gate")
			} else {
				assert.Equal(t, v1.SubagentRequestPhaseDenied, got.Status.Phase)
				assert.Equal(t, tc.wantDeniedReason, got.Status.FailureReason)
			}
		})
	}
}

// TestRosterWorkshop_ModeCeilingIsTheWorkshopsTestingSurface pins the gate
// TestRosterWorkshop left for later. R1 admits a workshop child past the
// roster, but the mode ceiling still read the builder's roster, where a class
// authored in W at build time is never named — so PermittedSubagentModes
// answered nil and EVERY mode was refused, attended and single_turn alike,
// and with them the one workshop tool that creates these requests
// (test_tool). Observed live on oap-desktop 2026-09-13: a finished, valid
// build could not be rehearsed at all. A workshop child's ceiling is the
// workshop's own testing surface, and nothing wider: single_turn is what
// test_tool asks for; attended stays on the ceiling as headroom for a
// delegation mode a workshop child would use, though no workshop tool
// requests it today; task and chat stay refused, because the workshop
// rehearses a build, it does not run it.
func TestRosterWorkshop_ModeCeilingIsTheWorkshopsTestingSurface(t *testing.T) {
	const (
		builderNS    = "ns"
		builderName  = "ws-mode-parent"
		builderClass = "ws-mode-builder-class"
		wsNS         = "ws-mode-workshop"
		childClsName = "ws-mode-child-class"
	)
	cases := []struct {
		name       string
		mode       string
		wantDenied bool
	}{
		{name: "attended: permitted as headroom", mode: v1.SubagentModeAttended},
		{name: "single_turn (test_tool): permitted", mode: v1.SubagentModeSingleTurn},
		{name: "task: refused — the workshop rehearses a build, it does not run it", mode: v1.SubagentModeTask, wantDenied: true},
		{name: "chat: refused", mode: v1.SubagentModeChat, wantDenied: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sch := newScheme(t)
			require.NoError(t, corev1.AddToScheme(sch))
			req := wsRosterCrossNamespaceRequest(wsNS, "req-mode-workshop", builderNS, builderName, childClsName)
			req.Spec.Mode = tc.mode
			c := fake.NewClientBuilder().
				WithScheme(sch).
				WithObjects(
					parentClass(t, builderClass), // roster deliberately empty
					parentSession(t, builderName, builderClass),
					wsRosterNamespace(wsNS, builderNS, builderName),
					wsRosterClassInNamespace(wsNS, childClsName),
					req,
				).
				WithStatusSubresource(&v1.SubagentRequest{}, &v1.AgentSession{}).
				Build()
			r := &Reconciler{Client: c, Scheme: sch, Authz: &fakeAuthz{}, WorkshopBuild: alwaysAllowWorkshopBuild{}, MaxDelegationDepth: 3}
			_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: wsNS, Name: req.Name}})
			require.NoError(t, err)
			var got v1.SubagentRequest
			require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: wsNS, Name: req.Name}, &got))
			if tc.wantDenied {
				assert.Equal(t, v1.SubagentRequestPhaseDenied, got.Status.Phase)
				assert.Equal(t, "SubagentModeNotPermitted", got.Status.FailureReason)
			} else {
				assert.NotEqual(t, "SubagentModeNotPermitted", got.Status.FailureReason,
					"a workshop child's %s request must clear the mode ceiling", tc.mode)
			}
		})
	}
}
