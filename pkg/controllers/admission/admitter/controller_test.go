package admitter

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/admission"
)

const testClass = "demo-agent"

var (
	baseTime = time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
	passTime = baseTime.Add(time.Hour)
)

// oneCPUDemand is the pod set every test session needs: one 1-CPU pod.
func oneCPUDemand(_ context.Context, sess *spiceboxv1alpha1.AgentSession) (admission.Demand, error) {
	return admission.Demand{Pods: []admission.PodDemand{{Name: sess.Name + "-runner", CPUMilli: 1000, MemBytes: 1 << 20}}}, nil
}

// roomFor is a CapacitySource with one node that fits n oneCPUDemand pod sets.
type roomFor int

func (n roomFor) Snapshot(context.Context) (admission.CapacityModel, error) {
	node := corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-a"},
		Status: corev1.NodeStatus{Allocatable: corev1.ResourceList{
			corev1.ResourceCPU:    *resource.NewMilliQuantity(int64(n)*1000, resource.DecimalSI),
			corev1.ResourceMemory: resource.MustParse("1Gi"),
			corev1.ResourcePods:   resource.MustParse("110"),
		}},
	}
	return admission.NewNodeModel([]corev1.Node{node}, nil), nil
}

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	sch := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(sch), "core scheme")
	require.NoError(t, spiceboxv1alpha1.AddToScheme(sch), "spicebox scheme")
	return sch
}

// waitingSession is a booting, never-admitted session created at
// baseTime+offset that reached the admission gate at once
// (Admitted=False/Queued at creation).
func waitingSession(name string, offset time.Duration) *spiceboxv1alpha1.AgentSession {
	return &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "default",
			CreationTimestamp: metav1.NewTime(baseTime.Add(offset)),
		},
		Spec: spiceboxv1alpha1.AgentSessionSpec{Class: testClass},
		Status: spiceboxv1alpha1.AgentSessionStatus{
			Phase: spiceboxv1alpha1.AgentSessionPhasePending,
			Conditions: []metav1.Condition{{
				Type: spiceboxv1alpha1.AgentSessionConditionAdmitted, Status: metav1.ConditionFalse,
				Reason:             spiceboxv1alpha1.ReasonAdmissionQueued,
				LastTransitionTime: metav1.NewTime(baseTime.Add(offset)),
			}},
		},
	}
}

// newReconciler builds an admitter over a fake client holding objs, counting
// every status patch it sends.
func newReconciler(t *testing.T, enabled bool, capacity admission.CapacitySource, objs ...client.Object) (*Reconciler, client.Client, *atomic.Int32) {
	t.Helper()
	var statusPatches atomic.Int32
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(objs...).
		WithStatusSubresource(&spiceboxv1alpha1.AgentSession{}).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourcePatch: func(ctx context.Context, cl client.Client, sub string, obj client.Object, p client.Patch, opts ...client.SubResourcePatchOption) error {
				statusPatches.Add(1)
				return cl.SubResource(sub).Patch(ctx, obj, p, opts...)
			},
		}).
		Build()
	r := &Reconciler{
		Client:   c,
		Capacity: capacity,
		Enabled:  enabled,
		Now:      func() time.Time { return passTime },
		Demand:   oneCPUDemand,
	}
	return r, c, &statusPatches
}

func runPass(t *testing.T, r *Reconciler) reconcile.Result {
	t.Helper()
	res, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: Key})
	require.NoError(t, err, "Reconcile")
	return res
}

func getSession(t *testing.T, c client.Client, name string) *spiceboxv1alpha1.AgentSession {
	t.Helper()
	var s spiceboxv1alpha1.AgentSession
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: name}, &s), "Get %s", name)
	return &s
}

func assertAdmitted(t *testing.T, s *spiceboxv1alpha1.AgentSession) {
	t.Helper()
	cond := meta.FindStatusCondition(s.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionAdmitted)
	if !assert.NotNil(t, cond, "%s: Admitted condition", s.Name) {
		return
	}
	assert.Equal(t, metav1.ConditionTrue, cond.Status, "%s: Admitted status", s.Name)
	assert.Equal(t, spiceboxv1alpha1.ReasonAdmitted, cond.Reason, "%s: Admitted reason", s.Name)
	assert.True(t, cond.LastTransitionTime.Time.Equal(passTime), "%s: Admitted transition at the pass's Now, got %v", s.Name, cond.LastTransitionTime)
	assert.Nil(t, s.Status.Admission, "%s: an admitted session holds no line position", s.Name)
}

func assertPosition(t *testing.T, s *spiceboxv1alpha1.AgentSession, index, length int32) {
	t.Helper()
	assert.False(t, admission.IsAdmitted(s), "%s: not admitted", s.Name)
	if !assert.NotNil(t, s.Status.Admission, "%s: status.admission", s.Name) {
		return
	}
	assert.Equal(t, index, s.Status.Admission.Position, "%s: position", s.Name)
	assert.Equal(t, length, s.Status.Admission.LineLength, "%s: line length", s.Name)
	assert.True(t, s.Status.Admission.JoinedAt.Time.Equal(s.CreationTimestamp.Time), "%s: joinedAt is creation, got %v", s.Name, s.Status.Admission.JoinedAt)
}

func TestAdmitterPass(t *testing.T) {
	t.Run("three waiting demo-agent sessions, room for one: oldest gets Admitted=True, others get Position 1 and 2 of 2", func(t *testing.T) {
		r, c, _ := newReconciler(t, true, roomFor(1),
			waitingSession("rb-c", 3*time.Minute),
			waitingSession("rb-a", time.Minute),
			waitingSession("rb-b", 2*time.Minute),
		)
		res := runPass(t, r)
		assert.Equal(t, 15*time.Second, res.RequeueAfter, "periodic requeue")

		assertAdmitted(t, getSession(t, c, "rb-a"))
		assertPosition(t, getSession(t, c, "rb-b"), 1, 2)
		assertPosition(t, getSession(t, c, "rb-c"), 2, 2)
	})

	t.Run("Enabled=false: every waiting session admitted, with no room at all", func(t *testing.T) {
		r, c, _ := newReconciler(t, false, roomFor(0),
			waitingSession("rb-a", time.Minute),
			waitingSession("rb-b", 2*time.Minute),
			waitingSession("rb-c", 3*time.Minute),
		)
		runPass(t, r)
		for _, name := range []string{"rb-a", "rb-b", "rb-c"} {
			assertAdmitted(t, getSession(t, c, name))
		}
	})

	t.Run("Pending without the Queued marker (parked at an earlier gate, or fatal settings): not admitted, no position", func(t *testing.T) {
		parked := waitingSession("rb-parked", 0)
		parked.Status.Conditions = nil
		r, c, _ := newReconciler(t, true, roomFor(5), parked, waitingSession("rb-a", time.Minute))
		runPass(t, r)

		got := getSession(t, c, "rb-parked")
		assert.Nil(t, meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionAdmitted),
			"a session that never reached the gate is never admitted")
		assert.Nil(t, got.Status.Admission, "and holds no line position")
		assertAdmitted(t, getSession(t, c, "rb-a"))
	})

	t.Run("a position write is skipped when unchanged: second pass sends no status patch", func(t *testing.T) {
		r, c, patches := newReconciler(t, true, roomFor(1),
			waitingSession("rb-a", time.Minute),
			waitingSession("rb-b", 2*time.Minute),
			waitingSession("rb-c", 3*time.Minute),
		)
		runPass(t, r)
		require.Positive(t, patches.Load(), "first pass writes admission and positions")
		patches.Store(0)

		runPass(t, r)
		assert.Zero(t, patches.Load(), "nothing changed, so nothing is written")
		assertPosition(t, getSession(t, c, "rb-b"), 1, 2)
	})

	t.Run("a holder still occupying the room: nobody is admitted, the line starts at 1", func(t *testing.T) {
		holder := waitingSession("rb-holder", 0)
		holder.Status.Phase = spiceboxv1alpha1.AgentSessionPhaseRunning
		holder.Status.Conditions = []metav1.Condition{{
			Type: spiceboxv1alpha1.AgentSessionConditionAdmitted, Status: metav1.ConditionTrue,
			Reason: spiceboxv1alpha1.ReasonAdmitted, LastTransitionTime: metav1.NewTime(baseTime),
		}}
		r, c, _ := newReconciler(t, true, roomFor(1), holder,
			waitingSession("rb-a", time.Minute),
			waitingSession("rb-b", 2*time.Minute),
		)
		runPass(t, r)
		assertPosition(t, getSession(t, c, "rb-a"), 1, 2)
		assertPosition(t, getSession(t, c, "rb-b"), 2, 2)
	})

	t.Run("a terminal session still Admitted=True: not a holder, so the head is admitted", func(t *testing.T) {
		done := waitingSession("rb-done", 0)
		done.Status.Phase = spiceboxv1alpha1.AgentSessionPhaseSucceeded
		done.Status.Conditions = []metav1.Condition{{
			Type: spiceboxv1alpha1.AgentSessionConditionAdmitted, Status: metav1.ConditionTrue,
			Reason: spiceboxv1alpha1.ReasonAdmitted, LastTransitionTime: metav1.NewTime(baseTime),
		}}
		r, c, _ := newReconciler(t, true, roomFor(1), done, waitingSession("rb-a", time.Minute))
		runPass(t, r)
		assertAdmitted(t, getSession(t, c, "rb-a"))
	})

	t.Run("a session parked at a pre-start gate: not waiting, never admitted or positioned", func(t *testing.T) {
		parked := waitingSession("rb-parked", 0)
		parked.Status.Phase = spiceboxv1alpha1.AgentSessionPhaseAwaitingDecision
		r, c, _ := newReconciler(t, true, roomFor(5), parked)
		runPass(t, r)
		got := getSession(t, c, "rb-parked")
		assert.False(t, admission.IsAdmitted(got), "parked session not admitted")
		assert.Nil(t, got.Status.Admission, "parked session has no line position")
	})

	t.Run("demand fails for the oldest: it keeps waiting unpositioned, the next is admitted", func(t *testing.T) {
		r, c, _ := newReconciler(t, true, roomFor(1),
			waitingSession("rb-a", time.Minute),
			waitingSession("rb-b", 2*time.Minute),
		)
		r.Demand = func(ctx context.Context, sess *spiceboxv1alpha1.AgentSession) (admission.Demand, error) {
			if sess.Name == "rb-a" {
				return admission.Demand{}, fmt.Errorf("settings not resolved")
			}
			return oneCPUDemand(ctx, sess)
		}
		runPass(t, r)
		a := getSession(t, c, "rb-a")
		assert.False(t, admission.IsAdmitted(a), "rb-a not admitted")
		assert.Nil(t, a.Status.Admission, "rb-a not positioned")
		assertAdmitted(t, getSession(t, c, "rb-b"))
	})

	t.Run("capacity snapshot fails: capacity unknown, every waiting session admitted as AdmittedUnknownCapacity", func(t *testing.T) {
		r, c, _ := newReconciler(t, true, failingSource{},
			waitingSession("rb-a", time.Minute),
			waitingSession("rb-b", 2*time.Minute),
		)
		runPass(t, r)
		assertAdmittedUnknown(t, getSession(t, c, "rb-a"))
		assertAdmittedUnknown(t, getSession(t, c, "rb-b"))
	})

	t.Run("namespace-scoped operator (watch set): capacity treated as unknown even with no room, admitted as AdmittedUnknownCapacity", func(t *testing.T) {
		r, c, _ := newReconciler(t, true, roomFor(0),
			waitingSession("rb-a", time.Minute),
		)
		r.Namespaces = []string{"default"}
		runPass(t, r)
		assertAdmittedUnknown(t, getSession(t, c, "rb-a"))
	})

	t.Run("known capacity: admitted with reason Admitted", func(t *testing.T) {
		r, c, _ := newReconciler(t, true, roomFor(1), waitingSession("rb-a", time.Minute))
		runPass(t, r)
		assertAdmitted(t, getSession(t, c, "rb-a"))
	})
}

// assertAdmittedUnknown asserts s was admitted under an unknown capacity
// model, which the reconciler's misfit release must leave alone.
func assertAdmittedUnknown(t *testing.T, s *spiceboxv1alpha1.AgentSession) {
	t.Helper()
	cond := meta.FindStatusCondition(s.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionAdmitted)
	if !assert.NotNil(t, cond, "%s: Admitted condition", s.Name) {
		return
	}
	assert.Equal(t, metav1.ConditionTrue, cond.Status, "%s: Admitted status", s.Name)
	assert.Equal(t, spiceboxv1alpha1.ReasonAdmittedUnknownCapacity, cond.Reason, "%s: Admitted reason", s.Name)
}

type failingSource struct{}

func (failingSource) Snapshot(context.Context) (admission.CapacityModel, error) {
	return nil, fmt.Errorf("nodes is forbidden")
}

// TestUnplacedDemand pins how much of a holder's pod set Decide must still
// reserve: a pod bound to a node is already in the snapshot; a pending or
// missing one is not.
func TestUnplacedDemand(t *testing.T) {
	d := admission.Demand{Pods: []admission.PodDemand{
		{Name: "bound", CPUMilli: 1}, {Name: "pending", CPUMilli: 2}, {Name: "missing", CPUMilli: 4},
		// A sandbox pod adopted from a SandboxWarmPool: bound under a
		// pool-minted name, found by its bundle SpiceboxSession's label.
		{Name: "s1-code-pod", SandboxSession: "s1-code", CPUMilli: 8},
	}}
	pods := newPodIndex([]corev1.Pod{
		{ObjectMeta: metav1.ObjectMeta{Name: "bound", Namespace: "default"}, Spec: corev1.PodSpec{NodeName: "node-a"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "pending", Namespace: "default"}},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "warmpool-x7k2p", Namespace: "default", Labels: map[string]string{sandboxPodLabel: "s1-code"}},
			Spec:       corev1.PodSpec{NodeName: "node-a"},
		},
	})
	cases := []struct {
		name  string
		pods  podIndex
		known bool
		want  []string
	}{
		{name: "pods listed: pending and missing pods are unplaced, the pool-named sandbox pod is placed", pods: pods, known: true, want: []string{"pending", "missing"}},
		{name: "pod list failed: every pod counted unplaced", known: false, want: []string{"bound", "pending", "missing", "s1-code-pod"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := unplaced("default", d, tc.pods, tc.known)
			var names []string
			for _, p := range got.Pods {
				names = append(names, p.Name)
			}
			assert.Equal(t, tc.want, names)
		})
	}
}

// TestAdmitterWatchNamespaces pins the --watch-namespaces contract: under
// namespaced RBAC a cluster-wide list 403s, so with a watch set the pass lists
// sessions one namespace at a time and never touches any other namespace.
func TestAdmitterWatchNamespaces(t *testing.T) {
	inNamespace := func(s *spiceboxv1alpha1.AgentSession, ns string) *spiceboxv1alpha1.AgentSession {
		s.Namespace = ns
		return s
	}
	cases := []struct {
		name         string
		namespaces   []string
		wantLists    []string
		wantAdmitted []string
	}{
		{
			name:       "watch set: one namespaced list per watched namespace, unwatched sessions untouched",
			namespaces: []string{"team-a", "team-b"},
			wantLists:  []string{"team-a", "team-b"}, wantAdmitted: []string{"team-a/rb-a", "team-b/rb-b"},
		},
		{
			name:      "no watch set: one cluster-wide list, every namespace admitted",
			wantLists: []string{""}, wantAdmitted: []string{"other/rb-c", "team-a/rb-a", "team-b/rb-b"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var lists []string
			c := fake.NewClientBuilder().
				WithScheme(testScheme(t)).
				WithObjects(
					inNamespace(waitingSession("rb-a", time.Minute), "team-a"),
					inNamespace(waitingSession("rb-b", 2*time.Minute), "team-b"),
					inNamespace(waitingSession("rb-c", 3*time.Minute), "other"),
				).
				WithStatusSubresource(&spiceboxv1alpha1.AgentSession{}).
				WithInterceptorFuncs(interceptor.Funcs{
					List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
						if _, ok := list.(*spiceboxv1alpha1.AgentSessionList); ok {
							lo := (&client.ListOptions{}).ApplyOptions(opts)
							lists = append(lists, lo.Namespace)
						}
						return cl.List(ctx, list, opts...)
					},
				}).
				Build()
			r := &Reconciler{
				Client: c, Capacity: roomFor(5), Enabled: true, Namespaces: tc.namespaces,
				Now: func() time.Time { return passTime }, Demand: oneCPUDemand,
			}
			runPass(t, r)
			assert.Equal(t, tc.wantLists, lists, "AgentSession list scopes")

			var all spiceboxv1alpha1.AgentSessionList
			require.NoError(t, c.List(context.Background(), &all), "list all")
			var admitted []string
			for i := range all.Items {
				if admission.IsAdmitted(&all.Items[i]) {
					admitted = append(admitted, all.Items[i].Namespace+"/"+all.Items[i].Name)
				}
			}
			assert.ElementsMatch(t, tc.wantAdmitted, admitted, "admitted sessions")
		})
	}
}

// warmIdleSession is an admitted, channel-attached Idle session that went idle
// idleFor before the pass and has not slept: its pods still hold capacity.
func warmIdleSession(name string, idleFor time.Duration) *spiceboxv1alpha1.AgentSession {
	s := waitingSession(name, 0)
	s.Spec.InputChannel = &spiceboxv1alpha1.ChannelBinding{Name: "chan-1", Kind: "fake", Key: name}
	s.Status.Phase = spiceboxv1alpha1.AgentSessionPhaseIdle
	idleAt := metav1.NewTime(passTime.Add(-idleFor))
	s.Status.LastIdleAt = &idleAt
	s.Status.Conditions = []metav1.Condition{{
		Type: spiceboxv1alpha1.AgentSessionConditionAdmitted, Status: metav1.ConditionTrue,
		Reason: spiceboxv1alpha1.ReasonAdmitted, LastTransitionTime: metav1.NewTime(baseTime),
	}}
	return s
}

// boundRunnerPod is the session's oneCPUDemand pod, bound to node-a and
// requesting what oneCPUDemand prices.
func boundRunnerPod(sessName string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: sessName + "-runner", Namespace: "default",
			Labels: map[string]string{sessionPodLabel: sessName},
		},
		Spec: corev1.PodSpec{
			NodeName: "node-a",
			Containers: []corev1.Container{{
				Name: "runner",
				Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("1"),
					corev1.ResourceMemory: resource.MustParse("1Mi"),
				}},
			}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

// cpuNode is node-a with cpus whole CPUs allocatable.
func cpuNode(cpus int64) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-a"},
		Status: corev1.NodeStatus{Allocatable: corev1.ResourceList{
			corev1.ResourceCPU:    *resource.NewQuantity(cpus, resource.DecimalSI),
			corev1.ResourceMemory: resource.MustParse("1Gi"),
			corev1.ResourcePods:   resource.MustParse("110"),
		}},
	}
}

// TestAdmitterEarlyReclaim covers the reclaim request a pass makes for a line
// whose head does not fit: it annotates the warm Idle sessions of that line
// PlanReclaim picks, priced from the live pods the cluster snapshot also sees.
func TestAdmitterEarlyReclaim(t *testing.T) {
	const minWarm = 5 * time.Minute
	cases := []struct {
		name          string
		objs          []client.Object
		wantAnnotated []string
		wantClean     []string
	}{
		{
			name: "line has a waiting head, one warm idle past minWarm: reclaim annotation set on it only",
			objs: []client.Object{
				cpuNode(2),
				warmIdleSession("rb-old", 10*time.Minute), boundRunnerPod("rb-old"),
				warmIdleSession("rb-young", time.Minute), boundRunnerPod("rb-young"),
				waitingSession("rb-wait", time.Minute),
			},
			wantAnnotated: []string{"rb-old"},
			wantClean:     []string{"rb-young", "rb-wait"},
		},
		{
			name: "no waiting head: no annotation",
			objs: []client.Object{
				cpuNode(1),
				warmIdleSession("rb-old", 10*time.Minute), boundRunnerPod("rb-old"),
			},
			wantClean: []string{"rb-old"},
		},
		{
			name: "the head fits without a reclaim: admitted, no annotation",
			objs: []client.Object{
				cpuNode(2),
				warmIdleSession("rb-old", 10*time.Minute), boundRunnerPod("rb-old"),
				waitingSession("rb-wait", time.Minute),
			},
			wantClean: []string{"rb-old", "rb-wait"},
		},
		{
			name: "only warm idle is younger than minWarm: no annotation",
			objs: []client.Object{
				cpuNode(1),
				warmIdleSession("rb-young", time.Minute), boundRunnerPod("rb-young"),
				waitingSession("rb-wait", time.Minute),
			},
			wantClean: []string{"rb-young", "rb-wait"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, c, _ := newReconciler(t, true, nil, tc.objs...)
			r.Capacity = admission.KubeCapacitySource{Reader: c}
			r.MinWarm = minWarm
			runPass(t, r)

			for _, name := range tc.wantAnnotated {
				got := getSession(t, c, name)
				assert.Equal(t, passTime.Format(time.RFC3339), got.Annotations[spiceboxv1alpha1.AnnotationReclaimRequested],
					"%s: reclaim requested at the pass's Now", name)
			}
			for _, name := range tc.wantClean {
				assert.NotContains(t, getSession(t, c, name).Annotations, spiceboxv1alpha1.AnnotationReclaimRequested,
					"%s: no reclaim requested", name)
			}
		})
	}

	t.Run("a reclaim already requested: the annotation is not rewritten", func(t *testing.T) {
		old := warmIdleSession("rb-old", 10*time.Minute)
		earlier := baseTime.Format(time.RFC3339)
		old.Annotations = map[string]string{spiceboxv1alpha1.AnnotationReclaimRequested: earlier}
		r, c, _ := newReconciler(t, true, nil, cpuNode(1), old, boundRunnerPod("rb-old"), waitingSession("rb-wait", time.Minute))
		r.Capacity = admission.KubeCapacitySource{Reader: c}
		r.MinWarm = minWarm
		runPass(t, r)
		assert.Equal(t, earlier, getSession(t, c, "rb-old").Annotations[spiceboxv1alpha1.AnnotationReclaimRequested],
			"the first request's time stands")
	})
}

// TestAdmitterReclaimInFlightIsCredited pins that one head gets one reclaim:
// between the request and the reclaimed pods actually leaving the node, every
// pass (each woken by the sleep's status write or a pod event) still sees the
// head not fit in the snapshot. Crediting the in-flight reclaim's bound pods
// keeps those passes from reclaiming the next-oldest warm session too.
func TestAdmitterReclaimInFlightIsCredited(t *testing.T) {
	const minWarm = 5 * time.Minute
	cases := []struct {
		name    string
		advance func(t *testing.T, c client.Client) // what happened to rb-1 between passes
	}{
		{
			name:    "reclaim requested but not yet slept: second pass annotates nothing new",
			advance: func(*testing.T, client.Client) {},
		},
		{
			name: "slept and Admitted=False/Reclaimed, pod still bound: second pass annotates nothing new",
			advance: func(t *testing.T, c client.Client) {
				markReclaimed(t, c, "rb-1")
			},
		},
		{
			name: "slept and Admitted=False/Reclaimed, pod terminating (DeletionTimestamp): second pass annotates nothing new",
			advance: func(t *testing.T, c client.Client) {
				markReclaimed(t, c, "rb-1")
				var pod corev1.Pod
				require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "rb-1-runner"}, &pod))
				require.NoError(t, c.Delete(context.Background(), &pod), "delete held by a finalizer: pod lingers terminating")
				require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(&pod), &pod))
				require.NotNil(t, pod.DeletionTimestamp, "pod is terminating")
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pod1 := boundRunnerPod("rb-1")
			pod1.Finalizers = []string{"test/hold"}
			r, c, _ := newReconciler(t, true, nil,
				cpuNode(3),
				warmIdleSession("rb-1", 30*time.Minute), pod1,
				warmIdleSession("rb-2", 20*time.Minute), boundRunnerPod("rb-2"),
				warmIdleSession("rb-3", 10*time.Minute), boundRunnerPod("rb-3"),
				waitingSession("rb-wait", time.Minute),
			)
			r.Capacity = admission.KubeCapacitySource{Reader: c}
			r.MinWarm = minWarm

			runPass(t, r)
			require.Contains(t, getSession(t, c, "rb-1").Annotations, spiceboxv1alpha1.AnnotationReclaimRequested, "pass 1 reclaims the oldest")
			require.NotContains(t, getSession(t, c, "rb-2").Annotations, spiceboxv1alpha1.AnnotationReclaimRequested, "pass 1 reclaims only one")

			tc.advance(t, c)
			runPass(t, r)
			for _, name := range []string{"rb-2", "rb-3"} {
				assert.NotContains(t, getSession(t, c, name).Annotations, spiceboxv1alpha1.AnnotationReclaimRequested,
					"%s: the in-flight reclaim already covers the head", name)
			}
		})
	}
}

// markReclaimed does what the sleep path's status write does: SleptAt and
// Admitted=False/Reclaimed. The reclaim annotation stays (it is removed only
// after the reap) and the pods are left for the test to dispose of.
func markReclaimed(t *testing.T, c client.Client, name string) {
	t.Helper()
	s := getSession(t, c, name)
	slept := metav1.NewTime(passTime)
	s.Status.SleptAt = &slept
	meta.SetStatusCondition(&s.Status.Conditions, metav1.Condition{
		Type: spiceboxv1alpha1.AgentSessionConditionAdmitted, Status: metav1.ConditionFalse,
		Reason: spiceboxv1alpha1.ReasonAdmissionReclaimed, LastTransitionTime: metav1.NewTime(passTime),
	})
	require.NoError(t, c.Status().Update(context.Background(), s), "mark %s reclaimed", name)
}

// cappedClass is testClass with spec.maxConcurrentSessions set.
func cappedClass(limit int32) *spiceboxv1alpha1.AgentClass {
	return &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: testClass, Namespace: "default"},
		Spec:       spiceboxv1alpha1.AgentClassSpec{MaxConcurrentSessions: &limit},
	}
}

// admittedRunning is a non-child session already holding its admission.
func admittedRunning(name string) *spiceboxv1alpha1.AgentSession {
	s := waitingSession(name, 0)
	s.Status.Phase = spiceboxv1alpha1.AgentSessionPhaseRunning
	s.Status.Conditions = []metav1.Condition{{
		Type: spiceboxv1alpha1.AgentSessionConditionAdmitted, Status: metav1.ConditionTrue,
		Reason: spiceboxv1alpha1.ReasonAdmitted, LastTransitionTime: metav1.NewTime(baseTime),
	}}
	return s
}

// childOf marks s as a session delegated by parent.
func childOf(s *spiceboxv1alpha1.AgentSession, parent string) *spiceboxv1alpha1.AgentSession {
	s.Spec.Parent = &spiceboxv1alpha1.NamespacedRef{Namespace: "default", Name: parent}
	return s
}

func TestAdmitterConcurrencyCap(t *testing.T) {
	t.Run("class cap 1 with one admitted parent: its waiting child is admitted; a waiting non-child is not", func(t *testing.T) {
		r, c, _ := newReconciler(t, true, roomFor(5),
			cappedClass(1),
			admittedRunning("rb-parent"),
			waitingSession("rb-other", time.Minute),
			childOf(waitingSession("rb-child", 2*time.Minute), "rb-parent"),
		)
		runPass(t, r)
		assertAdmitted(t, getSession(t, c, "rb-child"))
		assertPosition(t, getSession(t, c, "rb-other"), 1, 1)
	})

	t.Run("class cap 2 with one admitted parent: the oldest waiting non-child takes the second slot, the next waits", func(t *testing.T) {
		r, c, _ := newReconciler(t, true, roomFor(5),
			cappedClass(2),
			admittedRunning("rb-parent"),
			waitingSession("rb-a", time.Minute),
			waitingSession("rb-b", 2*time.Minute),
		)
		runPass(t, r)
		assertAdmitted(t, getSession(t, c, "rb-a"))
		assertPosition(t, getSession(t, c, "rb-b"), 1, 1)
	})

	t.Run("no AgentClass to read a cap from: the line is uncapped, every waiting session that fits is admitted", func(t *testing.T) {
		r, c, _ := newReconciler(t, true, roomFor(5),
			admittedRunning("rb-parent"),
			waitingSession("rb-a", time.Minute),
			waitingSession("rb-b", 2*time.Minute),
		)
		runPass(t, r)
		assertAdmitted(t, getSession(t, c, "rb-a"))
		assertAdmitted(t, getSession(t, c, "rb-b"))
	})

	t.Run("AgentClass read fails: the line's non-children keep waiting unpositioned, its child is still admitted", func(t *testing.T) {
		c := fake.NewClientBuilder().
			WithScheme(testScheme(t)).
			WithObjects(
				cappedClass(1),
				admittedRunning("rb-parent"),
				waitingSession("rb-other", time.Minute),
				childOf(waitingSession("rb-child", 2*time.Minute), "rb-parent"),
			).
			WithStatusSubresource(&spiceboxv1alpha1.AgentSession{}).
			WithInterceptorFuncs(interceptor.Funcs{
				Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					if _, ok := obj.(*spiceboxv1alpha1.AgentClass); ok {
						return fmt.Errorf("apiserver unavailable")
					}
					return cl.Get(ctx, key, obj, opts...)
				},
			}).
			Build()
		r := &Reconciler{
			Client: c, Capacity: roomFor(5), Enabled: true,
			Now: func() time.Time { return passTime }, Demand: oneCPUDemand,
		}
		runPass(t, r)
		assertAdmitted(t, getSession(t, c, "rb-child"))
		other := getSession(t, c, "rb-other")
		assert.False(t, admission.IsAdmitted(other), "rb-other not admitted past a cap it could not read")
		assert.Nil(t, other.Status.Admission, "rb-other not positioned")
	})
}

// preAdmissionSession is a session that started its pods before admission was
// switched on: in phase, with its runner pod recorded on status, and no
// Admitted condition at all.
func preAdmissionSession(name string, phase string) *spiceboxv1alpha1.AgentSession {
	s := waitingSession(name, 0)
	s.Status.Phase = phase
	s.Status.Conditions = nil
	s.Status.RunnerPodName = name + "-runner"
	return s
}

// TestAdmitterAdoptsPreAdmissionSessions pins the rollout: a session whose
// pods predate admission is adopted (Admitted=True/Adopted) and counted as a
// holder — toward its line's cap, and its unplaced pods reserved — rather
// than gated with its pods up or left uncounted.
func TestAdmitterAdoptsPreAdmissionSessions(t *testing.T) {
	assertAdopted := func(t *testing.T, s *spiceboxv1alpha1.AgentSession) {
		t.Helper()
		cond := meta.FindStatusCondition(s.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionAdmitted)
		if assert.NotNil(t, cond, "%s: Admitted condition", s.Name) {
			assert.Equal(t, metav1.ConditionTrue, cond.Status, "%s: adopted as admitted", s.Name)
			assert.Equal(t, spiceboxv1alpha1.ReasonAdmissionAdopted, cond.Reason, "%s: reason", s.Name)
		}
	}

	t.Run("Running pre-admission session with a bound pod, class cap 1: adopted, and the cap holds the waiting session back", func(t *testing.T) {
		r, c, _ := newReconciler(t, true, roomFor(5),
			cappedClass(1),
			preAdmissionSession("rb-old", spiceboxv1alpha1.AgentSessionPhaseRunning), boundRunnerPod("rb-old"),
			waitingSession("rb-a", time.Minute),
		)
		runPass(t, r)
		assertAdopted(t, getSession(t, c, "rb-old"))
		assertPosition(t, getSession(t, c, "rb-a"), 1, 1)
	})

	t.Run("Pending pre-admission session with an unbound pod, room for one: adopted, its unplaced pod reserved, the waiting session waits", func(t *testing.T) {
		pod := boundRunnerPod("rb-old")
		pod.Spec.NodeName = ""
		pod.Status.Phase = corev1.PodPending
		r, c, _ := newReconciler(t, true, roomFor(1),
			preAdmissionSession("rb-old", spiceboxv1alpha1.AgentSessionPhasePending), pod,
			waitingSession("rb-a", time.Minute),
		)
		runPass(t, r)
		assertAdopted(t, getSession(t, c, "rb-old"))
		assertPosition(t, getSession(t, c, "rb-a"), 1, 1)
	})

	t.Run("pre-admission session with nothing waiting: still adopted", func(t *testing.T) {
		r, c, _ := newReconciler(t, true, roomFor(1),
			preAdmissionSession("rb-old", spiceboxv1alpha1.AgentSessionPhaseIdle), boundRunnerPod("rb-old"),
		)
		runPass(t, r)
		assertAdopted(t, getSession(t, c, "rb-old"))
	})

	t.Run("pre-admission status but no pod listed: not adopted (nothing to hold)", func(t *testing.T) {
		r, c, _ := newReconciler(t, true, roomFor(1),
			preAdmissionSession("rb-old", spiceboxv1alpha1.AgentSessionPhaseRunning),
			waitingSession("rb-a", time.Minute),
		)
		runPass(t, r)
		assert.Nil(t, meta.FindStatusCondition(getSession(t, c, "rb-old").Status.Conditions, spiceboxv1alpha1.AgentSessionConditionAdmitted),
			"no pods: not a holder")
		assertAdmitted(t, getSession(t, c, "rb-a"))
	})

	t.Run("Enabled=false: nothing adopted", func(t *testing.T) {
		r, c, _ := newReconciler(t, false, roomFor(1),
			preAdmissionSession("rb-old", spiceboxv1alpha1.AgentSessionPhaseRunning), boundRunnerPod("rb-old"),
		)
		runPass(t, r)
		assert.Nil(t, meta.FindStatusCondition(getSession(t, c, "rb-old").Status.Conditions, spiceboxv1alpha1.AgentSessionConditionAdmitted))
	})
}

// TestAdmitterCapReclaim pins early reclaim for a line blocked by its cap
// rather than by capacity: warm idle holders count toward the cap, so the
// oldest one past minWarm is asked back, one per head, and not while an
// earlier request for the line is still in flight.
func TestAdmitterCapReclaim(t *testing.T) {
	const minWarm = 5 * time.Minute
	requested := func(s *spiceboxv1alpha1.AgentSession) *spiceboxv1alpha1.AgentSession {
		s.Annotations = map[string]string{spiceboxv1alpha1.AnnotationReclaimRequested: baseTime.Format(time.RFC3339)}
		return s
	}
	cases := []struct {
		name          string
		objs          []client.Object
		wantAnnotated []string
		wantClean     []string
	}{
		{
			name: "cap 2 held by two warm idle sessions, plenty of room: the oldest is reclaimed, only it",
			objs: []client.Object{
				cpuNode(8), cappedClass(2),
				warmIdleSession("rb-old", 30*time.Minute), boundRunnerPod("rb-old"),
				warmIdleSession("rb-mid", 20*time.Minute), boundRunnerPod("rb-mid"),
				waitingSession("rb-wait", time.Minute),
			},
			wantAnnotated: []string{"rb-old"},
			wantClean:     []string{"rb-mid", "rb-wait"},
		},
		{
			name: "a cap reclaim already in flight for the line: no second one",
			objs: []client.Object{
				cpuNode(8), cappedClass(2),
				requested(warmIdleSession("rb-old", 30*time.Minute)), boundRunnerPod("rb-old"),
				warmIdleSession("rb-mid", 20*time.Minute), boundRunnerPod("rb-mid"),
				waitingSession("rb-wait", time.Minute),
			},
			wantClean: []string{"rb-mid", "rb-wait"},
		},
		{
			name: "the cap is held by a running session, the warm one is too young: nothing reclaimed",
			objs: []client.Object{
				cpuNode(8), cappedClass(2),
				admittedRunning("rb-run"),
				warmIdleSession("rb-young", time.Minute), boundRunnerPod("rb-young"),
				waitingSession("rb-wait", time.Minute),
			},
			wantClean: []string{"rb-young", "rb-wait"},
		},
		{
			name: "under the cap and the head fits: admitted, nothing reclaimed",
			objs: []client.Object{
				cpuNode(8), cappedClass(3),
				warmIdleSession("rb-old", 30*time.Minute), boundRunnerPod("rb-old"),
				waitingSession("rb-wait", time.Minute),
			},
			wantClean: []string{"rb-old"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, c, _ := newReconciler(t, true, nil, tc.objs...)
			r.Capacity = admission.KubeCapacitySource{Reader: c}
			r.MinWarm = minWarm
			runPass(t, r)
			for _, name := range tc.wantAnnotated {
				assert.Equal(t, passTime.Format(time.RFC3339), getSession(t, c, name).Annotations[spiceboxv1alpha1.AnnotationReclaimRequested],
					"%s: reclaim requested at the pass's Now", name)
			}
			for _, name := range tc.wantClean {
				assert.NotContains(t, getSession(t, c, name).Annotations, spiceboxv1alpha1.AnnotationReclaimRequested,
					"%s: no reclaim requested", name)
			}
		})
	}
}

// A warm idle holder whose sandbox pod was adopted from a SandboxWarmPool has
// that pod under a pool-minted name. Matched by name, it was never counted as
// bound: its demand stayed "unplaced" (reserved on top of the snapshot, which
// already counts it — a double count for life), and an early reclaim could
// not credit what it gives back. Matched by its bundle SpiceboxSession's
// label, it is the bundle entry's pod.
func TestBoundPodsMatchesPoolNamedSandboxPod(t *testing.T) {
	d := admission.Demand{Pods: []admission.PodDemand{
		{Name: "rb-runner", CPUMilli: 1000},
		{Name: "rb-code-pod", SandboxSession: "rb-code", CPUMilli: 500},
	}}
	runner := boundRunnerPod("rb")
	sandbox := boundRunnerPod("ignored")
	sandbox.Name = "warmpool-x7k2p"
	sandbox.Labels = map[string]string{sandboxPodLabel: "rb-code"}
	pods := newPodIndex([]corev1.Pod{*runner, *sandbox})

	got := boundPods("default", d, pods)
	var names []string
	for _, p := range got {
		names = append(names, p.Name)
	}
	assert.ElementsMatch(t, []string{"rb-runner", "warmpool-x7k2p"}, names, "both the runner and the pool-named sandbox pod are bound")
	assert.Empty(t, unplaced("default", d, pods, true).Pods, "nothing left unplaced")
}

// A session that is no longer waiting must not keep showing a line position
// ("3rd in line") it no longer has: the pass clears a stale status.admission.
func TestAdmitterClearsStalePosition(t *testing.T) {
	stale := func(s *spiceboxv1alpha1.AgentSession) *spiceboxv1alpha1.AgentSession {
		s.Status.Admission = &spiceboxv1alpha1.AdmissionStatus{Position: 3, LineLength: 5, JoinedAt: s.CreationTimestamp}
		return s
	}
	failed := stale(waitingSession("rb-failed", 0))
	failed.Status.Phase = spiceboxv1alpha1.AgentSessionPhaseFailed
	parked := stale(waitingSession("rb-parked", 0))
	parked.Status.Phase = spiceboxv1alpha1.AgentSessionPhaseAwaitingCredentials

	r, c, _ := newReconciler(t, true, roomFor(1), failed, parked)
	runPass(t, r)
	assert.Nil(t, getSession(t, c, "rb-failed").Status.Admission, "a Failed session holds no line position")
	assert.Nil(t, getSession(t, c, "rb-parked").Status.Admission, "a parked session holds no line position")
}
