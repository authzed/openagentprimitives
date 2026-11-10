//go:build e2e

package e2e

import (
	"context"
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
// sets, whatever their size. Every Reserve of a non-empty demand takes one
// set's room, so with the harness's pods never binding to a node (there is
// no scheduler), each admitted session holds one slot until it is released.
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

func (m *setModel) Reserve(d admission.Demand) bool {
	if len(d.Pods) == 0 {
		return true
	}
	if m.free == 0 {
		return false
	}
	m.free--
	return true
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
