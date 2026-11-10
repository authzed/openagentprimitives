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

const testClass = "reviewbot"

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
// baseTime+offset.
func waitingSession(name string, offset time.Duration) *spiceboxv1alpha1.AgentSession {
	return &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "default",
			CreationTimestamp: metav1.NewTime(baseTime.Add(offset)),
		},
		Spec:   spiceboxv1alpha1.AgentSessionSpec{Class: testClass},
		Status: spiceboxv1alpha1.AgentSessionStatus{Phase: spiceboxv1alpha1.AgentSessionPhasePending},
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
	t.Run("three waiting reviewbot sessions, room for one: oldest gets Admitted=True, others get Position 1 and 2 of 2", func(t *testing.T) {
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

	t.Run("capacity snapshot fails: capacity unknown, every waiting session admitted in line order", func(t *testing.T) {
		r, c, _ := newReconciler(t, true, failingSource{},
			waitingSession("rb-a", time.Minute),
			waitingSession("rb-b", 2*time.Minute),
		)
		runPass(t, r)
		assertAdmitted(t, getSession(t, c, "rb-a"))
		assertAdmitted(t, getSession(t, c, "rb-b"))
	})
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
	}}
	pods := podIndex{
		{Namespace: "default", Name: "bound"}:   &corev1.Pod{Spec: corev1.PodSpec{NodeName: "node-a"}},
		{Namespace: "default", Name: "pending"}: &corev1.Pod{},
	}
	cases := []struct {
		name  string
		pods  podIndex
		known bool
		want  []string
	}{
		{name: "pods listed: pending and missing pods are unplaced", pods: pods, known: true, want: []string{"pending", "missing"}},
		{name: "pod list failed: every pod counted unplaced", known: false, want: []string{"bound", "pending", "missing"}},
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
		ObjectMeta: metav1.ObjectMeta{Name: sessName + "-runner", Namespace: "default"},
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
