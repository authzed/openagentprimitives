//go:build e2e

package e2e

import (
	"context"
	"strings"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/authzed/openagentprimitives/pkg/controllers/admission"
	"github.com/authzed/openagentprimitives/pkg/controllers/admission/admitter"
)

// AdmissionOptions turns on whole-set session admission for one harness: the
// AgentSession reconciler gates every booting session's pods on Admitted=True
// (AdmissionEnabled), and the admitter runs against Capacity.
//
// Opt-in per scenario, like WithTokenAuthz. Nil — every scenario but an
// admission one — leaves sessions ungated, which is the reconciler's zero
// value and what every other test was written against.
type AdmissionOptions struct {
	// Capacity is what the admitter places sets against. Required: envtest
	// has no nodes, so the production KubeCapacitySource would report unknown
	// capacity and admit every session at once — a line that never queues.
	Capacity admission.CapacitySource
}

// registerAdmitter adds the admitter to mgr when admission is on. Called for
// the first manager and again by RestartOperator, so a restarted operator
// resumes the line from AgentSession status as production's does.
func (h *Harness) registerAdmitter(t *testing.T, mgr manager.Manager) {
	t.Helper()
	if h.opts.Admission == nil {
		return
	}
	if err := (&admitter.Reconciler{
		Client:    mgr.GetClient(),
		APIReader: mgr.GetAPIReader(),
		Capacity:  h.opts.Admission.Capacity,
		Enabled:   true,
	}).SetupWithManager(mgr); err != nil {
		t.Fatalf("e2e: register admission controller: %v", err)
	}
}

// FixedSetCapacity is a cluster with room for exactly Sets whole session pod
// sets, whatever their size. A set's room is charged to its runner pod, the
// one pod every session's set has exactly one of, so with the harness's pods
// never binding to a node (there is no scheduler), each admitted session holds
// one slot until it is released.
//
// It stands in for nodes, not for the admission logic: the admitter still
// orders lines, applies fair share and reserves holders' unplaced demand
// through admission.Decide exactly as in production.
type FixedSetCapacity struct {
	Sets int
}

// Snapshot returns a fresh model with every set free; the admitter reserves
// current holders on it before deciding.
func (c FixedSetCapacity) Snapshot(context.Context) (admission.CapacityModel, error) {
	return &setModel{free: c.Sets, sets: c.Sets}, nil
}

// setModelSetCPUMilli and setModelSetMemBytes size one set for Totals, the
// denominator of a line's dominant share. Only the RATIO between lines'
// shares matters to Decide, so any positive size will do.
const (
	setModelSetCPUMilli = 4000
	setModelSetMemBytes = 8 << 30
)

type setModel struct {
	free, sets int
}

func (m *setModel) Known() bool { return true }

// Reserve charges one set per runner pod in d. Charging per call instead
// would be wrong: Decide reserves a holder's unplaced pods one at a time, so a
// three-pod holder would take three sets. A pod that is not a runner (sandbox,
// detector, sidecar) rides on its session's set and is free.
func (m *setModel) Reserve(d admission.Demand) bool {
	sets := 0
	for _, p := range d.Pods {
		if isRunnerPod(p) {
			sets++
		}
	}
	if sets > m.free {
		return false
	}
	m.free -= sets
	return true
}

// isRunnerPod matches the runner pod of a session's demand, named by
// agentsession.RunnerPodName.
func isRunnerPod(p admission.PodDemand) bool {
	return p.SandboxSession == "" && strings.HasSuffix(p.Name, "-runner")
}

// Free is a no-op: a model rebuilt every pass has no placed pods to return,
// and nothing in a scenario frees through the model mid-pass.
func (m *setModel) Free([]admission.PlacedPod) {}

func (m *setModel) Clone() admission.CapacityModel {
	c := *m
	return &c
}

func (m *setModel) Totals() (cpuMilli, memBytes int64) {
	return int64(m.sets) * setModelSetCPUMilli, int64(m.sets) * setModelSetMemBytes
}
